// Package config loads, merges and validates ms365backupd settings.
//
// Precedence (lowest to highest): built-in defaults < config file < CLI flags.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration is a time.Duration that (un)marshals as a Go duration string ("30s", "15m").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// allow plain seconds as a number
		var n float64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return fmt.Errorf("duration must be a string like \"30s\": %w", err)
		}
		d.Duration = time.Duration(n * float64(time.Second))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Services selects which parts of the account are backed up.
type Services struct {
	OneDrive bool `json:"onedrive"`
	Mail     bool `json:"mail"`
	Contacts bool `json:"contacts"`
	Calendar bool `json:"calendar"`
}

// Email configures failure notifications and weekly reports.
type Email struct {
	To []string `json:"to"`
	// Via is "smtp" or "graph" (send through the backed-up account, needs Mail.Send).
	// Empty means: smtp if smtp_host is set, otherwise graph.
	Via          string `json:"via,omitempty"`
	From         string `json:"from,omitempty"`
	SMTPHost     string `json:"smtp_host,omitempty"`
	SMTPPort     int    `json:"smtp_port,omitempty"`
	Username     string `json:"username,omitempty"`
	Password     string `json:"password,omitempty"`
	PasswordFile string `json:"password_file,omitempty"`
	// TLS is "starttls" (default, port 587), "tls" (implicit, port 465) or "none".
	TLS string `json:"tls,omitempty"`

	WeeklyReportDay  string `json:"weekly_report_day"`  // monday..sunday, "" or "off" disables
	WeeklyReportTime string `json:"weekly_report_time"` // HH:MM local time
}

// Config is the full effective configuration.
type Config struct {
	// TenantID is the Directory (tenant) ID of the app registration (GUID) or the
	// tenant's primary domain such as contoso.onmicrosoft.com. Required.
	TenantID string `json:"tenant_id"`
	ClientID string `json:"client_id"`

	// Target is the mount point of the external disk. Data goes to <Target>/ms365backupd.
	Target string `json:"target"`
	// RequireMount refuses to write unless Target is a separate mounted filesystem.
	// Protects the SD card from filling up when the disk is unplugged.
	RequireMount bool `json:"require_mount"`

	Exclude  []string `json:"exclude"`
	Services Services `json:"services"`
	// SharePointSites lists the sites to back up. "*" (the default) means every site the
	// account can reach through its Microsoft 365 groups plus the root site. An empty
	// list disables SharePoint.
	SharePointSites []string `json:"sharepoint_sites"`

	Schedule      string   `json:"schedule"` // daily HH:MM local time
	RetryAttempts int      `json:"retry_attempts"`
	RetryDelay    Duration `json:"retry_delay"`
	PollInterval  Duration `json:"poll_interval"`
	Concurrency   int      `json:"concurrency"`
	KeepDeleted   bool     `json:"keep_deleted"` // move deleted items to .deleted/<date>/ instead of removing
	MinFreeMB     int64    `json:"min_free_mb"`

	StateDir string `json:"state_dir,omitempty"` // local state (tokens, history, socket)
	LogLevel string `json:"log_level"`
	LogFile  string `json:"log_file,omitempty"`

	GraphURL string `json:"graph_url,omitempty"` // override for testing

	Email Email `json:"email"`

	// Mailboxes are additional mailboxes backed up on top of the account's own mail
	// (to <target>/ms365backupd/mailboxes/<name>/).
	Mailboxes []Mailbox `json:"mailboxes"`
}

// AllSites in sharepoint_sites selects every SharePoint site the account belongs to.
const AllSites = "*"

// Default returns the built-in defaults.
func Default() *Config {
	return &Config{
		RequireMount:    true,
		Exclude:         []string{},
		SharePointSites: []string{AllSites},
		Services:        Services{OneDrive: true, Mail: true, Contacts: true, Calendar: true},
		Schedule:        "02:30",
		RetryAttempts:   3,
		RetryDelay:      Duration{20 * time.Minute},
		PollInterval:    Duration{30 * time.Second},
		Concurrency:     4,
		KeepDeleted:     true,
		MinFreeMB:       1024,
		LogLevel:        "info",
		Email: Email{
			To:               []string{},
			WeeklyReportDay:  "monday",
			WeeklyReportTime: "08:00",
		},
	}
}

