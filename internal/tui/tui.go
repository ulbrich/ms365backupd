// Package tui is a terminal dashboard for the ms365backupd daemon.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/daemon"
	"github.com/ulbrich/ms365backupd/internal/disk"
	"github.com/ulbrich/ms365backupd/internal/history"
	"github.com/ulbrich/ms365backupd/internal/notify"
	"github.com/ulbrich/ms365backupd/internal/status"
)

var (
	cAccent = lipgloss.AdaptiveColor{Light: "#005fd7", Dark: "#5fafff"}
	cOK     = lipgloss.AdaptiveColor{Light: "#008700", Dark: "#5fd75f"}
	cWarn   = lipgloss.AdaptiveColor{Light: "#af8700", Dark: "#ffd75f"}
	cBad    = lipgloss.AdaptiveColor{Light: "#d70000", Dark: "#ff5f5f"}
	cDim    = lipgloss.AdaptiveColor{Light: "#808080", Dark: "#808080"}

	sTitle = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	sDim   = lipgloss.NewStyle().Foreground(cDim)
	sOK    = lipgloss.NewStyle().Foreground(cOK)
	sWarn  = lipgloss.NewStyle().Foreground(cWarn)
	sBad   = lipgloss.NewStyle().Foreground(cBad)
	sBold  = lipgloss.NewStyle().Bold(true)
	sBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cDim).Padding(0, 1)
)

type statusMsg struct {
	st   *status.Daemon
	hist []status.RunResult
	err  error
}
type tickMsg time.Time
type flashMsg string

type mode int

const (
	modeDashboard mode = iota
	modeSetup
	modeLogin
)

// Options control how the TUI starts.
type Options struct {
	// ConfigPath is the config file the setup screen edits.
	ConfigPath string
	// Setup opens the setup screen first. If the config file does not exist yet this
	// is a first run: cancelling setup quits without writing anything.
	Setup bool
	// Reload returns the effective config (file + command line flags) after setup saved
	// the file. Defaults to reading the file.
	Reload func() (*config.Config, error)
}

type model struct {
	cfg      *config.Config
	opts     Options
	client   *daemon.Client
	stateDir string
	st       *status.Daemon
	hist     []status.RunResult
	err      error
	flash    string
	flashAt  time.Time
	width    int
	confirm  string // pending confirmation action

	mode  mode
	setup *setup
	quit  string // printed after the TUI exits

	login       *auth.DeviceLogin
	loginErr    error
	loginCancel context.CancelFunc
}

type loginStartedMsg struct {
	dl  *auth.DeviceLogin
	err error
}
type loginDoneMsg struct {
	account string
	err     error
}
type reloadedMsg struct{ text string }

// Run starts the TUI.
func Run(cfg *config.Config, opts Options) error {
	if opts.ConfigPath == "" {
		opts.ConfigPath = config.DefaultPath()
	}
	if opts.Reload == nil {
		opts.Reload = func() (*config.Config, error) { return config.Load(opts.ConfigPath, false) }
	}
	m := model{cfg: cfg, opts: opts, stateDir: cfg.EffectiveStateDir(), client: daemon.NewClient(cfg.EffectiveStateDir()), width: 100}
	if opts.Setup {
		if err := m.openSetup(); err != nil {
			return err
		}
	}
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if fm, ok := final.(model); ok && fm.quit != "" {
		fmt.Println(fm.quit)
	}
	return err
}

// openSetup starts the setup screen on the config file content (without flag overrides).
func (m *model) openSetup() error {
	_, statErr := os.Stat(m.opts.ConfigPath)
	firstRun := statErr != nil
	base, err := config.Load(m.opts.ConfigPath, false)
	if err != nil {
		return err
	}
	m.setup = newSetup(m.opts.ConfigPath, base, firstRun, m.width)
	m.mode = modeSetup
	return nil
}

func (m *model) setFlash(s string) { m.flash, m.flashAt = s, time.Now() }

