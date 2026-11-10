package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Mailbox types.
const (
	MailboxGraph = "graph" // Microsoft 365 mailbox with its own sign-in (device code flow)
	MailboxIMAP  = "imap"  // any IMAP server, username + password
)

// Mailbox is an additional mailbox, backed up on top of the account's own mail to
// <target>/ms365backupd/mailboxes/<name>/. It does not depend on services.mail.
type Mailbox struct {
	// Name identifies the mailbox: directory name on the disk, key of its sync state
	// and (graph) of its token file. Letters, digits, '.', '_' and '-'.
	Name string `json:"name"`
	// Type is "graph" or "imap".
	Type string `json:"type"`
	// Disabled keeps the entry (and its sign-in) but skips it in backups.
	Disabled bool `json:"disabled,omitempty"`

	// Graph: the app registration used for this mailbox's sign-in. Empty means the
	// main tenant_id/client_id (the app must then allow accounts of that tenant).
	TenantID string `json:"tenant_id,omitempty"`
	ClientID string `json:"client_id,omitempty"`

	// IMAP.
	Host         string `json:"host,omitempty"`
	Port         int    `json:"port,omitempty"` // 0 = default for TLS (993/143)
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	PasswordFile string `json:"password_file,omitempty"`
	// TLS is "tls" (implicit, default, port 993), "starttls" (port 143) or "none".
	TLS string `json:"tls,omitempty"`
}

// MailboxScopes are the delegated permissions requested for an additional Graph mailbox.
var MailboxScopes = []string{"offline_access", "User.Read", "Mail.Read"}

// ValidMailboxName reports whether n can be used as a mailbox name.
func ValidMailboxName(n string) error {
	if n == "" {
		return errors.New("name is required")
	}
	if len(n) > 64 {
		return errors.New("name is longer than 64 characters")
	}
	if strings.HasPrefix(n, ".") {
		return errors.New("name must not start with '.'")
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return fmt.Errorf("name may only contain letters, digits, '.', '_' and '-' (got %q)", r)
		}
	}
	return nil
}

// Validate checks a single mailbox entry.
func (m *Mailbox) Validate() error {
	var errs []string
	if err := ValidMailboxName(m.Name); err != nil {
		errs = append(errs, err.Error())
	}
	switch m.Type {
	case MailboxGraph:
	case MailboxIMAP:
		if m.Host == "" {
			errs = append(errs, "host is required for imap")
		}
		if m.Username == "" {
			errs = append(errs, "username is required for imap")
		}
		if m.Password == "" && m.PasswordFile == "" {
			errs = append(errs, "password or password_file is required for imap")
		}
		if m.Port < 0 || m.Port > 65535 {
			errs = append(errs, "port must be 0–65535")
		}
		switch strings.ToLower(m.TLS) {
		case "", "tls", "starttls", "none":
		default:
			errs = append(errs, "tls must be tls|starttls|none")
		}
	case "":
		errs = append(errs, "type is required (graph or imap)")
	default:
		errs = append(errs, fmt.Sprintf("unknown type %q (graph or imap)", m.Type))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// IMAPTLS returns the normalised TLS mode ("tls" by default).
func (m *Mailbox) IMAPTLS() string {
	if m.TLS == "" {
		return "tls"
	}
	return strings.ToLower(m.TLS)
}

// IMAPAddr returns host:port, using the default port for the TLS mode.
func (m *Mailbox) IMAPAddr() string {
	port := m.Port
	if port == 0 {
		port = 993
		if m.IMAPTLS() != "tls" {
			port = 143
		}
	}
	return fmt.Sprintf("%s:%d", m.Host, port)
}

// Secret returns the IMAP password, reading password_file if set.
func (m *Mailbox) Secret() (string, error) {
	if m.PasswordFile != "" {
		b, err := os.ReadFile(expandHome(m.PasswordFile))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return m.Password, nil
}

// Describe is a one-line summary for listings (no secrets).
func (m *Mailbox) Describe() string {
	switch m.Type {
	case MailboxIMAP:
		return fmt.Sprintf("imap %s@%s (%s)", m.Username, m.IMAPAddr(), m.IMAPTLS())
	case MailboxGraph:
		return "graph (own sign-in)"
	}
	return m.Type
}

// MailboxTenant / MailboxClient resolve the app registration of a Graph mailbox.
func (c *Config) MailboxTenant(m *Mailbox) string {
	if m.TenantID != "" {
		return m.TenantID
	}
	return c.TenantID
}

func (c *Config) MailboxClient(m *Mailbox) string {
	if m.ClientID != "" {
		return m.ClientID
	}
	return c.ClientID
}

// ActiveMailboxes returns the mailboxes that are not disabled.
func (c *Config) ActiveMailboxes() []Mailbox {
	var out []Mailbox
	for _, m := range c.Mailboxes {
		if !m.Disabled {
			out = append(out, m)
		}
	}
	return out
}

// FindMailbox returns the index of the mailbox named n (case-insensitive), or -1.
func (c *Config) FindMailbox(n string) int {
	for i, m := range c.Mailboxes {
		if strings.EqualFold(m.Name, n) {
			return i
		}
	}
	return -1
}

// AddMailbox validates m and appends it; names must be unique.
func (c *Config) AddMailbox(m Mailbox) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if c.FindMailbox(m.Name) >= 0 {
		return fmt.Errorf("a mailbox named %q already exists", m.Name)
	}
	c.Mailboxes = append(append([]Mailbox(nil), c.Mailboxes...), m)
	return nil
}

// RemoveMailbox deletes the mailbox named n and returns it.
func (c *Config) RemoveMailbox(n string) (Mailbox, error) {
	i := c.FindMailbox(n)
	if i < 0 {
		return Mailbox{}, fmt.Errorf("no mailbox named %q", n)
	}
	m := c.Mailboxes[i]
	out := append([]Mailbox(nil), c.Mailboxes[:i]...)
	c.Mailboxes = append(out, c.Mailboxes[i+1:]...)
	return m, nil
}

// Edit loads the config file at path (without defaults from flags), applies fn and
// saves it. Keys fn does not touch are written back unchanged. Nothing is written
// if fn fails.
func Edit(path string, fn func(*Config) error) (*Config, error) {
	cfg, err := Load(path, false)
	if err != nil {
		return nil, err
	}
	if err := fn(cfg); err != nil {
		return nil, err
	}
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetMailboxDisabled enables or disables the mailbox named n.
func (c *Config) SetMailboxDisabled(n string, disabled bool) error {
	i := c.FindMailbox(n)
	if i < 0 {
		return fmt.Errorf("no mailbox named %q", n)
	}
	c.Mailboxes = append([]Mailbox(nil), c.Mailboxes...)
	c.Mailboxes[i].Disabled = disabled
	return nil
}
