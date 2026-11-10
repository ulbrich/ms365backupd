package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/daemon"
)

func dashboard(t *testing.T, cfg *config.Config) model {
	path := testEnv(t)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return model{cfg: cfg, opts: Options{ConfigPath: path, Reload: func() (*config.Config, error) { return config.Load(path, true) }},
		client: daemon.NewClient(t.TempDir()), width: 100}
}

func TestDashboardKeySOpensSetupAndEscReturns(t *testing.T) {
	m := dashboard(t, config.Default())
	nm, _ := m.Update(key("s"))
	m = nm.(model)
	if m.mode != modeSetup || m.setup == nil || m.setup.firstRun {
		t.Fatalf("s did not open setup (mode %v)", m.mode)
	}
	nm, _ = m.Update(key("esc"))
	if m = nm.(model); m.mode != modeDashboard {
		t.Fatal("esc without changes should return to the dashboard")
	}
}

func TestDashboardKeyLStartsLogin(t *testing.T) {
	m := dashboard(t, config.Default())
	nm, _ := m.Update(key("l"))
	if m = nm.(model); m.mode != modeDashboard || m.flash == "" {
		t.Fatal("l without client ID should explain instead of starting the login")
	}
	cfg := config.Default()
	cfg.TenantID = "contoso.onmicrosoft.com"
	cfg.ClientID = "11111111-2222-3333-4444-555555555555"
	m = dashboard(t, cfg)
	nm, cmd := m.Update(key("l"))
	if m = nm.(model); m.mode != modeLogin || cmd == nil {
		t.Fatal("l did not start the sign-in")
	}
	_ = m.View()
	nm, _ = m.Update(key("esc"))
	if m = nm.(model); m.mode != modeDashboard {
		t.Fatal("esc should cancel the sign-in")
	}
}

func TestSetupMailboxStep(t *testing.T) {
	cfg := config.Default()
	cfg.TenantID = "contoso.onmicrosoft.com"
	cfg.ClientID = "11111111-2222-3333-4444-555555555555"
	cfg.Target = "/Volumes/SSD"
	cfg.Mailboxes = []config.Mailbox{{Name: "old", Type: config.MailboxIMAP, Host: "imap.example.org", Username: "u", Password: "p"}}
	m := dashboard(t, cfg)
	send := func(keys ...string) {
		t.Helper()
		for _, k := range keys {
			nm, _ := m.Update(key(k))
			m = nm.(model)
		}
	}
	typ := func(s string) {
		for _, r := range s {
			nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			m = nm.(model)
		}
	}
	load := func() *config.Config {
		t.Helper()
		got, err := config.Load(m.opts.ConfigPath, true)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	send("a")
	if m.mode != modeSetup || m.setup.steps[m.setup.cur].title != mailboxesStep {
		t.Fatal("a did not open setup at the mailbox step")
	}
	e := m.setup.mailboxEditor()
	_ = m.View()
	send("a")
	if e.form == nil {
		t.Fatal("a did not open the add form")
	}
	typ("gmx")
	send("tab", "right") // type → imap
	send("tab")
	typ("imap.gmx.net")
	send("tab", "tab", "tab") // TLS, Port → Username
	typ("me@gmx.de")
	send("tab")
	typ("secret pw")
	_ = m.View()
	send("ctrl+s")
	if e.form != nil {
		t.Fatalf("form not submitted: %s", e.form.err)
	}
	if len(e.list) != 2 || len(load().Mailboxes) != 1 {
		t.Fatal("adding must only change the wizard, not the file")
	}
	send(" ") // disable gmx
	if !e.list[1].Disabled {
		t.Fatal("space did not disable")
	}
	send("l") // imap: explains instead of signing in
	if m.mode != modeSetup {
		t.Fatal("l on an imap mailbox left setup")
	}
	send("a")
	typ("gmx")
	send("ctrl+s")
	if e.form == nil || e.form.err == "" {
		t.Fatal("duplicate name accepted")
	}
	send("esc")
	if e.form != nil || m.mode != modeSetup {
		t.Fatal("esc did not close the form only")
	}
	send("up", "d", "n")
	if len(e.list) != 2 {
		t.Fatal("delete without confirmation")
	}
	send("d", "y") // remove "old"
	if len(e.list) != 1 || e.list[0].Name != "gmx" {
		t.Fatalf("delete failed: %+v", e.list)
	}
	_ = m.View()

	// Cancel discards everything.
	send("esc", "y")
	if m.mode != modeDashboard {
		t.Fatal("cancel did not return to the dashboard")
	}
	if got := load(); len(got.Mailboxes) != 1 || got.Mailboxes[0].Name != "old" {
		t.Fatalf("cancel changed the file: %+v", got.Mailboxes)
	}

	// Again, and save this time.
	send("a")
	e = m.setup.mailboxEditor()
	send("d", "y", "a")
	typ("work")
	send("ctrl+s") // graph mailbox with main tenant/client
	send("ctrl+s") // save setup
	if m.mode != modeDashboard {
		t.Fatalf("save did not finish setup: %s %s", m.setup.flash, m.setup.confirm)
	}
	got := load()
	if len(got.Mailboxes) != 1 || got.Mailboxes[0].Name != "work" || got.Mailboxes[0].Type != config.MailboxGraph {
		t.Fatalf("mailboxes not saved: %+v", got.Mailboxes)
	}
	if got.TenantID != cfg.TenantID || got.Target != cfg.Target {
		t.Fatal("other keys lost")
	}
	if len(m.cfg.Mailboxes) != 1 {
		t.Fatal("dashboard config not reloaded")
	}
}