// setupFinished handles leaving the setup screen.
func (m model) setupFinished() (tea.Model, tea.Cmd) {
	s := m.setup
	m.setup, m.mode = nil, modeDashboard
	if !s.saved {
		if s.firstRun {
			m.quit = "Setup cancelled; no configuration was written."
			return m, tea.Quit
		}
		m.setFlash("setup cancelled, nothing changed")
		return m, nil
	}
	if cfg, err := m.opts.Reload(); err == nil {
		m.cfg = cfg
	} else {
		m.cfg = s.savedCfg
	}
	msg := "Configuration saved to " + s.path + "."
	if _, err := auth.New(m.cfg).TokenSource(context.Background()); err != nil || len(auth.New(m.cfg).MissingScopes()) > 0 {
		msg += " Press l to sign in."
	}
	m.setFlash(msg)
	return m, tea.Batch(m.fetch(), m.reloadDaemon(msg))
}

// reloadDaemon asks a running daemon to apply the new configuration.
func (m model) reloadDaemon(prefix string) tea.Cmd {
	return func() tea.Msg {
		deferred, err := m.client.Reload()
		switch {
		case errors.Is(err, daemon.ErrNotRunning):
			return nil
		case err != nil:
			return reloadedMsg{prefix + " Daemon rejected it: " + err.Error()}
		case deferred:
			return reloadedMsg{prefix + " The daemon applies it after the current backup."}
		}
		return reloadedMsg{prefix + " The daemon is using it now."}
	}
}

