package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/disk"
	"github.com/ulbrich/ms365backupd/internal/notify"
)

type fieldKind int

const (
	kText fieldKind = iota
	kSecret
	kBool
	kChoice
	kLines     // multi-line list, one entry per line
	kMailboxes // list of additional mailboxes (own editor, value is JSON)
)

type field struct {
	label, help string
	kind        fieldKind
	choices     []string
	in          textinput.Model
	area        textarea.Model
	b           bool
	ci          int
	orig, def   string
	get         func(*config.Config) string
	set         func(*config.Config, string) error
	show        func(*setup) bool
	mb          *mbEditor
}

func (f *field) value() string {
	switch f.kind {
	case kBool:
		return strconv.FormatBool(f.b)
	case kChoice:
		return f.choices[f.ci]
	case kLines:
		return strings.Join(splitList(f.area.Value(), "\n"), "\n")
	case kMailboxes:
		return mailboxesJSON(f.mb.list)
	default:
		return strings.TrimSpace(f.in.Value())
	}
}

func (f *field) setValue(v string) {
	switch f.kind {
	case kBool:
		f.b = v == "true"
	case kChoice:
		f.ci = 0
		for i, c := range f.choices {
			if c == v {
				f.ci = i
			}
		}
	case kLines:
		f.area.SetValue(v)
	case kMailboxes:
		f.mb.list = parseMailboxes(v)
	default:
		f.in.SetValue(v)
	}
}

func (f *field) focus() tea.Cmd {
	switch f.kind {
	case kLines:
		return f.area.Focus()
	case kText, kSecret:
		return f.in.Focus()
	}
	return nil
}

func (f *field) blur() {
	switch f.kind {
	case kText, kSecret:
		f.in.Blur()
	case kLines:
		f.area.Blur()
	}
}

// display renders the value for lists of changes (secrets masked).
func (f *field) display(v string) string {
	switch {
	case f.kind == kSecret && v != "":
		return "••••••"
	case f.kind == kMailboxes:
		var names []string
		for _, m := range parseMailboxes(v) {
			names = append(names, m.Name)
		}
		if len(names) == 0 {
			return "(none)"
		}
		return strings.Join(names, ", ")
	case v == "":
		return "(empty)"
	case f.kind == kLines:
		return strings.ReplaceAll(v, "\n", ", ")
	}
	return v
}

type step struct {
	title, intro string
	fields       []*field
	info         func(*setup) string
}

// setup is the configuration wizard. It edits a copy of the config file's content
// (without command line overrides) and writes it only on save.
type setup struct {
	path     string
	firstRun bool
	base     *config.Config
	steps    []step
	cur, foc int
	width    int
	confirm  string // "discard" | "invalid"
	problems []string
	flash    string

	done, saved bool
	savedCfg    *config.Config

	probeKey string
	probe    disk.Info

	accountKey, accountText string
	inputWidth              int

	// loginReq asks the parent to sign in an additional Graph mailbox.
	loginReq *config.Mailbox
}

func newSetup(path string, base *config.Config, firstRun bool, width int) *setup {
	s := &setup{path: path, base: base, firstRun: firstRun, width: width}
	s.steps = buildSteps()
	def := config.Default()
	for _, st := range s.steps {
		for _, f := range st.fields {
			f.orig = f.get(base)
			f.def = f.get(def)
			switch f.kind {
			case kText, kSecret:
				f.in = textinput.New()
				f.in.Prompt = ""
				f.in.CharLimit = 512
				if f.kind == kSecret {
					f.in.EchoMode = textinput.EchoPassword
					f.in.EchoCharacter = '•'
				}
			case kLines:
				f.area = textarea.New()
				f.area.ShowLineNumbers = false
				f.area.Prompt = "│ "
				f.area.SetHeight(5)
				f.area.CharLimit = 0
			case kMailboxes:
				f.mb = &mbEditor{notes: map[string]string{}, testing: map[string]bool{}}
			}
			f.setValue(f.orig)
		}
	}
	s.resize(width)
	s.focusCurrent()
	return s
}

