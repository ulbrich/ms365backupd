// Package daemon runs the long-lived backup service: it watches for the backup
// disk, runs the daily schedule with retries, sends notifications and weekly
// reports, and serves status/control requests on a local unix socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/backup"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/disk"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/history"
	"github.com/ulbrich/ms365backupd/internal/notify"
	"github.com/ulbrich/ms365backupd/internal/status"
)

// Trigger names.
const (
	TriggerSchedule      = "schedule"
	TriggerRetry         = "retry"
	TriggerDiskConnected = "disk-connected"
	TriggerManual        = "manual"
)

// persisted survives daemon restarts.
type persisted struct {
	LastWeeklySent  time.Time `json:"last_weekly_sent"`
	LastFailureMail time.Time `json:"last_failure_mail"`
}

// RunRequest asks for a backup.
type RunRequest struct {
	Trigger    string
	Only       []string
	FullResync bool
}

// Daemon is the service.
type Daemon struct {
	cfg     *config.Config
	cfgPath string
	version string
	log     *slog.Logger
	auth    *auth.Authenticator
	hist    *history.History
	sender  notify.Sender
	dir     string

	reqs   chan RunRequest
	done   chan *status.RunResult
	reload chan chan error
	// Reload re-reads the configuration (file + command line flags). Defaults to the file.
	Reload        func() (*config.Config, error)
	pendingReload bool
	ticker        *time.Ticker

	mu         sync.Mutex
	started    time.Time
	engine     *backup.Engine
	cancelRun  context.CancelFunc
	runTrigger string
	last       *status.RunResult
	disk       disk.Info
	nextRun    time.Time
	nextWhy    string
	retries    int
	nextReport time.Time
	authErr    string
	state      persisted
	reportBusy bool
}

// New creates a daemon.
func New(cfg *config.Config, cfgPath, version string, log *slog.Logger) *Daemon {
	d := &Daemon{
		cfg: cfg, cfgPath: cfgPath, version: version, log: log,
		auth:   auth.New(cfg),
		dir:    cfg.EffectiveStateDir(),
		reqs:   make(chan RunRequest, 4),
		done:   make(chan *status.RunResult, 1),
		reload: make(chan chan error, 1),
	}
	d.Reload = func() (*config.Config, error) { return config.Load(cfgPath, true) }
	d.hist = history.Open(d.dir)
	d.sender = notify.New(cfg, d.graphClient)
	return d
}

// SocketPath is the control socket location for a state dir.
func SocketPath(stateDir string) string { return filepath.Join(stateDir, "ctl.sock") }

func (d *Daemon) graphClient(ctx context.Context) (*graph.Client, error) {
	ts, err := d.auth.TokenSource(ctx)
	if err != nil {
		return nil, err
	}
	return graph.New(d.cfg.GraphURL, ts, d.log), nil
}

func (d *Daemon) loadState() {
	b, err := os.ReadFile(filepath.Join(d.dir, "daemon.json"))
	if err == nil {
		_ = json.Unmarshal(b, &d.state)
	}
}

func (d *Daemon) saveState() {
	b, _ := json.MarshalIndent(d.state, "", "  ")
	p := filepath.Join(d.dir, "daemon.json")
	if err := os.WriteFile(p+".tmp", b, 0o600); err == nil {
		_ = os.Rename(p+".tmp", p)
	}
}

// Run blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	unlock, err := Lock(d.dir, "daemon.lock")
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return errors.New("another ms365backupd daemon is already running")
		}
		return err
	}
	defer unlock()
	d.started = time.Now()
	d.loadState()

	if all, _ := d.hist.Last(1); len(all) > 0 {
		d.last = &all[0]
	}
	h, m, _ := config.ParseClock(d.cfg.Schedule)
	d.nextRun, d.nextWhy = NextDaily(time.Now(), h, m), TriggerSchedule
	d.planReport(time.Now())

	srv, err := d.serve(SocketPath(d.dir))
	if err != nil {
		return err
	}
	defer srv.Close()

	d.log.Info("daemon started", "version", d.version, "target", d.cfg.TargetPath(),
		"schedule", d.cfg.Schedule, "next_run", d.nextRun.Format(time.RFC3339), "state_dir", d.dir)
	if acc := d.auth.Account(); acc == "" {
		if _, err := d.auth.TokenSource(ctx); err != nil {
			d.authErr = err.Error()
			d.log.Warn("not logged in yet; run `ms365backupd login`")
		}
	}

	tick := time.NewTicker(d.cfg.PollInterval.Duration)
	defer tick.Stop()
	d.ticker = tick
	prevPresent, prevID := false, ""
	d.tick(ctx, &prevPresent, &prevID)
	for {
		select {
		case <-ctx.Done():
			d.mu.Lock()
			if d.cancelRun != nil {
				d.log.Info("shutting down: cancelling running backup")
				d.cancelRun()
			}
			running := d.engine != nil
			d.mu.Unlock()
			if running {
				select {
				case res := <-d.done:
					d.finish(context.Background(), res)
				case <-time.After(60 * time.Second):
					d.log.Warn("backup did not stop within 60s")
				}
			}
			return nil
		case req := <-d.reqs:
			d.start(ctx, req)
		case res := <-d.done:
			d.finish(ctx, res)
			if d.pendingReload {
				_ = d.applyReload()
			}
		case reply := <-d.reload:
			reply <- d.applyReload()
		case <-tick.C:
			if d.pendingReload {
				_ = d.applyReload()
			}
			d.tick(ctx, &prevPresent, &prevID)
		}
	}
}

