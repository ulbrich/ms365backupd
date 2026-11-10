package backup

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/status"
	"github.com/ulbrich/ms365backupd/internal/store"
)

// MailboxDir is the backup-relative directory of an additional mailbox.
func MailboxDir(name string) string { return joinRel(SvcMailboxes, name) }

// mailboxColl is the state collection prefix of an additional mailbox.
func mailboxColl(name string) string { return "mbx:" + strings.ToLower(name) + ":" }

// MailboxGraphClient returns a Graph client signed in as the additional mailbox m.
func MailboxGraphClient(ctx context.Context, cfg *config.Config, m *config.Mailbox, log *slog.Logger) (*graph.Client, error) {
	a := auth.ForMailbox(cfg, m)
	ts, err := a.TokenSource(ctx)
	if err != nil {
		return nil, err
	}
	g := graph.New(cfg.GraphURL, ts, log)
	g.LoginCmd = a.LoginCmd
	return g, nil
}

func (e *Engine) mailboxGraph(ctx context.Context, m *config.Mailbox) (*graph.Client, error) {
	if e.o.MailboxGraph != nil {
		return e.o.MailboxGraph(ctx, m)
	}
	return MailboxGraphClient(ctx, e.cfg, m, e.log)
}

// runFatal: errors that must stop the whole run even when they happen in an
// additional mailbox. Sign-in problems of an additional mailbox are not among them:
// they must not mark the main account as needing a new login.
func runFatal(err error) bool {
	var de *graph.DiskError
	return errors.As(err, &de) || errors.Is(err, context.Canceled) || errors.Is(err, errDiskFull)
}

