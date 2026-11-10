package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMergesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(p, []byte(`{"tenant_id":"contoso.onmicrosoft.com","client_id":"abc","target":"/mnt/ssd","exclude":["onedrive/Videos"],"retry_delay":"5m","email":{"to":["me@example.com"],"via":"graph"}}`), 0o600)
	c, err := Load(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule != "02:30" || !c.Services.Mail || c.RetryDelay.Duration != 5*time.Minute {
		t.Errorf("defaults not merged: %+v", c)
	}
	if err := c.Validate(true); err != nil {
		t.Error(err)
	}
}

func TestUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(p, []byte(`{"targett":"/mnt"}`), 0o600)
	if _, err := Load(p, true); err == nil || !strings.Contains(err.Error(), "targett") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/x.json", false); err != nil {
		t.Error("missing optional file should give defaults")
	}
	if _, err := Load("/nonexistent/x.json", true); err == nil {
		t.Error("missing explicit file should fail")
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Schedule = "25:00"
	c.Email.To = []string{"x@example.com"}
	c.Email.Via = "smtp"
	err := c.Validate(true)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, s := range []string{"tenant_id", "client_id", "target", "schedule", "smtp_host"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("missing %q in %v", s, err)
		}
	}
}

func TestRedacted(t *testing.T) {
	c := Default()
	c.Email.Password = "secret"
	if strings.Contains(c.Redacted().Email.Password, "secret") || c.Email.Password != "secret" {
		t.Error("redaction wrong")
	}
}

func TestSharePointSitesDefault(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]string{
		`{}`:                        {AllSites},
		`{"sharepoint_sites":null}`: {AllSites},
		`{"sharepoint_sites":[]}`:   {},
		`{"sharepoint_sites":["a.com:/sites/x"]}`: {"a.com:/sites/x"},
	}
	for in, want := range cases {
		p := filepath.Join(dir, "c.json")
		_ = os.WriteFile(p, []byte(in), 0o600)
		c, err := Load(p, true)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(c.SharePointSites, ",") != strings.Join(want, ",") || c.SharePointSites == nil {
			t.Errorf("%s: got %#v, want %#v", in, c.SharePointSites, want)
		}
		// An explicit empty list must survive a save/load round trip.
		if err := c.Save(p); err != nil {
			t.Fatal(err)
		}
		c2, _ := Load(p, true)
		if strings.Join(c2.SharePointSites, ",") != strings.Join(want, ",") {
			t.Errorf("%s: round trip got %#v", in, c2.SharePointSites)
		}
	}
}

func TestMailboxes(t *testing.T) {
	c := Default()
	if err := c.AddMailbox(Mailbox{Name: "a b", Type: MailboxGraph}); err == nil {
		t.Fatal("invalid name accepted")
	}
	if err := c.AddMailbox(Mailbox{Name: "imap", Type: MailboxIMAP, Host: "h"}); err == nil {
		t.Fatal("imap without username/password accepted")
	}
	if err := c.AddMailbox(Mailbox{Name: "Work", Type: MailboxGraph}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddMailbox(Mailbox{Name: "work", Type: MailboxGraph}); err == nil {
		t.Fatal("duplicate name (case-insensitive) accepted")
	}
	m := Mailbox{Name: "gmx", Type: MailboxIMAP, Host: "imap.gmx.net", Username: "u", Password: "pw"}
	if err := c.AddMailbox(m); err != nil {
		t.Fatal(err)
	}
	if m.IMAPAddr() != "imap.gmx.net:993" {
		t.Fatal(m.IMAPAddr())
	}
	m.TLS = "starttls"
	if m.IMAPAddr() != "imap.gmx.net:143" {
		t.Fatal(m.IMAPAddr())
	}
	c.TenantID = "t"
	if c.MailboxTenant(&c.Mailboxes[0]) != "t" {
		t.Fatal("tenant default")
	}

	p := filepath.Join(t.TempDir(), "config.json")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Edit(p, func(c *Config) error { return c.SetMailboxDisabled("WORK", true) })
	if err != nil || len(got.Mailboxes) != 2 || !got.Mailboxes[0].Disabled || len(got.ActiveMailboxes()) != 1 {
		t.Fatalf("edit: %v %+v", err, got)
	}
	if got.Redacted().Mailboxes[1].Password == "pw" || got.Mailboxes[1].Password != "pw" {
		t.Fatal("redaction")
	}
	if _, err := Edit(p, func(c *Config) error { _, err := c.RemoveMailbox("nope"); return err }); err == nil {
		t.Fatal("removing unknown mailbox succeeded")
	}
	got, _ = Edit(p, func(c *Config) error { _, err := c.RemoveMailbox("work"); return err })
	b, _ := os.ReadFile(p)
	if len(got.Mailboxes) != 1 || !strings.Contains(string(b), `"mailboxes": [`) {
		t.Fatalf("remove: %+v", got.Mailboxes)
	}
	got, _ = Edit(p, func(c *Config) error { _, err := c.RemoveMailbox("gmx"); return err })
	b, _ = os.ReadFile(p)
	if len(got.Mailboxes) != 0 || !strings.Contains(string(b), `"mailboxes": []`) {
		t.Fatalf("empty list must be written as []: %s", b)
	}
}