func (d *Daemon) tick(ctx context.Context, prevPresent *bool, prevID *string) {
	info := disk.Probe(d.cfg.TargetPath(), d.cfg.RequireMount)
	d.mu.Lock()
	d.disk = info
	running := d.engine != nil
	next, why := d.nextRun, d.nextWhy
	d.mu.Unlock()

	connected := info.Present && (!*prevPresent || info.DiskID != *prevID)
	if info.Present != *prevPresent {
		if info.Present {
			d.log.Info("backup disk connected", "path", info.Path, "disk", info.DiskID, "free", notify.HumanBytes(info.Free))
		} else {
			d.log.Info("backup disk not available", "path", info.Path, "reason", info.Reason)
		}
	}
	*prevPresent, *prevID = info.Present, info.DiskID

	if connected && !running {
		// New or reconnected disk: back up now unless it was backed up recently.
		var last *status.RunResult
		if info.DiskID != "" {
			last = d.hist.LastSuccess(info.DiskID)
		}
		if last == nil || time.Since(last.Finished) > 20*time.Hour {
			d.start(ctx, RunRequest{Trigger: TriggerDiskConnected})
			return
		}
	}
	if !running && !time.Now().Before(next) {
		d.start(ctx, RunRequest{Trigger: why})
	}
	d.mu.Lock()
	dueReport := !d.nextReport.IsZero() && !time.Now().Before(d.nextReport) && !d.reportBusy
	d.mu.Unlock()
	if dueReport {
		go d.sendWeekly(context.WithoutCancel(ctx), false)
	}
}