func (s *setup) resize(w int) {
	s.width = w
	iw := max(20, min(60, w-34))
	s.inputWidth = iw
	for _, st := range s.steps {
		for _, f := range st.fields {
			switch f.kind {
			case kText, kSecret:
				f.in.Width = iw
			case kLines:
				f.area.SetWidth(iw + 2)
			}
		}
	}
}

func (s *setup) visible() []*field {
	var out []*field
	for _, f := range s.steps[s.cur].fields {
		if f.show == nil || f.show(s) {
			out = append(out, f)
		}
	}
	return out
}

func (s *setup) focusCurrent() tea.Cmd {
	for _, st := range s.steps {
		for _, f := range st.fields {
			f.blur()
		}
	}
	v := s.visible()
	if len(v) == 0 {
		return nil
	}
	s.foc = max(0, min(s.foc, len(v)-1))
	return v[s.foc].focus()
}

func (s *setup) value(label string) string {
	for _, st := range s.steps {
		for _, f := range st.fields {
			if f.label == label {
				return f.value()
			}
		}
	}
	return ""
}

func (s *setup) dirty() bool {
	for _, st := range s.steps {
		for _, f := range st.fields {
			if f.value() != f.orig {
				return true
			}
		}
	}
	return false
}

// build applies all fields to a copy of the base config.
func (s *setup) build() (*config.Config, map[*field]error) {
	cp := *s.base
	cp.Exclude = append([]string(nil), s.base.Exclude...)
	cp.SharePointSites = append([]string(nil), s.base.SharePointSites...)
	cp.Email.To = append([]string(nil), s.base.Email.To...)
	cp.Mailboxes = append([]config.Mailbox(nil), s.base.Mailboxes...)
	errs := map[*field]error{}
	for _, st := range s.steps {
		for _, f := range st.fields {
			if err := f.set(&cp, f.value()); err != nil {
				errs[f] = err
			}
		}
	}
	return &cp, errs
}

func (s *setup) stepErrors(i int) (fieldErr bool) {
	_, errs := s.build()
	for _, f := range s.steps[i].fields {
		if errs[f] != nil {
			return true
		}
	}
	return false
}

func (s *setup) next(d int) tea.Cmd {
	s.cur = max(0, min(len(s.steps)-1, s.cur+d))
	s.foc = 0
	if d < 0 {
		s.foc = len(s.visible()) - 1
	}
	return s.focusCurrent()
}

func (s *setup) moveFocus(d int) tea.Cmd {
	v := s.visible()
	n := s.foc + d
	if n >= len(v) {
		return s.next(1)
	}
	if n < 0 {
		if s.cur == 0 {
			return nil
		}
		return s.next(-1)
	}
	s.foc = n
	return s.focusCurrent()
}

func (s *setup) cancel() {
	if s.dirty() {
		s.confirm = "discard"
		return
	}
	s.done = true
}

// mailboxEditor returns the mailbox list editor.
func (s *setup) mailboxEditor() *mbEditor {
	for _, st := range s.steps {
		for _, f := range st.fields {
			if f.kind == kMailboxes {
				return f.mb
			}
		}
	}
	return nil
}

// gotoStep shows the step with the given title.
func (s *setup) gotoStep(title string) tea.Cmd {
	for i, st := range s.steps {
		if st.title == title {
			s.cur, s.foc = i, 0
		}
	}
	return s.focusCurrent()
}

func (s *setup) save(force bool) {
	cfg, errs := s.build()
	if len(errs) > 0 {
		s.cur = len(s.steps) - 1
		s.flash = "fix the marked fields first"
		return
	}
	if err := cfg.Validate(true); err != nil && !force {
		s.cur = len(s.steps) - 1
		s.confirm = "invalid"
		return
	}
	if err := cfg.Save(s.path); err != nil {
		s.flash = "saving failed: " + err.Error()
		return
	}
	// Removed Graph mailboxes no longer need their sign-in.
	for i := range s.base.Mailboxes {
		old := &s.base.Mailboxes[i]
		if old.Type == config.MailboxGraph && cfg.FindMailbox(old.Name) < 0 {
			_ = auth.ForMailbox(s.base, old).Logout()
		}
	}
	s.done, s.saved, s.savedCfg = true, true, cfg
}

