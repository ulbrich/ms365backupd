// Package backup implements the incremental Microsoft 365 → disk backup.
//
// Layout on the disk (nothing outside <target>/ms365backupd is ever touched):
//
//	<target>/ms365backupd/
//	  .ms365backupd/disk.json         disk identity marker
//	  onedrive/...                    OneDrive files, original folder structure
//	  sharepoint/<site>/<library>/... SharePoint document libraries (optional)
//	  mail/<folder>/<year>/*.eml      one RFC 822 file per message
//	  contacts/<folder>/*.json|.vcf
//	  calendar/<calendar>/*.json
//	  .deleted/<date>/...             items deleted in Microsoft 365 (keep_deleted)
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/disk"
	"github.com/ulbrich/ms365backupd/internal/exclude"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/status"
	"github.com/ulbrich/ms365backupd/internal/store"
)

// Service names (also the top-level directories on disk).
const (
	SvcOneDrive   = "onedrive"
	SvcSharePoint = "sharepoint"
	SvcMail       = "mail"
	SvcContacts   = "contacts"
	SvcCalendar   = "calendar"
)

// AllServices in execution order.
var AllServices = []string{SvcOneDrive, SvcSharePoint, SvcMail, SvcContacts, SvcCalendar}

// Options configure one run.
type Options struct {
	Config   *config.Config
	Graph    *graph.Client
	StateDir string
	Log      *slog.Logger
	Trigger  string
	// Only restricts the run to these services (nil = all enabled).
	Only []string
	// FullResync ignores stored delta tokens (re-verifies everything; existing files are not re-downloaded).
	FullResync bool
}

// Engine runs a backup. Create with New, then call Run once.
type Engine struct {
	o       Options
	cfg     *config.Config
	g       *graph.Client
	log     *slog.Logger
	ex      *exclude.Matcher
	st      *store.Store
	root    string // <target>/ms365backupd
	today   string
	exclKey string

	mu       sync.Mutex
	prog     status.Progress
	counters map[string]*status.Counters
	samples  []string
	fullSync []string
}

// New prepares an engine.
func New(o Options) (*Engine, error) {
	ex, err := exclude.New(o.Config.Exclude)
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Engine{
		o: o, cfg: o.Config, g: o.Graph, log: o.Log, ex: ex,
		counters: map[string]*status.Counters{},
		exclKey:  strings.Join(ex.Patterns(), "\x00"),
	}, nil
}

// Progress returns a snapshot of the live progress.
func (e *Engine) Progress() status.Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.prog
	p.Services = map[string]status.Counters{}
	for k, c := range e.counters {
		p.Services[k] = *c
		p.Totals.Add(*c)
	}
	return p
}

func (e *Engine) setPhase(svc, phase string) {
	e.mu.Lock()
	e.prog.Service, e.prog.Phase, e.prog.Current = svc, phase, ""
	e.mu.Unlock()
	e.log.Info("phase", "service", svc, "phase", phase)
}

func (e *Engine) setCurrent(s string) {
	e.mu.Lock()
	e.prog.Current = s
	e.mu.Unlock()
}

func (e *Engine) addQueued(n int64) {
	e.mu.Lock()
	e.prog.Queued += n
	e.mu.Unlock()
}

// count updates service counters under the lock.
func (e *Engine) count(svc string, f func(c *status.Counters)) {
	e.mu.Lock()
	c, ok := e.counters[svc]
	if !ok {
		c = &status.Counters{}
		e.counters[svc] = c
	}
	f(c)
	e.mu.Unlock()
}

func (e *Engine) sample(msg string) {
	e.mu.Lock()
	if len(e.samples) < 25 {
		e.samples = append(e.samples, msg)
	}
	e.mu.Unlock()
}

func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// isFatal: errors that make continuing pointless for the whole run.
func isFatal(err error) bool {
	var de *graph.DiskError
	return errors.As(err, &de) || graph.IsAuth(err) || errors.Is(err, context.Canceled) || errors.Is(err, errDiskFull)
}