func (d *Daemon) start(ctx context.Context, req RunRequest) {
	d.mu.Lock()
	if d.engine != nil {
		d.mu.Unlock()
		d.log.Info("backup already running; request ignored", "trigger", req.Trigger)
		return
	}
	g, err := d.graphClient(ctx)
	if err != nil {
		d.authErr = err.Error()
		d.mu.Unlock()
		now := time.Now()
		res := &status.RunResult{ID: now.Format("20060102-150405") + "-auth", Trigger: req.Trigger, Started: now, Finished: now,
			Status: status.StatusFailed, Error: err.Error(), NeedsLogin: true}
		go func() { d.done <- res }()
		return
	}
	d.authErr = ""
	eng, err := backup.New(backup.Options{
		Config: d.cfg, Graph: g, StateDir: d.dir, Log: d.log, Trigger: req.Trigger,
		Only: req.Only, FullResync: req.FullResync,
	})
	if err != nil {
		d.mu.Unlock()
		d.log.Error("cannot start backup", "err", err)
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	d.engine, d.cancelRun, d.runTrigger = eng, cancel, req.Trigger
	d.mu.Unlock()
	d.log.Info("backup started", "trigger", req.Trigger)
	go func() {
		defer cancel()
		// Run lock also guards against a concurrent one-shot `ms365backupd backup`.
		unlock, err := Lock(d.dir, "run.lock")
		if err != nil {
			now := time.Now()
			d.done <- &status.RunResult{ID: now.Format("20060102-150405") + "-locked", Trigger: req.Trigger, Started: now, Finished: now,
				Status: status.StatusFailed, Error: "another backup is running: " + err.Error()}
			return
		}
		defer unlock()
		d.done <- eng.Run(rctx)
	}()
}

func isAutomatic(trigger string) bool { return trigger != TriggerManual }

func (d *Daemon) finish(ctx context.Context, res *status.RunResult) {
	if err := d.hist.Append(res); err != nil {
		d.log.Error("cannot write history", "err", err)
	}
	d.mu.Lock()
	d.engine, d.cancelRun = nil, nil
	d.last = res
	if res.NeedsLogin {
		d.authErr = res.Error
	}
	h, m, _ := config.ParseClock(d.cfg.Schedule)
	daily := NextDaily(time.Now(), h, m)
	notifyNow := false
	attempts := d.retries + 1
	switch {
	case res.OK():
		d.retries = 0
		if res.Trigger == TriggerSchedule || res.Trigger == TriggerRetry {
			d.nextRun, d.nextWhy = daily, TriggerSchedule
		}
	case !isAutomatic(res.Trigger):
		// Manual runs: the user is watching; don't retry or mail.
	case res.NeedsLogin || res.DiskMissing || d.retries >= d.cfg.RetryAttempts:
		// Missing disk / login: retrying won't help. (Connecting the disk starts a run anyway.)
		notifyNow = true
		d.retries = 0
		d.nextRun, d.nextWhy = daily, TriggerSchedule
	default:
		d.retries++
		retryAt := time.Now().Add(d.cfg.RetryDelay.Duration * time.Duration(d.retries))
		if retryAt.Before(daily) {
			d.nextRun, d.nextWhy = retryAt, TriggerRetry
		}
		d.log.Warn("backup failed; will retry", "attempt", d.retries, "of", d.cfg.RetryAttempts, "at", retryAt.Format("15:04"))
	}
	d.mu.Unlock()
	if notifyNow {
		d.notifyFailure(ctx, res, attempts)
	}
}

func (d *Daemon) notifyFailure(ctx context.Context, res *status.RunResult, attempts int) {
	if d.sender == nil {
		return
	}
	now := time.Now()
	d.mu.Lock()
	sentToday := d.state.LastFailureMail.Format("2006-01-02") == now.Format("2006-01-02")
	d.mu.Unlock()
	if sentToday {
		d.log.Info("failure mail already sent today; not sending again")
		return
	}
	var lastOK *time.Time
	if r := d.hist.LastSuccess(""); r != nil {
		t := r.Finished
		lastOK = &t
	}
	subj, body := notify.FailureMessage(res, attempts, lastOK)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	if err := d.sender.Send(sctx, subj, body); err != nil {
		d.log.Error("cannot send failure mail", "err", err)
		return
	}
	d.log.Info("failure mail sent", "to", strings.Join(d.cfg.Email.To, ","))
	d.mu.Lock()
	d.state.LastFailureMail = now
	d.saveState()
	d.mu.Unlock()
}

func (d *Daemon) planReport(now time.Time) {
	if !d.cfg.WeeklyEnabled() {
		d.nextReport = time.Time{}
		return
	}
	wd, _ := config.ParseWeekday(d.cfg.Email.WeeklyReportDay)
	h, m, _ := config.ParseClock(d.cfg.Email.WeeklyReportTime)
	prev := PrevWeekly(now, wd, h, m)
	if !d.state.LastWeeklySent.IsZero() && d.state.LastWeeklySent.Before(prev) {
		d.nextReport = now.Add(2 * time.Minute) // missed while down: catch up soon
		return
	}
	d.nextReport = NextWeekly(now, wd, h, m)
	if d.state.LastWeeklySent.IsZero() {
		d.state.LastWeeklySent = now // first start: begin counting from now
		d.saveState()
	}
}

// sendWeekly builds and sends the weekly report. force=true is a manual request.
func (d *Daemon) sendWeekly(ctx context.Context, force bool) error {
	d.mu.Lock()
	if d.reportBusy {
		d.mu.Unlock()
		return errors.New("report already in progress")
	}
	d.reportBusy = true
	info := d.disk
	d.mu.Unlock()
	err := d.doSendWeekly(ctx, info, force)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reportBusy = false
	switch {
	case err == nil:
		d.state.LastWeeklySent = time.Now()
		d.saveState()
		d.planReport(time.Now())
	case !force:
		d.nextReport = time.Now().Add(time.Hour) // retry later
	}
	return err
}

func (d *Daemon) doSendWeekly(ctx context.Context, info disk.Info, force bool) error {
	if d.sender == nil {
		if force {
			return errors.New("email is not configured")
		}
		return nil
	}
	to := time.Now()
	from := to.AddDate(0, 0, -7)
	runs, _ := d.hist.Since(from)
	size, files := DataSize(info)
	subj, body := notify.WeeklyMessage(runs, from, to, toDiskState(info), size, files)
	sctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := d.sender.Send(sctx, subj, body); err != nil {
		d.log.Error("cannot send weekly report", "err", err)
		return err
	}
	d.log.Info("weekly report sent", "to", strings.Join(d.cfg.Email.To, ","))
	return nil
}

// DataSize walks the backup data on the disk (excluding .deleted).
func DataSize(info disk.Info) (uint64, int64) {
	if !info.Present {
		return 0, 0
	}
	root := filepath.Join(info.Path, disk.DataDir)
	var size uint64
	var n int64
	_ = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() && (de.Name() == ".deleted" || de.Name() == disk.MetaDir) {
			return filepath.SkipDir
		}
		if de.Type().IsRegular() {
			if fi, err := de.Info(); err == nil {
				size += uint64(fi.Size())
				n++
			}
		}
		return nil
	})
	return size, n
}