// Update handles a key; the parent checks s.done afterwards.
func (s *setup) Update(msg tea.Msg) tea.Cmd {
	if tm, ok := msg.(mbTestMsg); ok {
		if e := s.mailboxEditor(); e != nil {
			e.testDone(tm)
		}
		return nil
	}
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return s.passThrough(msg)
	}
	k := km.String()
	s.flash = ""
	if v := s.visible(); s.confirm == "" && len(v) > 0 && v[s.foc].kind == kMailboxes {
		if cmd, handled := s.updateMailboxes(v[s.foc].mb, km); handled {
			return cmd
		}
	}
	if s.confirm != "" {
		c := s.confirm
		s.confirm = ""
		if k == "y" || k == "Y" {
			switch c {
			case "discard":
				s.done = true
			case "invalid":
				s.save(true)
			}
		}
		return nil
	}
	switch k {
	case "esc", "ctrl+c":
		s.cancel()
		return nil
	case "ctrl+s":
		s.save(false)
		return nil
	case "pgdown", "ctrl+n":
		return s.next(1)
	case "pgup", "ctrl+p":
		return s.next(-1)
	case "tab":
		return s.moveFocus(1)
	case "shift+tab":
		return s.moveFocus(-1)
	}
	v := s.visible()
	if len(v) == 0 { // review page
		switch k {
		case "enter", "s":
			s.save(false)
		case "up", "left", "backspace", "b":
			return s.next(-1)
		}
		return nil
	}
	f := v[s.foc]
	switch k {
	case "up":
		if f.kind != kLines || f.area.Line() == 0 {
			return s.moveFocus(-1)
		}
	case "down":
		if f.kind != kLines || f.area.Line() >= f.area.LineCount()-1 {
			return s.moveFocus(1)
		}
	case "enter":
		if f.kind != kLines {
			return s.moveFocus(1)
		}
	}
	switch f.kind {
	case kBool:
		if k == " " || k == "left" || k == "right" || k == "x" {
			f.b = !f.b
		}
		return nil
	case kChoice:
		switch k {
		case "right", " ":
			f.ci = (f.ci + 1) % len(f.choices)
		case "left":
			f.ci = (f.ci + len(f.choices) - 1) % len(f.choices)
		}
		if s.foc >= len(s.visible()) { // a choice may hide following fields
			s.foc = len(s.visible()) - 1
		}
		return nil
	}
	return s.passThrough(msg)
}

func (s *setup) passThrough(msg tea.Msg) tea.Cmd {
	v := s.visible()
	if len(v) == 0 {
		return nil
	}
	f := v[s.foc]
	var cmd tea.Cmd
	switch f.kind {
	case kText, kSecret:
		f.in, cmd = f.in.Update(msg)
	case kLines:
		f.area, cmd = f.area.Update(msg)
	}
	return cmd
}

