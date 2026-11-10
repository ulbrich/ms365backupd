package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/backup"
	"github.com/ulbrich/ms365backupd/internal/config"
)

// The "Mailboxes" step of the setup wizard edits the list of additional mailboxes in
// memory, like every other field: nothing is written before the wizard saves.

const mailboxesStep = "Mailboxes"

// mbEditor is the state of the mailbox list field.
type mbEditor struct {
	list    []config.Mailbox
	sel     int
	form    *mbForm
	confirm bool              // delete pending
	notes   map[string]string // name → last test / sign-in result (styled)
	testing map[string]bool
}

type mbTestMsg struct {
	name, text string
	err        error
}

func mailboxesJSON(l []config.Mailbox) string {
	if len(l) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func parseMailboxes(v string) []config.Mailbox {
	var l []config.Mailbox
	_ = json.Unmarshal([]byte(v), &l)
	if l == nil {
		l = []config.Mailbox{}
	}
	return l
}

func mailboxesField() *field {
	return &field{label: "Mailboxes", kind: kMailboxes,
		help: "Other Microsoft 365 accounts (graph, own sign-in) or IMAP servers, backed up to mailboxes/<name>/ " +
			"in addition to the account's own mail. Removing a mailbox keeps its files on the disk.",
		get: func(c *config.Config) string { return mailboxesJSON(c.Mailboxes) },
		set: func(c *config.Config, v string) error { c.Mailboxes = parseMailboxes(v); return nil }}
}

func (e *mbEditor) selected() *config.Mailbox {
	if e.sel < 0 || e.sel >= len(e.list) {
		return nil
	}
	return &e.list[e.sel]
}

func (e *mbEditor) find(name string) int {
	for i, m := range e.list {
		if strings.EqualFold(m.Name, name) {
			return i
		}
	}
	return -1
}

// update handles a key while the mailbox field has the focus. handled=false lets the
// wizard process it (tab, page keys, ctrl+s, esc).
func (s *setup) updateMailboxes(e *mbEditor, km tea.KeyMsg) (tea.Cmd, bool) {
	k := km.String()
	if e.form != nil {
		return s.updateMBForm(e, km), true
	}
	if e.confirm {
		e.confirm = false
		if mb := e.selected(); (k == "y" || k == "Y") && mb != nil {
			e.list = append(e.list[:e.sel:e.sel], e.list[e.sel+1:]...)
			e.sel = max(0, min(e.sel, len(e.list)-1))
		}
		return nil, true
	}
	switch k {
	case "up", "k":
		if e.sel > 0 {
			e.sel--
		}
	case "down", "j":
		if e.sel < len(e.list)-1 {
			e.sel++
		}
	case "a", "n", "+":
		e.form = newMBForm(s.inputWidth)
		return textinput.Blink, true
	case "d", "x", "delete", "-":
		if e.selected() != nil {
			e.confirm = true
		}
	case " ", "e":
		if mb := e.selected(); mb != nil {
			mb.Disabled = !mb.Disabled
		}
	case "t":
		if mb := e.selected(); mb != nil && !e.testing[mb.Name] {
			e.testing[mb.Name] = true
			e.notes[mb.Name] = sDim.Render("testing…")
			cfg, _ := s.build()
			return testMailbox(cfg, *mb), true
		}
	case "l":
		mb := e.selected()
		if mb == nil {
			break
		}
		cfg, _ := s.build()
		switch {
		case mb.Type != config.MailboxGraph:
			e.notes[mb.Name] = sDim.Render("IMAP mailboxes sign in with their password; press t to test")
		case cfg.MailboxClient(mb) == "":
			e.notes[mb.Name] = sWarn.Render("enter the client ID in step Account first")
		case cfg.MailboxTenant(mb) == "":
			e.notes[mb.Name] = sWarn.Render("enter the tenant ID in step Account first")
		default:
			cp := *mb
			s.loginReq = &cp
		}
	default:
		return nil, false
	}
	return nil, true
}

func (s *setup) updateMBForm(e *mbEditor, km tea.KeyMsg) tea.Cmd {
	f := e.form
	vis := f.visible()
	cur := vis[f.focus]
	switch km.String() {
	case "esc", "ctrl+c":
		e.form = nil
		return nil
	case "tab", "down":
		return f.move(1)
	case "shift+tab", "up":
		return f.move(-1)
	case "left", "right", " ":
		if cur.choices != nil {
			d := 1
			if km.String() == "left" {
				d = -1
			}
			cur.ci = (cur.ci + d + len(cur.choices)) % len(cur.choices)
			return nil
		}
	case "enter", "ctrl+s":
		if km.String() == "enter" && f.focus < len(vis)-1 {
			return f.move(1)
		}
		mb, err := f.mailbox()
		if err != nil {
			f.err = err.Error()
			return nil
		}
		if e.find(mb.Name) >= 0 {
			f.err = fmt.Sprintf("a mailbox named %q already exists", mb.Name)
			return nil
		}
		e.form = nil
		e.list = append(e.list, mb)
		e.sel = len(e.list) - 1
		if mb.Type == config.MailboxGraph {
			e.notes[mb.Name] = sWarn.Render("press l to sign in with that account")
		} else {
			e.notes[mb.Name] = sDim.Render("press t to test the login")
		}
		return nil
	}
	if cur.choices != nil {
		return nil
	}
	f.err = ""
	var cmd tea.Cmd
	cur.in, cmd = cur.in.Update(km)
	return cmd
}

func testMailbox(cfg *config.Config, mb config.Mailbox) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		txt, err := backup.CheckMailbox(ctx, cfg, &mb, slog.New(slog.NewTextHandler(io.Discard, nil)))
		return mbTestMsg{name: mb.Name, text: txt, err: err}
	}
}