func toDiskState(i disk.Info) status.DiskState {
	return status.DiskState{Path: i.Path, Present: i.Present, Reason: i.Reason, DiskID: i.DiskID, Account: i.Account, Free: i.Free, Total: i.Total}
}

// Status returns the current daemon status.
func (d *Daemon) Status() status.Daemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := status.Daemon{
		PID: os.Getpid(), Version: d.version, Started: d.started, Account: d.auth.Account(),
		Last: d.last, NextRun: d.nextRun, NextRunWhy: d.nextWhy, Disk: toDiskState(d.disk),
		AuthError: d.authErr, ConfigPath: d.cfgPath, Target: d.cfg.TargetPath(),
	}
	if !d.nextReport.IsZero() {
		t := d.nextReport
		s.NextReport = &t
	}
	if d.engine != nil {
		p := d.engine.Progress()
		s.Running = &p
	}
	if r := d.hist.LastSuccess(""); r != nil {
		t := r.Finished
		s.LastSuccess = &t
	}
	return s
}

// ErrReloadDeferred means the new configuration is valid and will be applied as soon
// as the running backup (or report) has finished.
var ErrReloadDeferred = errors.New("configuration will be applied when the running backup finishes")

// RequestReload asks the daemon loop to re-read its configuration and waits for the result.
func (d *Daemon) RequestReload(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case d.reload <- reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// applyReload runs on the daemon loop goroutine. The new configuration is swapped in
// only while no backup or report is running, so no goroutine sees a half-changed setup.
func (d *Daemon) applyReload() error {
	cfg, err := d.Reload()
	if err == nil {
		err = cfg.Validate(true)
	}
	if err != nil {
		d.pendingReload = false
		d.log.Error("configuration reload failed; keeping the current configuration", "err", err)
		return err
	}
	if cfg.EffectiveStateDir() != d.dir {
		d.pendingReload = false
		err := errors.New("state_dir changed: restart the daemon to apply")
		d.log.Error("configuration reload", "err", err)
		return err
	}
	d.mu.Lock()
	if d.engine != nil || d.reportBusy {
		d.pendingReload = true
		d.mu.Unlock()
		d.log.Info("configuration reload deferred until the running backup finishes")
		return ErrReloadDeferred
	}
	d.pendingReload = false
	d.cfg = cfg
	d.auth = auth.New(cfg)
	d.sender = notify.New(cfg, d.graphClient)
	if d.nextWhy == TriggerSchedule {
		h, m, _ := config.ParseClock(cfg.Schedule)
		d.nextRun = NextDaily(time.Now(), h, m)
	}
	d.planReport(time.Now())
	d.recheckAuth()
	d.mu.Unlock()
	if d.ticker != nil {
		d.ticker.Reset(cfg.PollInterval.Duration)
	}
	d.log.Info("configuration reloaded", "target", cfg.TargetPath(), "schedule", cfg.Schedule, "next_run", d.nextRun.Format(time.RFC3339))
	return nil
}

// recheckAuth updates the login problem shown in the status. A failed run's
// "needs login" stays until the token has been stored again (new sign-in or a
// successful refresh) after that failure. Called with d.mu held.
func (d *Daemon) recheckAuth() {
	if _, err := d.auth.TokenSource(context.Background()); err != nil {
		d.authErr = err.Error()
		return
	}
	if d.last == nil || !d.last.NeedsLogin || d.auth.SavedAt().After(d.last.Finished) {
		d.authErr = ""
	}
}

// Request queues a backup (non-blocking).
func (d *Daemon) Request(r RunRequest) bool {
	select {
	case d.reqs <- r:
		return true
	default:
		return false
	}
}

// Cancel stops the running backup.
func (d *Daemon) Cancel() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancelRun != nil {
		d.cancelRun()
		return true
	}
	return false
}
