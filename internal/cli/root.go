// Package cli wires the cobra command tree.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ulbrich/ms365backupd/internal/backup"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/disk"
)

type globalFlags struct {
	configPath   string
	target       string
	exclude      []string
	clientID     string
	tenantID     string
	schedule     string
	emailTo      []string
	logLevel     string
	logFile      string
	concurrency  int
	requireMount bool
	stateDir     string
	services     []string
}

var gf globalFlags

// Execute runs the CLI.
func Execute(version string) {
	root := newRoot(version)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "ms365backupd",
		Short: "Incremental Microsoft 365 backup daemon (OneDrive, SharePoint, Mail, Contacts, Calendar) to an external disk",
		Long: `ms365backupd backs up a Microsoft 365 account to an external USB disk.

Data is written below <target>/ms365backupd/ only. Backups are incremental
(Graph delta queries); connecting a new disk automatically starts a full backup.

Settings come from ~/.config/ms365backupd/config.json and can be overridden
with command line flags.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		Args:          cobra.NoArgs,
		// Without a subcommand: first run on a terminal opens the setup screen, otherwise help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(gf.configPath); errors.Is(err, os.ErrNotExist) && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
				return runTUI(cmd, true)
			}
			return cmd.Help()
		},
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&gf.configPath, "config", "c", config.DefaultPath(), "config file")
	pf.StringVarP(&gf.target, "target", "t", "", "mount point of the backup disk (data goes to <target>/ms365backupd)")
	pf.StringArrayVarP(&gf.exclude, "exclude", "x", nil, "exclude a path/glob, e.g. onedrive/Videos or '**/node_modules' (repeatable, added to config)")
	pf.StringVar(&gf.clientID, "client-id", "", "Entra ID application (client) id")
	pf.StringVar(&gf.tenantID, "tenant-id", "", "Directory (tenant) ID of the app registration (GUID, or the primary domain e.g. contoso.onmicrosoft.com)")
	pf.StringVar(&gf.schedule, "schedule", "", "daily backup time HH:MM")
	pf.StringArrayVar(&gf.emailTo, "email-to", nil, "notification recipient (repeatable, replaces config)")
	pf.StringVar(&gf.logLevel, "log-level", "", "debug|info|warn|error")
	pf.StringVar(&gf.logFile, "log-file", "", "append logs to this file instead of stderr")
	pf.IntVar(&gf.concurrency, "concurrency", 0, "parallel downloads")
	pf.BoolVar(&gf.requireMount, "require-mount", true, "only write if target is a mounted filesystem")
	pf.StringVar(&gf.stateDir, "state-dir", "", "local state directory (default ~/.local/state/ms365backupd)")
	pf.StringSliceVar(&gf.services, "services", nil, "services to enable: onedrive,mail,contacts,calendar (replaces config)")

	_ = root.RegisterFlagCompletionFunc("config", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"json"}, cobra.ShellCompDirectiveFilterFileExt
	})
	_ = root.RegisterFlagCompletionFunc("target", completeDirs)
	_ = root.RegisterFlagCompletionFunc("state-dir", completeDirs)
	_ = root.RegisterFlagCompletionFunc("log-file", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveDefault
	})
	_ = root.RegisterFlagCompletionFunc("log-level", fixed("debug", "info", "warn", "error"))
	_ = root.RegisterFlagCompletionFunc("schedule", fixed("01:00", "02:30", "03:00", "04:00", "22:00", "23:30"))
	_ = root.RegisterFlagCompletionFunc("services", completeServices)
	_ = root.RegisterFlagCompletionFunc("exclude", completeExclude)
	_ = root.RegisterFlagCompletionFunc("concurrency", fixed("1", "2", "4", "8"))

	root.AddCommand(
		newLoginCmd(), newLogoutCmd(), newRunCmd(version), newBackupCmd(), newStatusCmd(), newTUICmd(),
		newHistoryCmd(), newReportCmd(), newTestEmailCmd(), newConfigCmd(), newVersionCmd(version), newServiceCmd(), newMailboxCmd(),
	)
	return root
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// loadConfig merges defaults < config file < flags.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	return loadConfigFrom(cmd, cmd.Flags().Changed("config"))
}

// loadConfigOptional is loadConfig but accepts a missing file.
func loadConfigOptional(cmd *cobra.Command) (*config.Config, error) {
	return loadConfigFrom(cmd, false)
}

func loadConfigFrom(cmd *cobra.Command, explicit bool) (*config.Config, error) {
	cfg, err := config.Load(gf.configPath, explicit)
	if err != nil {
		return nil, err
	}
	f := cmd.Flags()
	if f.Changed("target") {
		cfg.Target = gf.target
	}
	if f.Changed("exclude") {
		cfg.Exclude = append(cfg.Exclude, gf.exclude...)
	}
	if f.Changed("client-id") {
		cfg.ClientID = gf.clientID
	}
	if f.Changed("tenant-id") {
		cfg.TenantID = gf.tenantID
	}
	if f.Changed("schedule") {
		cfg.Schedule = gf.schedule
	}
	if f.Changed("email-to") {
		cfg.Email.To = gf.emailTo
	}
	if f.Changed("log-level") {
		cfg.LogLevel = gf.logLevel
	}
	if f.Changed("log-file") {
		cfg.LogFile = gf.logFile
	}
	if f.Changed("concurrency") {
		cfg.Concurrency = gf.concurrency
	}
	if f.Changed("require-mount") {
		cfg.RequireMount = gf.requireMount
	}
	if f.Changed("state-dir") {
		cfg.StateDir = gf.stateDir
	}
	if f.Changed("services") {
		cfg.Services = config.Services{}
		for _, s := range gf.services {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "onedrive":
				cfg.Services.OneDrive = true
			case "mail":
				cfg.Services.Mail = true
			case "contacts":
				cfg.Services.Contacts = true
			case "calendar":
				cfg.Services.Calendar = true
			default:
				return nil, fmt.Errorf("unknown service %q", s)
			}
		}
	}
	return cfg, nil
}

func newLogger(cfg *config.Config, w io.Writer) (*slog.Logger, func(), error) {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(cfg.LogLevel))
	closeFn := func() {}
	if cfg.LogFile != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, nil, err
		}
		w = f
		closeFn = func() { f.Close() }
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), closeFn, nil
}

func fixed(vals ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return vals, cobra.ShellCompDirectiveNoFileComp
	}
}

func completeDirs(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}

func completeServices(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	// Support comma separated lists: complete the last element.
	prefix := ""
	if i := strings.LastIndex(toComplete, ","); i >= 0 {
		prefix = toComplete[:i+1]
	}
	var out []string
	for _, s := range []string{"onedrive", "mail", "contacts", "calendar"} {
		out = append(out, prefix+s)
	}
	return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}

// completeExclude suggests paths from the backup on the connected disk, e.g.
// "onedrive/Pictures/", falling back to the service names.
func completeExclude(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := loadConfig(cmd)
	tops := []string{}
	for _, s := range backup.AllServices {
		tops = append(tops, s+"/")
	}
	tops = append(tops, "**/")
	if err != nil || cfg.Target == "" {
		return tops, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	root := filepath.Join(cfg.TargetPath(), disk.DataDir)
	dir := toComplete
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i+1]
	} else {
		dir = ""
	}
	if dir == "" {
		return tops, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, dir+e.Name()+"/")
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}
