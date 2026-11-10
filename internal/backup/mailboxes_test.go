package backup

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
	"golang.org/x/oauth2"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/status"
)

func startIMAP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	s := server.New(memory.New())
	s.AllowInsecureAuth = true
	s.ErrorLog = nopLogger{}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { _ = s.Close() })
	return ln.Addr().String()
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}
func (nopLogger) Println(...interface{})        {}

func imapDo(t *testing.T, addr string, fn func(c *imapclient.Client)) {
	t.Helper()
	c, err := imapclient.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if err := c.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	fn(c)
}

func appendMsg(t *testing.T, c *imapclient.Client, folder, subj string) {
	t.Helper()
	msg := "From: a@example.com\r\nSubject: " + subj + "\r\nDate: Tue, 4 Mar 2025 10:00:00 +0000\r\n\r\nbody " + subj + "\r\n"
	if err := c.Append(folder, nil, time.Date(2025, 3, 4, 10, 0, 0, 0, time.UTC), bytes.NewBufferString(msg)); err != nil {
		t.Fatal(err)
	}
}

func emls(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, ".eml") {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func runMailboxes(t *testing.T, fg *fakeGraph, stateDir, target string, mbs []config.Mailbox) *status.RunResult {
	t.Helper()
	cfg := config.Default()
	cfg.Target = target
	cfg.RequireMount = false
	cfg.MinFreeMB = 0
	cfg.Services = config.Services{}
	cfg.SharePointSites = []string{}
	cfg.Mailboxes = mbs
	cfg.Exclude = []string{"mailboxes/imap1/Skipped"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	newG := func() *graph.Client {
		g := graph.New(fg.srv.URL+"/v1.0", oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"}), log)
		g.MaxRetries = 1
		return g
	}
	e, err := New(Options{Config: cfg, Graph: newG(), StateDir: stateDir, Log: log, Trigger: "test",
		MailboxGraph: func(context.Context, *config.Mailbox) (*graph.Client, error) { return newG(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	return e.Run(context.Background())
}

func TestMailboxesIMAPAndGraph(t *testing.T) {
	addr := startIMAP(t)
	host, port, _ := net.SplitHostPort(addr)
	var p int
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	fg := newFakeGraph(t)
	fg.putMsg("g1", "Graph mail")
	stateDir, target := t.TempDir(), t.TempDir()
	mbs := []config.Mailbox{
		{Name: "imap1", Type: config.MailboxIMAP, Host: host, Port: p, Username: "username", Password: "password", TLS: "none"},
		{Name: "other", Type: config.MailboxGraph},
		{Name: "off", Type: config.MailboxGraph, Disabled: true},
	}
	imapDo(t, addr, func(c *imapclient.Client) {
		if err := c.Create("Archive"); err != nil {
			t.Fatal(err)
		}
		if err := c.Create("Skipped"); err != nil {
			t.Fatal(err)
		}
		appendMsg(t, c, "Archive", "Archived one")
		appendMsg(t, c, "Skipped", "Not backed up")
	})

	res := runMailboxes(t, fg, stateDir, target, mbs)
	if res.Status != status.StatusSuccess {
		t.Fatalf("run 1: %s %v", res.Error, res.ErrorSamples)
	}
	root := filepath.Join(target, "ms365backupd", "mailboxes")
	got := emls(t, filepath.Join(root, "imap1"))
	// memory backend starts with one message in INBOX.
	if len(got) != 2 {
		t.Fatalf("imap files after run 1: %v", got)
	}
	var archived string
	for p, b := range got {
		if strings.HasPrefix(p, "Archive/2025/") && strings.Contains(b, "body Archived one") {
			archived = p
		}
		if strings.HasPrefix(p, "Skipped/") {
			t.Fatalf("excluded folder backed up: %s", p)
		}
	}
	if archived == "" || !strings.Contains(archived, "Archived one") {
		t.Fatalf("archived message missing or badly named: %v", got)
	}
	if g := emls(t, filepath.Join(root, "other")); len(g) != 1 {
		t.Fatalf("graph mailbox files: %v", g)
	}
	if _, err := os.Stat(filepath.Join(root, "off")); err == nil {
		t.Fatal("disabled mailbox backed up")
	}
	if c := res.Services[SvcMailboxes]; c.New != 3 {
		t.Fatalf("counters run 1: %+v", c)
	}

	// Unchanged run: nothing new.
	res = runMailboxes(t, fg, stateDir, target, mbs)
	if c := res.Services[SvcMailboxes]; res.Status != status.StatusSuccess || c.New != 0 {
		t.Fatalf("run 2: %s %+v", res.Error, c)
	}

	// New message + deleted message.
	imapDo(t, addr, func(c *imapclient.Client) {
		appendMsg(t, c, "Archive", "Archived two")
		if _, err := c.Select("INBOX", false); err != nil {
			t.Fatal(err)
		}
		set := new(imap.SeqSet)
		set.AddNum(1)
		if err := c.Store(set, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Expunge(nil); err != nil {
			t.Fatal(err)
		}
	})
	res = runMailboxes(t, fg, stateDir, target, mbs)
	if c := res.Services[SvcMailboxes]; res.Status != status.StatusSuccess || c.New != 1 || c.Deleted != 1 {
		t.Fatalf("run 3: %s %+v", res.Error, c)
	}
	got = emls(t, filepath.Join(root, "imap1"))
	if len(got) != 2 {
		t.Fatalf("imap files after run 3: %v", got)
	}
	for p := range got {
		if !strings.HasPrefix(p, "Archive/") {
			t.Fatalf("unexpected file %s", p)
		}
	}
}

func TestMailboxFailureIsNotFatal(t *testing.T) {
	fg := newFakeGraph(t)
	stateDir, target := t.TempDir(), t.TempDir()
	mbs := []config.Mailbox{
		{Name: "broken", Type: config.MailboxIMAP, Host: "127.0.0.1", Port: 1, Username: "u", Password: "p", TLS: "none"},
	}
	res := runMailboxes(t, fg, stateDir, target, mbs)
	if res.Status == status.StatusSuccess || res.NeedsLogin {
		t.Fatalf("want failed/partial without login flag, got %s needsLogin=%v", res.Status, res.NeedsLogin)
	}
	if !strings.Contains(res.Error+strings.Join(res.ErrorSamples, " "), "broken") {
		t.Fatalf("error does not name the mailbox: %s %v", res.Error, res.ErrorSamples)
	}
}
