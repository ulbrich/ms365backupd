package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/backup"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/daemon"
)

func newMailboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "mailbox",
		Aliases: []string{"mailboxes"},
		Short:   "Manage additional mailboxes (other Microsoft 365 accounts or IMAP)",
		Long: "Additional mailboxes are backed up to <target>/ms365backupd/mailboxes/<name>/, on top of\n" +
			"the account's own mail. Type graph signs in to any Microsoft 365 account with its own\n" +
			"device code login; type imap uses host, username and password.",
	}
	cmd.AddCommand(newMailboxListCmd(), newMailboxAddCmd(), newMailboxRemoveCmd(), newMailboxLoginCmd(),
		newMailboxLogoutCmd(), newMailboxTestCmd(), newMailboxEnableCmd(true), newMailboxEnableCmd(false))
	return cmd
}

// completeMailboxes completes the names of configured mailboxes.
func completeMailboxes(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	cfg, err := config.Load(gf.configPath, false)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, m := range cfg.Mailboxes {
		out = append(out, m.Name)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// editMailboxes changes the config file and asks a running daemon to reload it.
func editMailboxes(fn func(*config.Config) error) (*config.Config, error) {
	cfg, err := config.Edit(gf.configPath, fn)
	if err != nil {
		return nil, err
	}
	reloadDaemon(cfg)
	return cfg, nil
}

func reloadDaemon(cfg *config.Config) {
	dir := cfg.StateDir
	if gf.stateDir != "" {
		dir = gf.stateDir
	}
	c := *cfg
	c.StateDir = dir
	deferred, err := daemon.NewClient(c.EffectiveStateDir()).Reload()
	switch {
	case errors.Is(err, daemon.ErrNotRunning):
	case err != nil:
		fmt.Fprintln(os.Stderr, "Note: could not reload the daemon:", err)
	case deferred:
		fmt.Println("The daemon applies the change after the running backup.")
	default:
		fmt.Println("Daemon configuration reloaded.")
	}
}

// mailboxArg loads the config (with flags) and returns the mailbox named by args[0].
func mailboxArg(cmd *cobra.Command, args []string) (*config.Config, *config.Mailbox, error) {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return nil, nil, err
	}
	i := cfg.FindMailbox(args[0])
	if i < 0 {
		return nil, nil, fmt.Errorf("no mailbox named %q (see `ms365backupd mailbox list`)", args[0])
	}
	return cfg, &cfg.Mailboxes[i], nil
}

// mailboxState describes the sign-in state of a mailbox for listings.
func mailboxState(cfg *config.Config, m *config.Mailbox) string {
	var s string
	switch m.Type {
	case config.MailboxGraph:
		if acc := auth.ForMailbox(cfg, m).Account(); acc != "" {
			s = "signed in as " + acc
		} else if _, err := os.Stat(auth.MailboxTokenPath(m.Name)); err == nil {
			s = "signed in"
		} else {
			s = "NOT SIGNED IN"
		}
	default:
		s = m.Describe()
	}
	if m.Disabled {
		s += " (disabled)"
	}
	return s
}

func newMailboxListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List additional mailboxes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfigOptional(cmd)
			if err != nil {
				return err
			}
			if len(cfg.Mailboxes) == 0 {
				fmt.Println("No additional mailboxes. Add one with `ms365backupd mailbox add`.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tSTATE\tDIRECTORY")
			for i := range cfg.Mailboxes {
				m := &cfg.Mailboxes[i]
				fmt.Fprintf(w, "%s\t%s\t%s\t%s/\n", m.Name, m.Type, mailboxState(cfg, m), backup.MailboxDir(m.Name))
			}
			return w.Flush()
		},
	}
}