func (s *setup) View() string {
	w := max(60, s.width)
	var b strings.Builder
	title := "Setup"
	if s.firstRun {
		title = "Welcome! Let's set up ms365backupd"
	}
	b.WriteString(sTitle.Render(title) + sDim.Render("  "+s.path) + "\n")
	if s.firstRun {
		b.WriteString(sDim.Render("No configuration found. Walk through the steps; nothing is written until you save.") + "\n")
	}
	b.WriteString("\n")

	// Step bar.
	var tabs []string
	for i, st := range s.steps {
		mark := ""
		if i < len(s.steps)-1 {
			if s.stepErrors(i) {
				mark = sBad.Render(" !")
			} else if s.stepMissing(i) {
				mark = sWarn.Render(" …")
			} else {
				mark = sOK.Render(" ✓")
			}
		}
		label := fmt.Sprintf("%d %s", i+1, st.title)
		if i == s.cur {
			label = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(cAccent).Render(label)
		} else {
			label = sDim.Render(label)
		}
		tabs = append(tabs, label+mark)
	}
	b.WriteString(strings.Join(tabs, sDim.Render("  ·  ")) + "\n\n")

	st := s.steps[s.cur]
	var body strings.Builder
	if st.intro != "" {
		body.WriteString(sDim.Render(wrap(st.intro, w-8)) + "\n\n")
	}
	if len(st.fields) == 0 {
		body.WriteString(s.reviewView(w))
	} else {
		_, errs := s.build()
		v := s.visible()
		for i, f := range v {
			body.WriteString(s.fieldView(f, i == s.foc, errs[f]) + "\n")
		}
		if f := v[s.foc]; f.help != "" && (f.mb == nil || f.mb.form == nil) {
			body.WriteString("\n" + sDim.Render(wrap(f.help, w-8)) + "\n")
		}
	}
	if st.info != nil {
		if info := st.info(s); info != "" {
			body.WriteString("\n" + info + "\n")
		}
	}
	b.WriteString(sBox.Width(w-2).Render(strings.TrimRight(body.String(), "\n")) + "\n")

	switch {
	case s.confirm == "discard":
		b.WriteString(sWarn.Render("Discard all changes? [y/N]") + "\n")
	case s.confirm == "invalid":
		b.WriteString(sWarn.Render("The configuration is incomplete (see above). Save anyway? [y/N]") + "\n")
	case s.flash != "":
		b.WriteString(sBad.Render(s.flash) + "\n")
	default:
		b.WriteString("\n")
	}
	keys := "tab/↑↓ field · ←→/space change · pgdn/ctrl+n next step · pgup/ctrl+p back · ctrl+s save · esc cancel"
	if len(st.fields) == 0 {
		keys = "enter save · b back · esc cancel"
	}
	b.WriteString(sDim.Render(keys))
	return b.String()
}

func (s *setup) fieldView(f *field, focused bool, err error) string {
	cursor := "  "
	label := fmt.Sprintf("%-22s", f.label)
	if focused {
		cursor = sTitle.Render("› ")
		label = sBold.Render(label)
	}
	var val string
	switch f.kind {
	case kBool:
		box := "[ ] no "
		if f.b {
			box = "[x] yes"
		}
		val = box
	case kChoice:
		if focused {
			val = sTitle.Render("‹ ") + f.choices[f.ci] + sTitle.Render(" ›")
		} else {
			val = f.choices[f.ci]
		}
	case kMailboxes:
		if focused {
			val = "\n\n" + s.mailboxesView(f.mb, s.width)
		} else if len(f.mb.list) == 0 {
			val = sDim.Render("(none)")
		} else {
			val = truncate(f.display(f.value()), max(20, s.width-40)) + sDim.Render(fmt.Sprintf("  (%d)", len(f.mb.list)))
		}
	case kLines:
		if focused {
			val = "\n" + f.area.View()
		} else {
			lines := splitList(f.area.Value(), "\n")
			switch len(lines) {
			case 0:
				val = sDim.Render("(none)")
			default:
				val = truncate(strings.Join(lines, ", "), max(20, s.width-40)) + sDim.Render(fmt.Sprintf("  (%d)", len(lines)))
			}
		}
	default:
		if f.in.Value() == "" && !focused {
			val = sDim.Render("(empty)")
		} else {
			val = f.in.View()
		}
	}
	if f.kind != kLines && f.kind != kMailboxes || !focused {
		val += strings.Repeat(" ", max(0, s.inputWidth+2-lipgloss.Width(val)))
	}
	var mark string
	cur := f.value()
	switch {
	case cur == "" && isRequired(f):
		mark = sWarn.Render("  required")
	case err != nil:
		mark = sBad.Render("  ✗ " + err.Error())
	case cur != f.orig:
		mark = sWarn.Render("  ● changed")
		if !s.firstRun {
			mark += sDim.Render(" (was " + truncate(f.display(f.orig), 30) + ")")
		}
	case cur == f.def && cur != "":
		mark = sDim.Render("  default")
	}
	if f.kind == kMailboxes && focused {
		return cursor + label + strings.TrimSpace(mark) + val
	}
	return cursor + label + val + mark
}

func isRequired(f *field) bool {
	return f.label == "Tenant ID" || f.label == "Client ID" || f.label == "Target (mount point)"
}

func (s *setup) stepMissing(i int) bool {
	for _, f := range s.steps[i].fields {
		if isRequired(f) && f.value() == "" {
			return true
		}
	}
	return false
}