// backupMailboxes backs up every active additional mailbox. A failing mailbox does
// not stop the others.
func (e *Engine) backupMailboxes(ctx context.Context) error {
	var failed []string
	for _, mb := range e.cfg.ActiveMailboxes() {
		dir := MailboxDir(mb.Name)
		if e.ex.Excluded(dir) {
			e.log.Info("mailbox excluded", "mailbox", mb.Name)
			continue
		}
		var err error
		switch mb.Type {
		case config.MailboxGraph:
			var g *graph.Client
			if g, err = e.mailboxGraph(ctx, &mb); err == nil {
				err = e.backupGraphMail(ctx, mailSource{g: g, dir: dir, coll: mailboxColl(mb.Name) + "mail:", svc: SvcMailboxes})
			}
		case config.MailboxIMAP:
			err = e.backupIMAP(ctx, &mb)
		default:
			err = fmt.Errorf("unknown mailbox type %q", mb.Type)
		}
		if err == nil {
			continue
		}
		if runFatal(err) || ctx.Err() != nil {
			return err
		}
		msg := err.Error()
		if graph.IsAuth(err) || errors.Is(err, auth.ErrNotLoggedIn) {
			msg = "sign-in needed, run `ms365backupd mailbox login " + mb.Name + "`: " + msg
		}
		e.log.Error("mailbox failed", "mailbox", mb.Name, "err", err)
		failed = append(failed, mb.Name+": "+msg)
	}
	if len(failed) > 0 {
		// Deliberately not wrapped: see runFatal.
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// ---- IMAP ----

// imapBatchBytes bounds the message data requested per FETCH (go-imap buffers
// literals in memory).
const imapBatchBytes = 8 << 20

var imapDialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// DialIMAP connects and logs in to the IMAP server of m.
func DialIMAP(m *config.Mailbox) (*imapclient.Client, error) {
	pw, err := m.Secret()
	if err != nil {
		return nil, fmt.Errorf("read password: %w", err)
	}
	addr := m.IMAPAddr()
	tc := &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12}
	var c *imapclient.Client
	switch m.IMAPTLS() {
	case "tls":
		c, err = imapclient.DialWithDialerTLS(imapDialer, addr, tc)
	default:
		c, err = imapclient.DialWithDialer(imapDialer, addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	c.ErrorLog = log.New(io.Discard, "", 0)
	c.Timeout = 5 * time.Minute
	if m.IMAPTLS() == "starttls" {
		if err := c.StartTLS(tc); err != nil {
			_ = c.Terminate()
			return nil, fmt.Errorf("starttls %s: %w", addr, err)
		}
	}
	if err := c.Login(m.Username, pw); err != nil {
		_ = c.Terminate()
		return nil, fmt.Errorf("login as %s: %w", m.Username, err)
	}
	return c, nil
}

// ListIMAPFolders returns the selectable folders.
func ListIMAPFolders(c *imapclient.Client) ([]*imap.MailboxInfo, error) {
	ch := make(chan *imap.MailboxInfo, 64)
	done := make(chan error, 1)
	go func() { done <- c.List("", "*", ch) }()
	var out []*imap.MailboxInfo
	for mi := range ch {
		sel := true
		for _, a := range mi.Attributes {
			if strings.EqualFold(a, imap.NoSelectAttr) || strings.EqualFold(a, "\\NonExistent") {
				sel = false
			}
		}
		if sel {
			out = append(out, mi)
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// imapFolderPath maps an IMAP folder name to a backup-relative path.
func imapFolderPath(dir string, mi *imap.MailboxInfo) string {
	parts := []string{mi.Name}
	if mi.Delimiter != "" {
		parts = strings.Split(mi.Name, mi.Delimiter)
	}
	out := []string{dir}
	for _, p := range parts {
		if p != "" {
			out = append(out, sanitizeName(p))
		}
	}
	return joinRel(out...)
}

func (e *Engine) backupIMAP(ctx context.Context, m *config.Mailbox) error {
	dir := MailboxDir(m.Name)
	e.setPhase(SvcMailboxes, "connecting "+m.Name)
	c, err := DialIMAP(m)
	if err != nil {
		return err
	}
	// go-imap does not take a context: drop the connection when the run is cancelled.
	stop := context.AfterFunc(ctx, func() { _ = c.Terminate() })
	defer func() {
		stop()
		_ = c.Logout()
	}()

	e.setPhase(SvcMailboxes, "listing folders of "+dir)
	folders, err := ListIMAPFolders(c)
	if err != nil {
		return err
	}
	prefix := mailboxColl(m.Name) + "imap:"
	live := map[string]bool{}
	var errs []string
	for _, mi := range folders {
		path := imapFolderPath(dir, mi)
		coll := prefix + mi.Name
		live[coll] = true
		if e.ex.Excluded(path) {
			e.log.Debug("mail folder excluded", "folder", path)
			continue
		}
		if err := e.syncIMAPFolder(ctx, c, mi.Name, path, coll); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if runFatal(err) {
				return err
			}
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			e.sample(fmt.Sprintf("mail folder %s: %v", path, err))
			if c.State() == imap.LogoutState {
				return fmt.Errorf("connection lost: %v", strings.Join(errs, "; "))
			}
		}
	}
	if err := e.retireFolders(prefix, live); err != nil {
		return err
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d folder(s) failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return nil
}

func (e *Engine) syncIMAPFolder(ctx context.Context, c *imapclient.Client, name, path, coll string) error {
	e.setPhase(SvcMailboxes, "listing "+path)
	mbox, err := c.Select(name, true)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}
	known, err := e.st.All(coll)
	if err != nil {
		return err
	}
	// UIDs are only meaningful together with UIDVALIDITY. If the server renumbered
	// the folder, start over (the old files go to .deleted with keep_deleted).
	uvKey := "uidvalidity:" + coll
	uv := strconv.FormatUint(uint64(mbox.UidValidity), 10)
	switch old := e.st.Meta(uvKey); {
	case old != "" && old != uv:
		e.log.Warn("IMAP UIDVALIDITY changed, full resync", "folder", path)
		for _, it := range known {
			if err := e.retire(it.Path); err != nil {
				return err
			}
		}
		if err := e.st.DropCollection(coll); err != nil {
			return err
		}
		known = map[string]store.Item{}
		e.markFullSync(path)
	case old == "" || e.o.FullResync:
		e.markFullSync(path)
	}
	if err := e.relocateFolder(coll, path); err != nil {
		return err
	}
	if err := e.st.SetMeta(uvKey, uv); err != nil {
		return err
	}

	var uids []uint32
	if mbox.Messages > 0 {
		crit := imap.NewSearchCriteria()
		all := new(imap.SeqSet)
		all.AddRange(1, 0) // 1:*
		crit.Uid = all
		if uids, err = c.UidSearch(crit); err != nil {
			return fmt.Errorf("search: %w", err)
		}
	}

	current := make(map[string]bool, len(uids))
	var todo []uint32
	for _, u := range uids {
		id := strconv.FormatUint(uint64(u), 10)
		current[id] = true
		if it, ok := known[id]; ok && (it.Skipped || it.Path != "" && exists(e.abs(it.Path))) {
			e.count(SvcMailboxes, func(c *status.Counters) { c.Unchanged++ })
			continue
		}
		todo = append(todo, u)
	}
	var removed []string
	for id, it := range known {
		if current[id] {
			continue
		}
		removed = append(removed, id)
		if err := e.retire(it.Path); err != nil {
			return err
		}
		e.count(SvcMailboxes, func(c *status.Counters) { c.Deleted++ })
	}
	if err := e.st.Delete(coll, removed...); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}

	e.setPhase(SvcMailboxes, "downloading "+path)
	e.addQueued(int64(len(todo)))
	defer func() { e.addQueued(-int64(len(todo))) }()

	// Sizes first, so that each FETCH of message data stays small.
	sizes := map[uint32]uint32{}
	for i := 0; i < len(todo); i += 500 {
		set := new(imap.SeqSet)
		set.AddNum(todo[i:min(i+500, len(todo))]...)
		if err := uidFetch(c, set, []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size}, func(msg *imap.Message) error {
			sizes[msg.Uid] = msg.Size
			return nil
		}); err != nil {
			return fmt.Errorf("fetch sizes: %w", err)
		}
	}

	section := &imap.BodySectionName{Peek: true}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, imap.FetchEnvelope, section.FetchItem()}
	for len(todo) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, total := 0, uint64(0)
		for n < len(todo) && (n == 0 || total+uint64(sizes[todo[n]]) <= imapBatchBytes) && n < 100 {
			total += uint64(sizes[todo[n]])
			n++
		}
		batch := todo[:n]
		todo = todo[n:]
		set := new(imap.SeqSet)
		set.AddNum(batch...)
		got := map[uint32]bool{}
		err := uidFetch(c, set, items, func(msg *imap.Message) error {
			got[msg.Uid] = true
			e.addQueued(-1)
			return e.saveIMAPMessage(coll, path, mbox.UidValidity, msg)
		})
		if err != nil {
			return err
		}
		for _, u := range batch {
			if !got[u] { // expunged meanwhile; picked up as removed next run
				e.addQueued(-1)
			}
		}
	}
	return nil
}

// uidFetch runs UID FETCH and calls fn for every message. Errors from fn stop the
// processing; the remaining responses are drained.
func uidFetch(c *imapclient.Client, set *imap.SeqSet, items []imap.FetchItem, fn func(*imap.Message) error) error {
	ch := make(chan *imap.Message, 8)
	done := make(chan error, 1)
	go func() { done <- c.UidFetch(set, items, ch) }()
	var first error
	for msg := range ch {
		if first == nil && msg.Uid != 0 {
			first = fn(msg)
		}
	}
	if err := <-done; err != nil && first == nil {
		first = err
	}
	return first
}

func (e *Engine) saveIMAPMessage(coll, folder string, uidValidity uint32, msg *imap.Message) error {
	id := strconv.FormatUint(uint64(msg.Uid), 10)
	var body imap.Literal
	for _, l := range msg.Body {
		body = l
	}
	if body == nil {
		e.log.Warn("item skipped", "service", SvcMailboxes, "item", folder+"/"+id, "err", "server returned no message data")
		e.count(SvcMailboxes, func(c *status.Counters) { c.Skipped++ })
		return e.st.Put(coll, id, store.Item{Skipped: true})
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m := mailMessage{ID: fmt.Sprintf("%d/%d", uidValidity, msg.Uid), Received: msg.InternalDate}
	if msg.Envelope != nil {
		m.Subject = msg.Envelope.Subject
		if m.Received.IsZero() {
			m.Received = msg.Envelope.Date
		}
	}
	rel := messageRel(folder, m)
	e.setCurrent(rel)
	if err := e.checkFree(); err != nil {
		return err
	}
	if err := e.writeFileAtomic(rel, data, m.Received); err != nil {
		return err
	}
	e.count(SvcMailboxes, func(c *status.Counters) { c.New++; c.Bytes += int64(len(data)) })
	return e.st.Put(coll, id, store.Item{Path: rel, ModTime: m.Received, Size: int64(len(data))})
}

// CheckMailbox connects to an additional mailbox and returns a short description
// of what it found (used by `mailbox test` and the TUI).
func CheckMailbox(ctx context.Context, cfg *config.Config, m *config.Mailbox, log *slog.Logger) (string, error) {
	switch m.Type {
	case config.MailboxIMAP:
		c, err := DialIMAP(m)
		if err != nil {
			return "", err
		}
		stop := context.AfterFunc(ctx, func() { _ = c.Terminate() })
		defer func() { stop(); _ = c.Logout() }()
		fs, err := ListIMAPFolders(c)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("signed in as %s, %d folder(s)", m.Username, len(fs)), nil
	case config.MailboxGraph:
		g, err := MailboxGraphClient(ctx, cfg, m, log)
		if err != nil {
			return "", err
		}
		g.MaxRetries = 2
		var me struct {
			UPN string `json:"userPrincipalName"`
		}
		if err := g.GetJSON(ctx, "me?$select=userPrincipalName", &me); err != nil {
			return "", err
		}
		var f struct {
			Count int `json:"totalItemCount"`
		}
		if err := g.GetJSON(ctx, "me/mailFolders/inbox?$select=totalItemCount", &f); err != nil {
			return "", err
		}
		return fmt.Sprintf("signed in as %s, %d message(s) in the inbox", strings.ToLower(me.UPN), f.Count), nil
	}
	return "", fmt.Errorf("unknown mailbox type %q", m.Type)
}