func (m model) startLogin() (tea.Model, tea.Cmd) {
	if m.cfg.ClientID == "" {
		m.setFlash("set the client ID first (press s for setup)")
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mode, m.login, m.loginErr, m.loginCancel = modeLogin, nil, nil, cancel
	a := auth.New(m.cfg)
	return m, func() tea.Msg {
		dl, err := a.StartLogin(ctx)
		return loginStartedMsg{dl, err}
	}
}

func waitLogin(ctx context.Context, dl *auth.DeviceLogin) tea.Cmd {
	return func() tea.Msg {
		acc, err := dl.Wait(ctx)
		return loginDoneMsg{acc, err}
	}
}

func (m model) loginView() string {
	var b strings.Builder
	b.WriteString(sTitle.Render("Sign in to Microsoft 365") + "\n\n")
	var body string
	switch {
	case m.loginErr != nil:
		body = sBad.Render("Sign-in failed: "+m.loginErr.Error()) + "\n\n" +
			sDim.Render("Check tenant ID and client ID (s), and that \"Allow public client flows\" is enabled in the app registration.")
	case m.login == nil:
		body = sDim.Render("Requesting a sign-in code…")
	default:
		body = "1. Open  " + sTitle.Render(m.login.URI) + "  in a browser on any device\n" +
			"2. Enter the code  " + lipgloss.NewStyle().Bold(true).Foreground(cOK).Render(m.login.Code) + "\n" +
			"3. Sign in with the account to back up and accept the permissions\n\n" +
			sDim.Render("Requested: "+strings.Join(m.cfg.Scopes(), " ")) + "\n" +
			sWarn.Render("● waiting for sign-in") + sDim.Render(" — code expires in "+until(m.login.Expiry))
	}
	b.WriteString(sBox.Width(max(60, m.width)-2).Render(body) + "\n\n")
	b.WriteString(sDim.Render("esc cancel"))
	return b.String()
}

func (m model) Init() tea.Cmd { return tea.Batch(m.fetch(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) fetch() tea.Cmd {
	return func() tea.Msg {
		st, err := m.client.Status()
		if err != nil {
			// Daemon not running: show local information.
			info := disk.Probe(m.cfg.TargetPath(), m.cfg.RequireMount)
			h, _ := history.Open(m.stateDir).Last(12)
			return statusMsg{st: &status.Daemon{Disk: status.DiskState{
				Path: info.Path, Present: info.Present, Reason: info.Reason, DiskID: info.DiskID, Free: info.Free, Total: info.Total,
			}, Target: m.cfg.TargetPath()}, hist: h, err: err}
		}
		h, _ := m.client.History(12)
		return statusMsg{st: st, hist: h}
	}
}

func (m model) act(name string, f func() error) tea.Cmd {
	return func() tea.Msg {
		if err := f(); err != nil {
			return flashMsg(name + " failed: " + err.Error())
		}
		return flashMsg(name + ": ok")
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Messages every mode needs.
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		if m.setup != nil {
			m.setup.resize(msg.Width)
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.fetch(), tick())
	case statusMsg:
		m.st, m.hist, m.err = msg.st, msg.hist, msg.err
		return m, nil
	case reloadedMsg:
		m.setFlash(msg.text)
		return m, m.fetch()
	case loginStartedMsg:
		if m.mode != modeLogin {
			return m, nil
		}
		if msg.err != nil {
			m.loginErr = msg.err
			return m, nil
		}
		m.login = msg.dl
		ctx, cancel := context.WithDeadline(context.Background(), msg.dl.Expiry)
		old := m.loginCancel
		m.loginCancel = func() { cancel(); old() }
		return m, waitLogin(ctx, msg.dl)
	case loginDoneMsg:
		if m.mode != modeLogin {
			return m, nil
		}
		if msg.err != nil {
			if errors.Is(msg.err, context.DeadlineExceeded) {
				msg.err = errors.New("the code expired; press esc and l to try again")
			}
			m.loginErr = msg.err
			return m, nil
		}
		m.loginCancel()
		m.mode, m.login = modeDashboard, nil
		m.setFlash("Signed in as " + msg.account + ". Press b to start a backup.")
		return m, tea.Batch(m.fetch(), m.reloadDaemon("Signed in as "+msg.account+". Press b to start a backup."))
	}

	switch m.mode {
	case modeSetup:
		cmd := m.setup.Update(msg)
		if m.setup.done {
			return m.setupFinished()
		}
		return m, cmd
	case modeLogin:
		if km, ok := msg.(tea.KeyMsg); ok {
			switch km.String() {
			case "esc", "q", "ctrl+c":
				m.loginCancel()
				m.mode, m.login, m.loginErr = modeDashboard, nil, nil
				m.setFlash("sign-in cancelled")
			}
		}
		return m, nil
	}

	switch msg := msg.(type) {
	case flashMsg:
		m.flash, m.flashAt = string(msg), time.Now()
		return m, m.fetch()
	case tea.KeyMsg:
		k := msg.String()
		if m.confirm != "" {
			c := m.confirm
			m.confirm = ""
			if k == "y" || k == "Y" {
				switch c {
				case "full":
					return m, m.act("full resync", func() error { return m.client.Backup(nil, true) })
				case "cancel":
					return m, m.act("cancel", func() error { _, err := m.client.Cancel(); return err })
				}
			}
			m.flash, m.flashAt = "aborted", time.Now()
			return m, nil
		}
		switch k {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "b":
			return m, m.act("backup", func() error { return m.client.Backup(nil, false) })
		case "f":
			m.confirm = "full"
		case "c":
			m.confirm = "cancel"
		case "m":
			m.flash, m.flashAt = "sending report…", time.Now()
			return m, m.act("weekly report", m.client.Report)
		case "r":
			return m, m.fetch()
		case "s":
			if err := m.openSetup(); err != nil {
				m.setFlash("cannot open setup: " + err.Error())
				return m, nil
			}
			return m, m.setup.focusCurrent()
		case "l":
			return m.startLogin()
		}
	}
	return m, nil
}

func (m model) View() string {
	switch m.mode {
	case modeSetup:
		return m.setup.View()
	case modeLogin:
		return m.loginView()
	}
	if m.st == nil {
		return "loading…"
	}
	w := m.width
	if w < 60 {
		w = 60
	}
	var b strings.Builder
	// Header
	head := sTitle.Render("ms365backupd")
	if m.err != nil {
		head += "  " + sBad.Render("● daemon not running") + sDim.Render("  (start: ms365backupd run / systemctl start ms365backupd@$USER)")
	} else {
		head += "  " + sOK.Render("● daemon running") + sDim.Render(fmt.Sprintf("  pid %d · %s · up %s", m.st.PID, m.st.Version, since(m.st.Started)))
		if m.st.Account != "" {
			head += sDim.Render(" · ") + m.st.Account
		}
	}
	b.WriteString(head + "\n")
	if m.st.AuthError != "" {
		b.WriteString(sBad.Render("  login problem: "+m.st.AuthError) + sDim.Render("  → press l to sign in") + "\n")
	}

	half := (w - 4) / 2
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top,
		sBox.Width(half).Render(m.diskView(half-4)),
		sBox.Width(w-half-4).Render(m.scheduleView()),
	) + "\n")
	b.WriteString(sBox.Width(w-2).Render(m.runView()) + "\n")
	b.WriteString(sBox.Width(w-2).Render(m.historyView()) + "\n")

	if m.confirm != "" {
		q := map[string]string{"full": "Start a FULL resync (re-checks every item)?", "cancel": "Cancel the running backup?"}[m.confirm]
		b.WriteString(sWarn.Render(q+" [y/N]") + "\n")
	} else if m.flash != "" && time.Since(m.flashAt) < 6*time.Second {
		b.WriteString(sWarn.Render(m.flash) + "\n")
	} else {
		b.WriteString("\n")
	}
	keys := []string{"b backup now", "f full resync", "c cancel run", "m send weekly report", "s setup", "l sign in", "r refresh", "q quit"}
	b.WriteString(sDim.Render(strings.Join(keys, " · ")))
	return b.String()
}

func (m model) diskView(width int) string {
	d := m.st.Disk
	var b strings.Builder
	b.WriteString(sBold.Render("Backup disk") + "\n")
	b.WriteString(d.Path + "\n")
	if !d.Present {
		b.WriteString(sBad.Render("● not available") + "\n" + sDim.Render(d.Reason))
		return b.String()
	}
	b.WriteString(sOK.Render("● connected"))
	if d.DiskID != "" {
		b.WriteString(sDim.Render("  id " + d.DiskID[:min(8, len(d.DiskID))]))
	} else {
		b.WriteString(sWarn.Render("  new disk (full backup on next run)"))
	}
	b.WriteString("\n")
	if d.Total > 0 {
		used := float64(d.Total-d.Free) / float64(d.Total)
		bw := max(10, width-22)
		fill := int(used * float64(bw))
		st := sOK
		if used > 0.9 {
			st = sBad
		} else if used > 0.75 {
			st = sWarn
		}
		b.WriteString(st.Render(strings.Repeat("█", fill)) + sDim.Render(strings.Repeat("░", bw-fill)))
		b.WriteString(fmt.Sprintf(" %3.0f%%\n", used*100))
		b.WriteString(sDim.Render(fmt.Sprintf("%s free of %s", notify.HumanBytes(d.Free), notify.HumanBytes(d.Total))))
	}
	return b.String()
}

func (m model) scheduleView() string {
	var b strings.Builder
	b.WriteString(sBold.Render("Schedule") + "\n")
	if !m.st.NextRun.IsZero() {
		why := ""
		if m.st.NextRunWhy == daemon.TriggerRetry {
			why = sWarn.Render(" (retry)")
		}
		b.WriteString(fmt.Sprintf("next run:    %s%s %s\n", m.st.NextRun.Format("Mon 15:04"), why, sDim.Render("in "+until(m.st.NextRun))))
	} else {
		b.WriteString(fmt.Sprintf("daily at:    %s\n", m.cfg.Schedule))
	}
	if m.st.LastSuccess != nil {
		age := time.Since(*m.st.LastSuccess)
		st := sOK
		if age > 26*time.Hour {
			st = sBad
		}
		b.WriteString("last ok:     " + st.Render(m.st.LastSuccess.Format("Mon Jan 2 15:04")) + sDim.Render(" ("+since(*m.st.LastSuccess)+" ago)") + "\n")
	} else if len(m.hist) > 0 {
		for _, r := range m.hist {
			if r.OK() {
				b.WriteString("last ok:     " + sOK.Render(r.Finished.Format("Mon Jan 2 15:04")) + "\n")
				break
			}
		}
	} else {
		b.WriteString("last ok:     " + sWarn.Render("never") + "\n")
	}
	if m.st.NextReport != nil {
		b.WriteString(fmt.Sprintf("report:      %s\n", m.st.NextReport.Format("Mon Jan 2 15:04")))
	} else if m.cfg.EmailEnabled() {
		b.WriteString("report:      " + sDim.Render("weekly report off") + "\n")
	} else {
		b.WriteString("email:       " + sDim.Render("not configured") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m model) runView() string {
	var b strings.Builder
	p := m.st.Running
	if p == nil {
		b.WriteString(sBold.Render("Current run") + "  " + sDim.Render("idle"))
		if l := m.st.Last; l != nil {
			b.WriteString("\n" + "last: " + statusStyled(l.Status) + sDim.Render(fmt.Sprintf("  %s · %s · %s", l.Started.Format("Mon 15:04"), l.Trigger, l.Duration().Round(time.Second))))
			if l.Error != "" {
				b.WriteString("\n" + sBad.Render(truncate(l.Error, m.width-8)))
			}
		}
		return b.String()
	}
	b.WriteString(sBold.Render("Current run") + "  " + sWarn.Render("● running") + sDim.Render(fmt.Sprintf("  %s · started %s · %s", p.Trigger, p.Started.Format("15:04:05"), since(p.Started))) + "\n")
	svc := p.Service
	if svc == "" {
		svc = "-"
	}
	b.WriteString(fmt.Sprintf("%s · %s", sTitle.Render(svc), p.Phase))
	if p.Queued > 0 {
		b.WriteString(sDim.Render(fmt.Sprintf("  (%d queued)", p.Queued)))
	}
	b.WriteString("\n")
	t := p.Totals
	b.WriteString(fmt.Sprintf("new %s  updated %s  deleted %s  moved %s  unchanged %s  skipped %s  errors %s  downloaded %s",
		sOK.Render(fmt.Sprint(t.New)), fmt.Sprint(t.Updated), fmt.Sprint(t.Deleted), fmt.Sprint(t.Moved),
		sDim.Render(fmt.Sprint(t.Unchanged)), fmt.Sprint(t.Skipped), errStyled(t.Errors), notify.HumanBytes(uint64(t.Bytes))))
	if p.Current != "" {
		b.WriteString("\n" + sDim.Render(truncate(p.Current, m.width-8)))
	}
	return b.String()
}

func (m model) historyView() string {
	var b strings.Builder
	b.WriteString(sBold.Render("Recent runs") + "\n")
	if len(m.hist) == 0 {
		b.WriteString(sDim.Render("no runs yet"))
		return b.String()
	}
	b.WriteString(sDim.Render(fmt.Sprintf("%-16s %-15s %-8s %9s %7s %7s %7s %6s %10s", "started", "trigger", "status", "duration", "new", "upd", "del", "err", "download")) + "\n")
	for _, r := range m.hist {
		b.WriteString(fmt.Sprintf("%-16s %-15s %s %9s %7d %7d %7d %s %10s\n",
			r.Started.Format("Mon 02 Jan 15:04"), truncate(r.Trigger, 15), padRight(statusStyled(r.Status), r.Status, 8),
			r.Duration().Round(time.Second), r.Totals.New, r.Totals.Updated, r.Totals.Deleted,
			padLeft(errStyled(r.Totals.Errors), fmt.Sprint(r.Totals.Errors), 6), notify.HumanBytes(uint64(r.Totals.Bytes))))
	}
	return strings.TrimRight(b.String(), "\n")
}

func statusStyled(s string) string {
	switch s {
	case status.StatusSuccess:
		return sOK.Render(s)
	case status.StatusPartial:
		return sWarn.Render(s)
	default:
		return sBad.Render(s)
	}
}

func errStyled(n int64) string {
	if n > 0 {
		return sBad.Render(fmt.Sprint(n))
	}
	return fmt.Sprint(n)
}

// padRight / padLeft pad a styled string using the width of its plain text.
func padRight(styled, plain string, width int) string {
	return styled + strings.Repeat(" ", max(0, width-len(plain)))
}

func padLeft(styled, plain string, width int) string {
	return strings.Repeat(" ", max(0, width-len(plain))) + styled
}

func truncate(s string, n int) string {
	if n < 4 {
		n = 4
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func since(t time.Time) string { return human(time.Since(t)) }
func until(t time.Time) string { return human(time.Until(t)) }

func human(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