func (s *setup) reviewView(w int) string {
	var b strings.Builder
	cfg, errs := s.build()
	var changes []string
	for _, st := range s.steps {
		for _, f := range st.fields {
			if v := f.value(); v != f.orig {
				if s.firstRun {
					changes = append(changes, fmt.Sprintf("  %-22s %s", f.label, f.display(v)))
				} else {
					changes = append(changes, fmt.Sprintf("  %-22s %s → %s", f.label, sDim.Render(truncate(f.display(f.orig), 30)), f.display(v)))
				}
			}
		}
	}
	if len(changes) == 0 {
		b.WriteString(sDim.Render("No changes.") + "\n")
	} else {
		b.WriteString(sBold.Render("Changes") + "\n" + strings.Join(changes, "\n") + "\n")
	}
	b.WriteString("\n")
	if len(errs) > 0 {
		b.WriteString(sBad.Render("Invalid fields:") + "\n")
		for _, st := range s.steps {
			for _, f := range st.fields {
				if errs[f] != nil {
					b.WriteString(sBad.Render(fmt.Sprintf("  %s (%s): %v", f.label, st.title, errs[f])) + "\n")
				}
			}
		}
	} else if err := cfg.Validate(true); err != nil {
		b.WriteString(sWarn.Render("Not complete yet:") + "\n")
		for _, l := range strings.Split(err.Error(), "\n")[1:] {
			b.WriteString(sWarn.Render(l) + "\n")
		}
	} else {
		b.WriteString(sOK.Render("✓ Configuration is complete.") + "\n")
	}
	b.WriteString("\n" + sDim.Render(wrap("Saving writes "+s.path+" (mode 0600). A running daemon picks up the new settings automatically.", w-8)))
	return b.String()
}

// ---- step definitions ----