var errDiskFull = errors.New("backup disk is full (below min_free_mb)")

// Run executes the backup and always returns a result (never nil).
func (e *Engine) Run(ctx context.Context) *status.RunResult {
	res := &status.RunResult{ID: newRunID(), Trigger: e.o.Trigger, Started: time.Now()}
	e.mu.Lock()
	e.prog = status.Progress{RunID: res.ID, Trigger: e.o.Trigger, Started: res.Started, Phase: "starting"}
	e.mu.Unlock()
	e.today = res.Started.Format("2006-01-02")

	err := e.run(ctx, res)
	res.Finished = time.Now()
	res.Services = map[string]status.Counters{}
	e.mu.Lock()
	for k, c := range e.counters {
		res.Services[k] = *c
		res.Totals.Add(*c)
	}
	res.ErrorSamples = append(res.ErrorSamples, e.samples...)
	res.FullSync = e.fullSync
	e.mu.Unlock()
	if e.st != nil {
		_ = e.st.Close()
	}
	if t := e.cfg.TargetPath(); t != "" {
		res.DiskFree, res.DiskTotal, _ = disk.Space(t)
	}
	switch {
	case err != nil:
		res.Status = status.StatusFailed
		res.Error = err.Error()
		res.NeedsLogin = graph.IsAuth(err)
	case res.Totals.Errors > 0:
		res.Status = status.StatusPartial
		res.Error = fmt.Sprintf("%d item(s) failed and will be retried on the next run", res.Totals.Errors)
	default:
		res.Status = status.StatusSuccess
	}
	e.log.Info("backup finished", "status", res.Status, "duration", res.Duration().Round(time.Second),
		"new", res.Totals.New, "updated", res.Totals.Updated, "deleted", res.Totals.Deleted,
		"errors", res.Totals.Errors, "bytes", res.Totals.Bytes, "error", res.Error)
	return res
}

