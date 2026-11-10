package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ulbrich/ms365backupd/internal/config"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func typeText(s *setup, text string) {
	for _, r := range text {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func testEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	return filepath.Join(dir, "ms365backupd", "config.json")
}

func TestSetupFirstRunSave(t *testing.T) {
	path := testEnv(t)
	s := newSetup(path, config.Default(), true, 100)
	if s.dirty() {
		t.Fatal("fresh setup must not be dirty")
	}
	typeText(s, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	s.Update(key("tab")) // tenant ID -> client ID
	typeText(s, "11111111-2222-3333-4444-555555555555")
	s.Update(key("pgdown")) // disk step
	typeText(s, "/mnt/backup")
	s.Update(key("pgdown")) // data step
	s.Update(key("space"))  // OneDrive off
	for i := 0; i < 5; i++ {
		s.Update(key("tab"))
	}
	typeText(s, "onedrive/Videos")
	s.Update(key("enter"))
	typeText(s, "**/*.iso")
	_ = s.View() // must not panic on any step
	s.Update(key("ctrl+s"))
	if !s.done || !s.saved {
		t.Fatalf("not saved: confirm=%q flash=%q", s.confirm, s.flash)
	}
	got, err := config.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" || got.ClientID != "11111111-2222-3333-4444-555555555555" || got.Target != "/mnt/backup" {
		t.Fatalf("got client %q target %q", got.ClientID, got.Target)
	}
	if got.Services.OneDrive {
		t.Fatal("OneDrive toggle not applied")
	}
	if strings.Join(got.Exclude, "|") != "onedrive/Videos|**/*.iso" {
		t.Fatalf("exclude = %q", got.Exclude)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestSetupCancelWritesNothing(t *testing.T) {
	path := testEnv(t)
	s := newSetup(path, config.Default(), true, 100)
	s.Update(key("tab"))
	typeText(s, "abc")
	s.Update(key("esc"))
	if s.done || s.confirm != "discard" {
		t.Fatal("expected discard confirmation")
	}
	s.Update(key("y"))
	if !s.done || s.saved {
		t.Fatal("expected cancelled")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config file must not exist after cancel")
	}
}

func TestSetupIncompleteAsksBeforeSaving(t *testing.T) {
	path := testEnv(t)
	s := newSetup(path, config.Default(), true, 100)
	s.Update(key("ctrl+s"))
	if s.confirm != "invalid" || s.done {
		t.Fatalf("expected confirmation, got confirm=%q done=%v", s.confirm, s.done)
	}
	_ = s.View()
	s.Update(key("n"))
	if s.done {
		t.Fatal("must not save on n")
	}
	s.Update(key("ctrl+s"))
	s.Update(key("y"))
	if !s.saved {
		t.Fatal("expected save anyway")
	}
}

func TestSetupInvalidFieldBlocksSave(t *testing.T) {
	path := testEnv(t)
	s := newSetup(path, config.Default(), true, 100)
	s.Update(key("tab"))
	typeText(s, "not-a-guid")
	s.Update(key("ctrl+s"))
	if s.done || s.flash == "" {
		t.Fatal("invalid client ID must block saving")
	}
	if !strings.Contains(s.View(), "expected a GUID") {
		t.Fatal("review page should list the invalid field")
	}
}

// Existing settings are shown, unchanged ones stay untouched and keys not in
// the form survive a save.
func TestSetupKeepsExistingConfig(t *testing.T) {
	path := testEnv(t)
	base := config.Default()
	base.TenantID = "contoso.onmicrosoft.com"
	base.ClientID = "11111111-2222-3333-4444-555555555555"
	base.Target = "/Volumes/SSD"
	base.PollInterval = config.Duration{Duration: 45 * time.Second}
	base.GraphURL = "http://example.invalid"
	base.Email.To = []string{"a@example.com", "b@example.com"}
	base.Email.SMTPHost = "smtp.example.com"
	base.Email.From = "backup@example.com"
	base.Mailboxes = []config.Mailbox{{Name: "gmx", Type: config.MailboxIMAP, Host: "imap.gmx.net", Username: "u", Password: "p"}}
	if err := base.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, _ := config.Load(path, true)
	s := newSetup(path, loaded, false, 100)
	if s.dirty() {
		for _, st := range s.steps {
			for _, f := range st.fields {
				if f.value() != f.orig {
					t.Errorf("%s: %q != %q", f.label, f.value(), f.orig)
				}
			}
		}
		t.Fatal("unchanged config must not be dirty")
	}
	if s.value("Recipients") != "a@example.com, b@example.com" {
		t.Fatalf("recipients shown as %q", s.value("Recipients"))
	}
	s.Update(key("pgdown"))
	s.Update(key("tab"))
	s.Update(key("space")) // require_mount off
	if !strings.Contains(s.View(), "changed") {
		t.Fatal("changed marker missing")
	}
	s.Update(key("ctrl+s"))
	if !s.saved {
		t.Fatalf("not saved: %q %q", s.confirm, s.flash)
	}
	got, _ := config.Load(path, true)
	if got.RequireMount || got.PollInterval.Duration != 45*time.Second || got.GraphURL != "http://example.invalid" || len(got.Email.To) != 2 ||
		len(got.Mailboxes) != 1 || got.Mailboxes[0].Password != "p" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestSetupSMTPFieldsOnlyForSMTP(t *testing.T) {
	path := testEnv(t)
	s := newSetup(path, config.Default(), true, 100)
	s.gotoStep("Email")
	n := len(s.visible())
	via := s.value("Send via")
	s.Update(key("tab"))   // -> Send via
	s.Update(key("right")) // toggle smtp/graph
	if s.value("Send via") == via {
		t.Fatal("choice not changed")
	}
	if len(s.visible()) == n {
		t.Fatal("SMTP fields should appear/disappear with Send via")
	}
}

func TestSetupTenantIDRequiredAndValidated(t *testing.T) {
	path := testEnv(t)
	s := newSetup(path, config.Default(), true, 100)
	if !strings.Contains(s.View(), "Tenant ID") || !strings.Contains(s.View(), "required") {
		t.Fatal("tenant ID should be shown as required")
	}
	typeText(s, "common")
	s.Update(key("ctrl+s"))
	if s.done || !strings.Contains(s.View(), "Directory (tenant) ID") {
		t.Fatal("\"common\" must be rejected")
	}
}

func TestSetupSharePointDefaultsToAll(t *testing.T) {
	s := newSetup(testEnv(t), config.Default(), true, 100)
	if s.value("SharePoint sites") != config.AllSites {
		t.Fatalf("SharePoint sites = %q", s.value("SharePoint sites"))
	}
}