// Dir returns ~/.config/ms365backupd (honours $XDG_CONFIG_HOME on both Linux and macOS).
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "ms365backupd")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ms365backupd")
}

// DefaultPath is ~/.config/ms365backupd/config.json.
func DefaultPath() string { return filepath.Join(Dir(), "config.json") }

// DefaultStateDir is ~/.local/state/ms365backupd.
func DefaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "ms365backupd")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ms365backupd")
}

// Load reads the config file at path on top of the defaults.
// A missing file is not an error when mustExist is false.
func Load(path string, mustExist bool) (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !mustExist {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// "sharepoint_sites": null (written by older versions) means "not set": use the
	// default. An explicit [] disables SharePoint.
	if cfg.SharePointSites == nil {
		cfg.SharePointSites = []string{AllSites}
	}
	if cfg.Exclude == nil {
		cfg.Exclude = []string{}
	}
	if cfg.Email.To == nil {
		cfg.Email.To = []string{}
	}
	if cfg.Mailboxes == nil {
		cfg.Mailboxes = []Mailbox{}
	}
	return cfg, nil
}

// Save writes cfg as indented JSON with 0600 permissions (it may contain an SMTP password).
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cp := *c
	// Write empty lists as [] (null would mean "default" for sharepoint_sites).
	if cp.SharePointSites == nil {
		cp.SharePointSites = []string{}
	}
	if cp.Exclude == nil {
		cp.Exclude = []string{}
	}
	if cp.Email.To == nil {
		cp.Email.To = []string{}
	}
	if cp.Mailboxes == nil {
		cp.Mailboxes = []Mailbox{}
	}
	b, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Redacted returns a copy without secrets, for display.
func (c *Config) Redacted() *Config {
	cp := *c
	if cp.Email.Password != "" {
		cp.Email.Password = "********"
	}
	cp.Mailboxes = make([]Mailbox, len(c.Mailboxes))
	for i, m := range c.Mailboxes {
		if m.Password != "" {
			m.Password = "********"
		}
		cp.Mailboxes[i] = m
	}
	return &cp
}

// EffectiveStateDir resolves the local state directory.
func (c *Config) EffectiveStateDir() string {
	if c.StateDir != "" {
		return expandHome(c.StateDir)
	}
	return DefaultStateDir()
}

// TargetPath returns the expanded target mount point.
func (c *Config) TargetPath() string { return expandHome(c.Target) }

// EmailEnabled reports whether notifications are configured.
func (c *Config) EmailEnabled() bool { return len(c.Email.To) > 0 }

// EmailVia resolves the delivery method.
func (c *Config) EmailVia() string {
	if c.Email.Via != "" {
		return strings.ToLower(c.Email.Via)
	}
	if c.Email.SMTPHost != "" {
		return "smtp"
	}
	return "graph"
}

// SMTPPassword returns the password, reading password_file if set.
func (c *Config) SMTPPassword() (string, error) {
	if c.Email.PasswordFile != "" {
		b, err := os.ReadFile(expandHome(c.Email.PasswordFile))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.Email.Password, nil
}

// Scopes returns the delegated Graph permissions needed for this configuration.
func (c *Config) Scopes() []string {
	s := []string{"offline_access", "User.Read"}
	// Files.Read.All covers SharePoint libraries the user can access, including
	// looking up a site by its path; Sites.Read.All is not needed.
	if c.Services.OneDrive || len(c.SharePointSites) > 0 {
		s = append(s, "Files.Read.All")
	}
	if c.Services.Mail {
		s = append(s, "Mail.Read")
	}
	if c.Services.Contacts {
		s = append(s, "Contacts.Read")
	}
	if c.Services.Calendar {
		s = append(s, "Calendars.Read")
	}
	if c.EmailEnabled() && c.EmailVia() == "graph" {
		s = append(s, "Mail.Send")
	}
	return s
}

// ParseClock parses "HH:MM".
func ParseClock(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, fmt.Errorf("invalid time %q (want HH:MM)", s)
	}
	return t.Hour(), t.Minute(), nil
}

// ParseWeekday parses an English weekday name (or 3-letter abbreviation).
func ParseWeekday(s string) (time.Weekday, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for d := time.Sunday; d <= time.Saturday; d++ {
		n := strings.ToLower(d.String())
		if s == n || (len(s) == 3 && strings.HasPrefix(n, s)) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("invalid weekday %q", s)
}

// WeeklyEnabled reports whether weekly reports are on.
func (c *Config) WeeklyEnabled() bool {
	d := strings.ToLower(strings.TrimSpace(c.Email.WeeklyReportDay))
	return c.EmailEnabled() && d != "" && d != "off" && d != "none"
}

// Validate checks the configuration. needAccount=false skips checks only needed
// for talking to Microsoft (used e.g. by `config show`).
func (c *Config) Validate(needAccount bool) error {
	var errs []string
	if needAccount && c.TenantID == "" {
		errs = append(errs, "tenant_id is required (Directory (tenant) ID on the app registration's Overview page)")
	}
	if needAccount && c.ClientID == "" {
		errs = append(errs, "client_id is required (Application (client) ID on the app registration's Overview page)")
	}
	if c.Target == "" {
		errs = append(errs, "target (mount point of the backup disk) is required")
	}
	if _, _, err := ParseClock(c.Schedule); err != nil {
		errs = append(errs, "schedule: "+err.Error())
	}
	if c.Concurrency < 1 || c.Concurrency > 32 {
		errs = append(errs, "concurrency must be between 1 and 32")
	}
	if c.RetryAttempts < 0 {
		errs = append(errs, "retry_attempts must be >= 0")
	}
	if c.PollInterval.Duration < time.Second {
		errs = append(errs, "poll_interval must be >= 1s")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, "log_level must be debug|info|warn|error")
	}
	if !c.Services.OneDrive && !c.Services.Mail && !c.Services.Contacts && !c.Services.Calendar && len(c.SharePointSites) == 0 &&
		len(c.ActiveMailboxes()) == 0 {
		errs = append(errs, "no services enabled")
	}
	seen := map[string]bool{}
	for i, m := range c.Mailboxes {
		what := fmt.Sprintf("mailboxes[%d]", i)
		if m.Name != "" {
			what = fmt.Sprintf("mailboxes[%q]", m.Name)
		}
		if err := m.Validate(); err != nil {
			errs = append(errs, what+": "+err.Error())
		}
		if k := strings.ToLower(m.Name); seen[k] {
			errs = append(errs, what+": duplicate name")
		} else {
			seen[k] = true
		}
	}
	if c.EmailEnabled() {
		switch c.EmailVia() {
		case "smtp":
			if c.Email.SMTPHost == "" {
				errs = append(errs, "email.smtp_host is required for smtp delivery")
			}
			if c.Email.From == "" {
				errs = append(errs, "email.from is required for smtp delivery")
			}
			switch strings.ToLower(c.Email.TLS) {
			case "", "starttls", "tls", "none":
			default:
				errs = append(errs, "email.tls must be starttls|tls|none")
			}
		case "graph":
		default:
			errs = append(errs, "email.via must be smtp or graph")
		}
		if c.WeeklyEnabled() {
			if _, err := ParseWeekday(c.Email.WeeklyReportDay); err != nil {
				errs = append(errs, "email.weekly_report_day: "+err.Error())
			}
			if _, _, err := ParseClock(c.Email.WeeklyReportTime); err != nil {
				errs = append(errs, "email.weekly_report_time: "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return errors.New("invalid configuration:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