func (e *Engine) enabled(svc string) bool {
	if len(e.o.Only) > 0 {
		found := false
		for _, s := range e.o.Only {
			if s == svc {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	switch svc {
	case SvcOneDrive:
		return e.cfg.Services.OneDrive
	case SvcSharePoint:
		return len(e.cfg.SharePointSites) > 0
	case SvcMail:
		return e.cfg.Services.Mail
	case SvcContacts:
		return e.cfg.Services.Contacts
	case SvcCalendar:
		return e.cfg.Services.Calendar
	}
	return false
}

func (e *Engine) run(ctx context.Context, res *status.RunResult) error {
	e.setPhase("", "checking disk")
	target := e.cfg.TargetPath()
	info := disk.Probe(target, e.cfg.RequireMount)
	if !info.Present {
		res.DiskMissing = true
		return fmt.Errorf("backup disk not available at %s: %s", target, info.Reason)
	}
	if err := e.checkFree(); err != nil {
		return err
	}

	e.setPhase("", "checking account")
	var me struct {
		UPN  string `json:"userPrincipalName"`
		Mail string `json:"mail"`
	}
	if err := e.g.GetJSON(ctx, "me?$select=userPrincipalName,mail", &me); err != nil {
		return fmt.Errorf("cannot reach Microsoft 365: %w", err)
	}
	account := strings.ToLower(me.UPN)
	if account == "" {
		account = strings.ToLower(me.Mail)
	}
	res.Account = account

	marker, created, err := disk.EnsureMarker(target, account)
	if err != nil {
		return err
	}
	if marker.Account != "" && account != "" && marker.Account != account {
		return fmt.Errorf("disk %s belongs to account %s, not %s; use another disk or remove %s",
			marker.DiskID, marker.Account, account, filepath.Join(target, disk.DataDir))
	}
	if marker.Account == "" && account != "" {
		_ = disk.UpdateMarkerAccount(target, marker, account)
	}
	res.DiskID, res.NewDisk = marker.DiskID, created
	e.root = filepath.Join(target, disk.DataDir)
	if err := disk.CheckWritable(e.root); err != nil {
		return fmt.Errorf("backup disk is not writable: %w", err)
	}

	st, err := store.Open(e.o.StateDir, marker.DiskID)
	if err != nil {
		return err
	}
	e.st = st
	if st.Meta("created") == "" {
		_ = st.SetMeta("created", time.Now().UTC().Format(time.RFC3339))
		e.log.Info("new backup disk: starting full backup", "disk", marker.DiskID)
	}

	type step struct {
		svc string
		fn  func(context.Context) error
	}
	steps := []step{
		{SvcOneDrive, e.backupOneDrive},
		{SvcSharePoint, e.backupSharePoint},
		{SvcMail, e.backupMail},
		{SvcContacts, e.backupContacts},
		{SvcCalendar, e.backupCalendar},
	}
	var failed []string
	for _, s := range steps {
		if !e.enabled(s.svc) {
			continue
		}
		if e.ex.Excluded(s.svc) {
			e.log.Info("service excluded", "service", s.svc)
			continue
		}
		e.count(s.svc, func(*status.Counters) {})
		if err := s.fn(ctx); err != nil {
			if isFatal(err) || ctx.Err() != nil {
				return fmt.Errorf("%s: %w", s.svc, err)
			}
			e.log.Error("service failed", "service", s.svc, "err", err)
			e.sample(fmt.Sprintf("%s: %v", s.svc, err))
			failed = append(failed, s.svc)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("service(s) failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (e *Engine) checkFree() error {
	free, _, err := disk.Space(e.cfg.TargetPath())
	if err != nil {
		return &graph.DiskError{Err: err}
	}
	if e.cfg.MinFreeMB > 0 && free < uint64(e.cfg.MinFreeMB)<<20 {
		return errDiskFull
	}
	return nil
}

// itemError classifies a per-item failure. It returns a non-nil error only if
// the error is fatal for the run; otherwise it records it and returns nil.
// The returned bool is true when the item should be retried next run (service dirty).
func (e *Engine) itemError(svc, what string, err error) (retry bool, fatal error) {
	if isFatal(err) {
		return true, err
	}
	if graph.IsPermanentItemError(err) {
		e.log.Warn("item skipped", "service", svc, "item", what, "err", err)
		e.count(svc, func(c *status.Counters) { c.Skipped++ })
		return false, nil
	}
	e.log.Error("item failed", "service", svc, "item", what, "err", err)
	e.count(svc, func(c *status.Counters) { c.Errors++ })
	e.sample(fmt.Sprintf("%s: %s: %v", svc, what, err))
	return true, nil
}

// forEach runs fn over items with bounded concurrency. It stops at the first
// fatal error (returned by fn) and returns it.
func forEach[T any](ctx context.Context, n int, items []T, fn func(context.Context, T) error) error {
	if len(items) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan T)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				if ctx.Err() != nil {
					continue
				}
				if err := fn(ctx, it); err != nil {
					once.Do(func() { first = err; cancel() })
				}
			}
		}()
	}
feed:
	for _, it := range items {
		select {
		case ch <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// sortByDepth sorts slash paths shallow-first.
func sortByDepth(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
}

func (e *Engine) markFullSync(coll string) {
	e.mu.Lock()
	e.fullSync = append(e.fullSync, coll)
	e.mu.Unlock()
}

// excludeChanged reports (and records) whether the exclude list changed since
// coll was last synced. A change forces a full resync so that newly
// un-excluded items get picked up.
func (e *Engine) excludeChanged(coll string) bool {
	k := "exclude:" + coll
	old := e.st.Meta(k)
	if old == e.exclKey {
		return false
	}
	_ = e.st.SetMeta(k, e.exclKey)
	return old != "" || e.exclKey != ""
}

func ensureDir(p string) error {
	if err := os.MkdirAll(p, 0o755); err != nil {
		return &graph.DiskError{Err: err}
	}
	return nil
}