func (e *mbEditor) testDone(msg mbTestMsg) {
	delete(e.testing, msg.name)
	if msg.err != nil {
		e.notes[msg.name] = sBad.Render("✗ " + msg.err.Error())
	} else {
		e.notes[msg.name] = sOK.Render("✓ " + msg.text)
	}
}

// mailboxState describes the sign-in state of a mailbox.
func mailboxState(cfg *config.Config, mb *config.Mailbox) string {
	if mb.Type != config.MailboxGraph {
		return sDim.Render(mb.Describe())
	}
	if acc := auth.ForMailbox(cfg, mb).Account(); acc != "" {
		return sOK.Render("signed in as " + acc)
	}
	if _, err := os.Stat(auth.MailboxTokenPath(mb.Name)); err == nil {
		return sOK.Render("signed in")
	}
	return sWarn.Render("not signed in")
}

func (s *setup) mailboxesView(e *mbEditor, w int) string {
	if e.form != nil {
		return e.form.view(w)
	}
	var b strings.Builder
	if len(e.list) == 0 {
		b.WriteString(sDim.Render("No additional mailboxes. Press a to add one.") + "\n")
	}
	for i := range e.list {
		mb := &e.list[i]
		cursor := "  "
		name := fmt.Sprintf("%-18s", truncate(mb.Name, 18))
		if i == e.sel {
			cursor, name = sTitle.Render("› "), sBold.Render(name)
		}
		on := sOK.Render("● ")
		if mb.Disabled {
			on = sDim.Render("○ ")
		}
		line := cursor + on + name + fmt.Sprintf(" %-6s ", mb.Type) + mailboxState(s.base, mb)
		if mb.Disabled {
			line += sDim.Render("  (disabled)")
		}
		b.WriteString(line + "\n")
		if n, ok := e.notes[mb.Name]; ok {
			b.WriteString("      " + truncate(n, w-12) + "\n")
		}
	}
	if e.confirm && e.selected() != nil {
		b.WriteString("\n" + sWarn.Render("Remove mailbox "+e.selected().Name+"? Its files on the disk are kept. [y/N]") + "\n")
	} else {
		b.WriteString("\n" + sDim.Render("a add · d remove · space enable/disable · l sign in (graph) · t test · ↑↓ select") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---- add form ----

// mbField is one input of the add form.
type mbField struct {
	label, help string
	in          textinput.Model
	choices     []string
	ci          int
	imap, graph bool // shown for these types
}

func (f *mbField) value() string {
	if f.choices != nil {
		return f.choices[f.ci]
	}
	return strings.TrimSpace(f.in.Value())
}

type mbForm struct {
	fields []*mbField
	focus  int
	err    string
}

func newMBForm(width int) *mbForm {
	mk := func(label, help string, imap, graph, secret bool) *mbField {
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 256
		in.Width = max(20, width)
		if secret {
			in.EchoMode = textinput.EchoPassword
			in.EchoCharacter = '•'
		}
		return &mbField{label: label, help: help, in: in, imap: imap, graph: graph}
	}
	typ := mk("Type", "graph: a Microsoft 365 account (also of another tenant) with its own sign-in · imap: any IMAP server (←/→ to change)", true, true, false)
	typ.choices = []string{config.MailboxGraph, config.MailboxIMAP}
	tlsF := mk("TLS", "tls: implicit TLS (port 993) · starttls: port 143 · none: unencrypted (←/→)", true, false, false)
	tlsF.choices = []string{"tls", "starttls", "none"}
	f := &mbForm{fields: []*mbField{
		mk("Name", "Directory name below mailboxes/ (letters, digits, . _ -).", true, true, false),
		typ,
		mk("Tenant ID", "Optional: Directory (tenant) ID of the app registration to sign in with; empty = the main tenant ID. "+
			"Needed for an account of another tenant (the app must accept that tenant).", false, true, false),
		mk("Client ID", "Optional: Application (client) ID; empty = the main client ID.", false, true, false),
		mk("Host", "IMAP server, e.g. imap.gmx.net.", true, false, false),
		tlsF,
		mk("Port", "Empty = 993 for tls, 143 otherwise.", true, false, false),
		mk("Username", "IMAP login name.", true, false, false),
		mk("Password", "Stored in the config file (mode 0600). Alternatively leave empty and use a password file.", true, false, true),
		mk("Password file", "Optional: file containing the password, e.g. ~/.config/ms365backupd/gmx-password.", true, false, false),
	}}
	f.fields[0].in.Focus()
	return f
}

func (f *mbForm) get(label string) *mbField {
	for _, x := range f.fields {
		if x.label == label {
			return x
		}
	}
	return nil
}

func (f *mbForm) visible() []*mbField {
	imap := f.get("Type").value() == config.MailboxIMAP
	var out []*mbField
	for _, x := range f.fields {
		if imap && x.imap || !imap && x.graph {
			out = append(out, x)
		}
	}
	return out
}

func (f *mbForm) mailbox() (config.Mailbox, error) {
	m := config.Mailbox{Name: f.get("Name").value(), Type: f.get("Type").value()}
	if m.Type == config.MailboxGraph {
		m.TenantID, m.ClientID = f.get("Tenant ID").value(), f.get("Client ID").value()
	} else {
		m.Host, m.Username = f.get("Host").value(), f.get("Username").value()
		m.Password = f.get("Password").in.Value()
		m.PasswordFile = f.get("Password file").value()
		if t := f.get("TLS").value(); t != "tls" {
			m.TLS = t
		}
		if p := f.get("Port").value(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				return m, fmt.Errorf("port is not a number")
			}
			m.Port = n
		}
	}
	return m, m.Validate()
}

func (f *mbForm) move(d int) tea.Cmd {
	vis := f.visible()
	f.focus = (f.focus + d + len(vis)) % len(vis)
	for _, x := range f.fields {
		x.in.Blur()
	}
	if cur := vis[f.focus]; cur.choices == nil {
		return cur.in.Focus()
	}
	return nil
}

func (f *mbForm) view(w int) string {
	vis := f.visible()
	var b strings.Builder
	b.WriteString(sBold.Render("Add mailbox") + "\n\n")
	for i, x := range vis {
		cursor := "  "
		label := fmt.Sprintf("%-15s", x.label)
		if i == f.focus {
			cursor, label = sTitle.Render("› "), sBold.Render(label)
		}
		var val string
		switch {
		case x.choices != nil && i == f.focus:
			val = sTitle.Render("‹ ") + x.value() + sTitle.Render(" ›")
		case x.choices != nil:
			val = x.value()
		case x.in.Value() == "" && i != f.focus:
			val = sDim.Render("(empty)")
		default:
			val = x.in.View()
		}
		b.WriteString(cursor + label + val + "\n")
	}
	b.WriteString("\n" + sDim.Render(wrap(vis[f.focus].help, w-8)))
	if f.err != "" {
		b.WriteString("\n" + sBad.Render("✗ "+f.err))
	}
	b.WriteString("\n\n" + sDim.Render("tab/↑↓ move · ←→ change choice · enter next / add · esc cancel"))
	return b.String()
}