func buildSteps() []step {
	return []step{
		{
			title: "Account",
			intro: "ms365backupd signs in with an Entra ID app registration of your own (public client, delegated permissions). " +
				"See README → \"Register an app\". After saving you can sign in right here.",
			fields: []*field{
				text("Tenant ID", "Directory (tenant) ID from the app registration's Overview page, e.g. 11111111-2222-3333-4444-555555555555 (the primary domain such as contoso.onmicrosoft.com works too).",
					func(c *config.Config) string { return c.TenantID },
					func(c *config.Config, v string) error {
						if v != "" && !looksLikeGUID(v) && !strings.Contains(v, ".") {
							return errors.New("use the Directory (tenant) ID (GUID) from the app registration")
						}
						c.TenantID = v
						return nil
					}),
				text("Client ID", "Application (client) ID of your app registration, e.g. 11111111-2222-3333-4444-555555555555.",
					func(c *config.Config) string { return c.ClientID },
					func(c *config.Config, v string) error {
						if v != "" && !looksLikeGUID(v) {
							return errors.New("expected a GUID")
						}
						c.ClientID = v
						return nil
					}),
			},
			info: accountInfo,
		},
		{
			title: "Disk",
			intro: "The external SSD. Backup data goes to <target>/ms365backupd; nothing else on the disk is touched. " +
				"A disk without a previous backup automatically gets a full backup.",
			fields: []*field{
				text("Target (mount point)", "Linux/Pi: e.g. /mnt/backup (mount with nofail). macOS: /Volumes/<name>.",
					func(c *config.Config) string { return c.Target },
					func(c *config.Config, v string) error { c.Target = v; return nil }),
				boolean("Require mount point", "Only write if the target is a separately mounted filesystem. Keeps an unplugged disk from filling the SD card. Disable only for testing.",
					func(c *config.Config) bool { return c.RequireMount },
					func(c *config.Config, v bool) { c.RequireMount = v }),
			},
			info: diskInfo,
		},
		{
			title: "Data",
			intro: "What to back up. Exclude patterns are relative to <target>/ms365backupd, case-insensitive, support * and **, " +
				"and exclude everything below a matching folder.",
			fields: []*field{
				boolean("OneDrive", "", func(c *config.Config) bool { return c.Services.OneDrive }, func(c *config.Config, v bool) { c.Services.OneDrive = v }),
				boolean("Mail", "Every message as .eml (MIME).", func(c *config.Config) bool { return c.Services.Mail }, func(c *config.Config, v bool) { c.Services.Mail = v }),
				boolean("Contacts", "", func(c *config.Config) bool { return c.Services.Contacts }, func(c *config.Config, v bool) { c.Services.Contacts = v }),
				boolean("Calendar", "", func(c *config.Config) bool { return c.Services.Calendar }, func(c *config.Config, v bool) { c.Services.Calendar = v }),
				list("SharePoint sites", "Comma separated. * = the root site plus the sites of all Microsoft 365 groups/Teams you are a member of. "+
					"Add other sites as contoso.sharepoint.com:/sites/Name. Empty = no SharePoint.",
					func(c *config.Config) []string { return c.SharePointSites },
					func(c *config.Config, v []string) {
						if v == nil {
							v = []string{}
						}
						c.SharePointSites = v
					}),
				lines("Exclude", "One pattern per line, e.g. onedrive/Videos, **/node_modules, **/*.iso, mail/Junk Email. Use tab to leave this box.",
					func(c *config.Config) []string { return c.Exclude },
					func(c *config.Config, v []string) { c.Exclude = v }),
			},
		},
		{
			title: mailboxesStep,
			intro: "Additional mailboxes, backed up next to the account's own mail (service \"mailboxes\"). " +
				"Changes are written when you save the setup.",
			fields: []*field{mailboxesField()},
		},
		{
			title: "Schedule",
			intro: "When to run and how to handle failures. Connecting a disk also starts a backup (unless it was backed up in the last 20 hours).",
			fields: []*field{
				text("Daily at (HH:MM)", "Local time of the daily backup.",
					func(c *config.Config) string { return c.Schedule },
					func(c *config.Config, v string) error {
						if _, _, err := config.ParseClock(v); err != nil {
							return errors.New("use HH:MM")
						}
						c.Schedule = v
						return nil
					}),
				intField("Retries", "Retries of a failed daily backup before the failure mail is sent.", 0, 20,
					func(c *config.Config) int { return c.RetryAttempts }, func(c *config.Config, v int) { c.RetryAttempts = v }),
				durField("Retry delay", "Wait before retry n is delay × n, e.g. 20m.",
					func(c *config.Config) *config.Duration { return &c.RetryDelay }),
				boolean("Keep deleted/changed", "Move items deleted or changed in Microsoft 365 to .deleted/<date>/ instead of removing them.",
					func(c *config.Config) bool { return c.KeepDeleted }, func(c *config.Config, v bool) { c.KeepDeleted = v }),
				intField("Min. free space (MB)", "Stop writing when the disk has less free space.", 0, 1<<30,
					func(c *config.Config) int { return int(c.MinFreeMB) }, func(c *config.Config, v int) { c.MinFreeMB = int64(v) }),
				intField("Parallel downloads", "1–32. 2–4 is right for a Raspberry Pi.", 1, 32,
					func(c *config.Config) int { return c.Concurrency }, func(c *config.Config, v int) { c.Concurrency = v }),
				choice("Log level", "", []string{"debug", "info", "warn", "error"},
					func(c *config.Config) string { return c.LogLevel }, func(c *config.Config, v string) { c.LogLevel = v }),
			},
		},
		{
			title:  "Email",
			intro:  "Failure alerts for daily backups and the weekly statistics. Leave recipients empty to disable mail.",
			fields: emailFields(),
			info:   emailInfo,
		},
		{
			title: "Save",
			intro: "Review your changes. Press enter to write the configuration, esc to leave without saving.",
		},
	}
}