func newMailboxAddCmd() *cobra.Command {
	var m config.Mailbox
	var login, test, askPassword bool
	cmd := &cobra.Command{
		Use:   "add <name> --type graph|imap [flags]",
		Short: "Add an additional mailbox",
		Example: "  ms365backupd mailbox add shared --type graph --login\n" +
			"  ms365backupd mailbox add gmx --type imap --host imap.gmx.net --username me@gmx.de --password-file ~/.config/ms365backupd/gmx.pw",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m.Name = args[0]
			m.Type = strings.ToLower(m.Type)
			if m.Type == config.MailboxIMAP && m.Password == "" && m.PasswordFile == "" {
				if !askPassword && !isTerminal(os.Stdin) {
					return errors.New("imap needs --password, --password-file or --ask-password")
				}
				pw, err := readPassword("IMAP password for " + m.Username + ": ")
				if err != nil {
					return err
				}
				m.Password = pw
			}
			if m.Type == config.MailboxGraph && (m.Host != "" || m.Username != "" || m.Password != "" || m.PasswordFile != "") {
				return errors.New("--host/--username/--password only apply to --type imap")
			}
			if m.Type == config.MailboxIMAP && (m.TenantID != "" || m.ClientID != "") {
				return errors.New("--tenant-id/--client-id only apply to --type graph")
			}
			if err := m.Validate(); err != nil {
				return err
			}
			if test && m.Type == config.MailboxIMAP {
				cfg, err := loadConfigOptional(cmd)
				if err != nil {
					return err
				}
				if err := runMailboxTest(cfg, &m); err != nil {
					return fmt.Errorf("not added: %w", err)
				}
			}
			cfg, err := editMailboxes(func(c *config.Config) error { return c.AddMailbox(m) })
			if err != nil {
				return err
			}
			fmt.Printf("Added mailbox %s (%s), backed up to %s/.\n", m.Name, m.Describe(), backup.MailboxDir(m.Name))
			if m.Password != "" {
				fmt.Println("The password is stored in the config file (mode 0600); --password-file keeps it separate.")
			}
			if m.Type == config.MailboxGraph {
				if !login {
					fmt.Printf("Sign in with: ms365backupd mailbox login %s\n", m.Name)
					return nil
				}
				return mailboxLogin(cfg, &cfg.Mailboxes[cfg.FindMailbox(m.Name)])
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&m.Type, "type", "", "graph (Microsoft 365 account, own sign-in) or imap")
	f.StringVar(&m.TenantID, "tenant-id", "", "graph: Directory (tenant) ID of the app registration (default: the main tenant_id)")
	f.StringVar(&m.ClientID, "client-id", "", "graph: application (client) ID (default: the main client_id)")
	f.BoolVar(&login, "login", false, "graph: sign in right away")
	f.StringVar(&m.Host, "host", "", "imap: server host name")
	f.IntVar(&m.Port, "port", 0, "imap: port (default 993 for tls, 143 otherwise)")
	f.StringVar(&m.Username, "username", "", "imap: login name")
	f.StringVar(&m.Password, "password", "", "imap: password (prefer --password-file or the prompt; shows up in the shell history)")
	f.StringVar(&m.PasswordFile, "password-file", "", "imap: file containing the password")
	f.BoolVar(&askPassword, "ask-password", false, "imap: read the password from stdin")
	f.StringVar(&m.TLS, "tls", "", "imap: tls (default), starttls or none")
	f.BoolVar(&test, "test", true, "imap: check the login before adding")
	_ = cmd.MarkFlagRequired("type")
	_ = cmd.RegisterFlagCompletionFunc("type", fixed(config.MailboxGraph, config.MailboxIMAP))
	_ = cmd.RegisterFlagCompletionFunc("tls", fixed("tls", "starttls", "none"))
	return cmd
}

func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if isTerminal(os.Stdin) {
		b, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func newMailboxRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "remove <name>",
		Aliases:           []string{"rm"},
		Short:             "Remove an additional mailbox (its backed-up files stay on the disk)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMailboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var removed config.Mailbox
			cfg, err := editMailboxes(func(c *config.Config) error {
				var err error
				removed, err = c.RemoveMailbox(args[0])
				return err
			})
			if err != nil {
				return err
			}
			if removed.Type == config.MailboxGraph {
				if err := auth.ForMailbox(cfg, &removed).Logout(); err != nil {
					fmt.Fprintln(os.Stderr, "Note: could not delete the token file:", err)
				}
			}
			fmt.Printf("Removed mailbox %s. Backed-up files in %s/ are kept.\n", removed.Name, backup.MailboxDir(removed.Name))
			return nil
		},
	}
}

func mailboxLogin(cfg *config.Config, m *config.Mailbox) error {
	if m.Type != config.MailboxGraph {
		return fmt.Errorf("mailbox %s is of type %s; only graph mailboxes sign in", m.Name, m.Type)
	}
	if cfg.MailboxClient(m) == "" {
		return errors.New("no client_id: set client_id in the config or pass --client-id to `mailbox add`")
	}
	ctx, cancel := signalContext()
	defer cancel()
	a := auth.ForMailbox(cfg, m)
	fmt.Printf("Signing in mailbox %s. Use the account whose mail should be backed up.\n", m.Name)
	fmt.Println("Requesting permissions:", strings.Join(a.Scopes(), " "))
	acc, err := a.Login(ctx, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("\nMailbox %s signed in as %s. Token stored in %s.\n", m.Name, acc, a.Path())
	if miss := a.MissingScopes(); len(miss) > 0 {
		fmt.Printf("WARNING: these permissions were not granted: %s\n", strings.Join(miss, " "))
	}
	reloadDaemon(cfg)
	return nil
}

func newMailboxLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "login <name>",
		Short:             "Sign in a graph mailbox (device code flow)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMailboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, m, err := mailboxArg(cmd, args)
			if err != nil {
				return err
			}
			return mailboxLogin(cfg, m)
		},
	}
}

func newMailboxLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "logout <name>",
		Short:             "Delete the stored token of a graph mailbox",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMailboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, m, err := mailboxArg(cmd, args)
			if err != nil {
				return err
			}
			if m.Type != config.MailboxGraph {
				return fmt.Errorf("mailbox %s is of type %s and has no sign-in", m.Name, m.Type)
			}
			if err := auth.ForMailbox(cfg, m).Logout(); err != nil {
				return err
			}
			fmt.Printf("Mailbox %s logged out.\n", m.Name)
			return nil
		},
	}
}

func runMailboxTest(cfg *config.Config, m *config.Mailbox) error {
	ctx, cancel := signalContext()
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel2()
	log, closeLog, err := newLogger(cfg, os.Stderr)
	if err != nil {
		return err
	}
	defer closeLog()
	fmt.Printf("Testing mailbox %s (%s)...\n", m.Name, m.Describe())
	msg, err := backup.CheckMailbox(ctx, cfg, m, log)
	if err != nil {
		return err
	}
	fmt.Println("OK:", msg)
	return nil
}

func newMailboxTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "test <name>",
		Short:             "Check that a mailbox can be reached and signed in to",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMailboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, m, err := mailboxArg(cmd, args)
			if err != nil {
				return err
			}
			return runMailboxTest(cfg, m)
		},
	}
}

func newMailboxEnableCmd(enable bool) *cobra.Command {
	use, short := "enable", "Include a disabled mailbox in backups again"
	if !enable {
		use, short = "disable", "Skip a mailbox in backups (keeps its settings and sign-in)"
	}
	return &cobra.Command{
		Use:               use + " <name>",
		Short:             short,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeMailboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := editMailboxes(func(c *config.Config) error { return c.SetMailboxDisabled(args[0], !enable) }); err != nil {
				return err
			}
			fmt.Printf("Mailbox %s %sd.\n", args[0], use)
			return nil
		},
	}
}