func emailFields() []*field {
	smtp := func(s *setup) bool { return s.value("Send via") == "smtp" }
	weekly := func(s *setup) bool { return s.value("Weekly report") != "off" }
	return []*field{
		list("Recipients", "Comma separated email addresses.",
			func(c *config.Config) []string { return c.Email.To },
			func(c *config.Config, v []string) { c.Email.To = v }),
		choice("Send via", "smtp: any mail server. graph: send through the backed-up mailbox (needs Mail.Send; sign in again after changing).",
			[]string{"smtp", "graph"},
			func(c *config.Config) string { return c.EmailVia() }, func(c *config.Config, v string) { c.Email.Via = v }),
		withShow(text("From", "Sender address.", func(c *config.Config) string { return c.Email.From },
			func(c *config.Config, v string) error { c.Email.From = v; return nil }), smtp),
		withShow(text("SMTP server", "e.g. smtp.office365.com, smtp.gmail.com", func(c *config.Config) string { return c.Email.SMTPHost },
			func(c *config.Config, v string) error { c.Email.SMTPHost = v; return nil }), smtp),
		withShow(choice("Encryption", "starttls (port 587), tls (implicit, port 465), none (port 25, no credentials sent).",
			[]string{"starttls", "tls", "none"},
			func(c *config.Config) string {
				if c.Email.TLS == "" {
					return "starttls"
				}
				return c.Email.TLS
			}, func(c *config.Config, v string) { c.Email.TLS = v }), smtp),
		withShow(intField("SMTP port", "0 = default for the encryption (587/465/25).", 0, 65535,
			func(c *config.Config) int { return c.Email.SMTPPort }, func(c *config.Config, v int) { c.Email.SMTPPort = v }), smtp),
		withShow(text("Username", "Leave empty for servers without authentication.", func(c *config.Config) string { return c.Email.Username },
			func(c *config.Config, v string) error { c.Email.Username = v; return nil }), smtp),
		withShow(text("Password file", "Recommended: a file (mode 0600) that contains only the password, e.g. ~/.config/ms365backupd/smtp-password.",
			func(c *config.Config) string { return c.Email.PasswordFile },
			func(c *config.Config, v string) error { c.Email.PasswordFile = v; return nil }), smtp),
		withShow(secret("Password", "Stored in the config file (mode 0600). Prefer the password file.",
			func(c *config.Config) string { return c.Email.Password },
			func(c *config.Config, v string) { c.Email.Password = v }), smtp),
		choice("Weekly report", "Day of the weekly statistics mail.",
			[]string{"off", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"},
			func(c *config.Config) string {
				d := strings.ToLower(c.Email.WeeklyReportDay)
				if d == "" {
					return "off"
				}
				return d
			}, func(c *config.Config, v string) { c.Email.WeeklyReportDay = v }),
		withShow(text("Report time (HH:MM)", "", func(c *config.Config) string { return c.Email.WeeklyReportTime },
			func(c *config.Config, v string) error {
				if _, _, err := config.ParseClock(v); err != nil {
					return errors.New("use HH:MM")
				}
				c.Email.WeeklyReportTime = v
				return nil
			}), weekly),
	}
}

// ---- info panels ----

func accountInfo(s *setup) string {
	cfg, _ := s.build()
	key := fmt.Sprintf("%s|%s|%d", cfg.TenantID, cfg.ClientID, time.Now().Unix()/5)
	if key == s.accountKey {
		return s.accountText
	}
	s.accountKey, s.accountText = key, renderAccount(cfg)
	return s.accountText
}

func renderAccount(cfg *config.Config) string {
	if cfg.ClientID == "" {
		return sDim.Render("● enter the client ID; you can sign in after saving")
	}
	a := auth.New(cfg)
	acc := a.Account()
	if _, err := a.TokenSource(context.Background()); err != nil || acc == "" {
		msg := "not signed in"
		if err != nil && !errors.Is(err, auth.ErrNotLoggedIn) {
			msg = err.Error()
		}
		return sWarn.Render("● "+msg) + sDim.Render(" — after saving, press l on the dashboard to sign in")
	}
	out := sOK.Render("● signed in as "+acc) + sDim.Render(" — the token renews automatically")
	if miss := a.MissingScopes(); len(miss) > 0 {
		out += "\n" + sWarn.Render("  permissions not granted yet: "+strings.Join(miss, " ")+" → sign in again (l) after saving")
	}
	return out
}

func diskInfo(s *setup) string {
	target := s.value("Target (mount point)")
	if target == "" {
		return ""
	}
	req := s.value("Require mount point") == "true"
	key := fmt.Sprintf("%s|%v|%d", target, req, time.Now().Unix()/5)
	if key != s.probeKey {
		cfg := &config.Config{Target: target}
		s.probe, s.probeKey = disk.Probe(cfg.TargetPath(), req), key
	}
	p := s.probe
	if !p.Present {
		return sBad.Render("● not available: ") + sDim.Render(p.Reason) +
			"\n" + sDim.Render("  (fine if the disk is not connected right now)")
	}
	out := sOK.Render("● available") + sDim.Render(fmt.Sprintf("  %s free of %s", notify.HumanBytes(p.Free), notify.HumanBytes(p.Total)))
	if p.DiskID != "" {
		out += "\n" + sDim.Render("  contains a backup (disk "+p.DiskID[:min(8, len(p.DiskID))])
		if p.Account != "" {
			out += sDim.Render(", account " + p.Account)
		}
		out += sDim.Render(") — will continue incrementally")
	} else {
		out += "\n" + sDim.Render("  no backup yet — the first run will be a full backup")
	}
	return out
}

func emailInfo(s *setup) string {
	if s.value("Recipients") == "" {
		return sWarn.Render("● no recipients: notifications are disabled")
	}
	if s.value("Send via") == "graph" {
		return sDim.Render("Mail is sent from the backed-up account. Test it with `ms365backupd test-email` after saving.")
	}
	return sDim.Render("Test it with `ms365backupd test-email` after saving.")
}

// ---- field constructors ----

func text(label, help string, get func(*config.Config) string, set func(*config.Config, string) error) *field {
	return &field{label: label, help: help, kind: kText, get: get, set: set}
}

func secret(label, help string, get func(*config.Config) string, set func(*config.Config, string)) *field {
	return &field{label: label, help: help, kind: kSecret, get: get,
		set: func(c *config.Config, v string) error { set(c, v); return nil }}
}

func boolean(label, help string, get func(*config.Config) bool, set func(*config.Config, bool)) *field {
	return &field{label: label, help: help, kind: kBool,
		get: func(c *config.Config) string { return strconv.FormatBool(get(c)) },
		set: func(c *config.Config, v string) error { set(c, v == "true"); return nil }}
}

func choice(label, help string, choices []string, get func(*config.Config) string, set func(*config.Config, string)) *field {
	return &field{label: label, help: help, kind: kChoice, choices: choices, get: get,
		set: func(c *config.Config, v string) error { set(c, v); return nil }}
}

func list(label, help string, get func(*config.Config) []string, set func(*config.Config, []string)) *field {
	return &field{label: label, help: help, kind: kText,
		get: func(c *config.Config) string { return strings.Join(get(c), ", ") },
		set: func(c *config.Config, v string) error { set(c, splitList(v, ",")); return nil }}
}

func lines(label, help string, get func(*config.Config) []string, set func(*config.Config, []string)) *field {
	return &field{label: label, help: help, kind: kLines,
		get: func(c *config.Config) string { return strings.Join(get(c), "\n") },
		set: func(c *config.Config, v string) error { set(c, splitList(v, "\n")); return nil }}
}

func intField(label, help string, lo, hi int, get func(*config.Config) int, set func(*config.Config, int)) *field {
	return &field{label: label, help: help, kind: kText,
		get: func(c *config.Config) string { return strconv.Itoa(get(c)) },
		set: func(c *config.Config, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return errors.New("not a number")
			}
			if n < lo || n > hi {
				return fmt.Errorf("must be %d–%d", lo, hi)
			}
			set(c, n)
			return nil
		}}
}

func durField(label, help string, ptr func(*config.Config) *config.Duration) *field {
	return &field{label: label, help: help, kind: kText,
		get: func(c *config.Config) string { return shortDuration(ptr(c).Duration) },
		set: func(c *config.Config, v string) error {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				return errors.New("e.g. 20m, 1h30m")
			}
			ptr(c).Duration = d
			return nil
		}}
}

func withShow(f *field, show func(*setup) bool) *field { f.show = show; return f }

// ---- helpers ----

func splitList(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func looksLikeGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

func wrap(s string, width int) string {
	return lipgloss.NewStyle().Width(max(20, width)).Render(s)
}
