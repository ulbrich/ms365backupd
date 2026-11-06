package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/backup"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/daemon"
	"github.com/ulbrich/ms365backupd/internal/disk"
	"github.com/ulbrich/ms365backupd/internal/graph"
	"github.com/ulbrich/ms365backupd/internal/history"
	"github.com/ulbrich/ms365backupd/internal/notify"
	"github.com/ulbrich/ms365backupd/internal/status"
	"github.com/ulbrich/ms365backupd/internal/tui"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func graphFor(ctx context.Context, cfg *config.Config) (*graph.Client, error) {
	ts, err := auth.New(cfg).TokenSource(ctx)
	if err != nil {
		return nil, err
	}
	log, _, _ := newLogger(cfg, os.Stderr)
	return graph.New(cfg.GraphURL, ts, log), nil
}

func newLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Sign in to Microsoft 365 (device code flow, works headless over SSH)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if cfg.ClientID == "" {
				return errors.New("client_id is not configured: set it in the config file or pass --client-id (see README)")
			}
			ctx, cancel := signalContext()
			defer cancel()
			a := auth.New(cfg)
			fmt.Println("Requesting permissions:", strings.Join(cfg.Scopes(), " "))
			acc, err := a.Login(ctx, os.Stdout)
			if err != nil {
				return err
			}
			fmt.Printf("\nSigned in as %s. Token stored in %s (renewed automatically).\n", acc, auth.TokenPath())
			if miss := a.MissingScopes(); len(miss) > 0 {
				fmt.Printf("WARNING: these permissions were not granted: %s\n"+
					"Add them to the app registration (API permissions) or disable the matching services.\n", strings.Join(miss, " "))
			}
			return nil
		},
	}
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete the stored refresh token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := auth.New(cfg).Logout(); err != nil {
				return err
			}
			fmt.Println("Logged out.")
			return nil
		},
	}
}

func newRunCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the daemon (foreground; use systemd/launchd to keep it running)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.Validate(true); err != nil {
				return err
			}
			log, closeLog, err := newLogger(cfg, os.Stderr)
			if err != nil {
				return err
			}
			defer closeLog()
			ctx, cancel := signalContext()
			defer cancel()
			d := daemon.New(cfg, gf.configPath, version, log)
			d.Reload = func() (*config.Config, error) { return loadConfig(cmd) } // file + same flags
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			go func() {
				for {
					select {
					case <-hup:
						log.Info("SIGHUP: reloading configuration")
						_ = d.RequestReload(ctx)
					case <-ctx.Done():
						return
					}
				}
			}()
			return d.Run(ctx)
		},
	}
}

func newBackupCmd() *cobra.Command {
	var only []string
	var full, foreground bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Run a backup now (via the daemon if it is running, otherwise in the foreground)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.Validate(true); err != nil {
				return err
			}
			if !foreground {
				err := daemon.NewClient(cfg.EffectiveStateDir()).Backup(only, full)
				if err == nil {
					fmt.Println("Backup started by the daemon. Follow it with `ms365backupd tui` or `ms365backupd status`.")
					return nil
				}
				if !errors.Is(err, daemon.ErrNotRunning) {
					return err
				}
			}
			return runForeground(cfg, only, full)
		},
	}
	cmd.Flags().StringSliceVarP(&only, "service", "s", nil, "only these services (onedrive,sharepoint,mail,contacts,calendar)")
	cmd.Flags().BoolVar(&full, "full", false, "full resync: re-check every item (existing files are not downloaded again)")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "never delegate to the daemon")
	_ = cmd.RegisterFlagCompletionFunc("service", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return backup.AllServices, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func runForeground(cfg *config.Config, only []string, full bool) error {
	log, closeLog, err := newLogger(cfg, os.Stderr)
	if err != nil {
		return err
	}
	defer closeLog()
	ctx, cancel := signalContext()
	defer cancel()
	dir := cfg.EffectiveStateDir()
	unlock, err := daemon.Lock(dir, "run.lock")
	if err != nil {
		return fmt.Errorf("cannot start: %w", err)
	}
	defer unlock()
	g, err := graphFor(ctx, cfg)
	if err != nil {
		return err
	}
	eng, err := backup.New(backup.Options{Config: cfg, Graph: g, StateDir: dir, Log: log, Trigger: daemon.TriggerManual, Only: only, FullResync: full})
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				p := eng.Progress()
				fmt.Fprintf(os.Stderr, "… %s %s | new %d upd %d del %d err %d | %s\n", p.Service, p.Phase,
					p.Totals.New, p.Totals.Updated, p.Totals.Deleted, p.Totals.Errors, notify.HumanBytes(uint64(p.Totals.Bytes)))
			}
		}
	}()
	res := eng.Run(ctx)
	close(stop)
	_ = history.Open(dir).Append(res)
	printResult(res)
	if res.Status == status.StatusFailed {
		return errors.New(res.Error)
	}
	if res.Status == status.StatusPartial {
		os.Exit(2)
	}
	return nil
}

func printResult(r *status.RunResult) {
	fmt.Printf("\nBackup %s in %s\n", strings.ToUpper(r.Status), r.Duration().Round(time.Second))
	if r.Error != "" {
		fmt.Println("  ", r.Error)
	}
	for _, k := range backup.AllServices {
		c, ok := r.Services[k]
		if !ok {
			continue
		}
		fmt.Printf("  %-10s new %-6d updated %-6d unchanged %-7d deleted %-5d moved %-5d skipped %-5d errors %-4d %s\n",
			k, c.New, c.Updated, c.Unchanged, c.Deleted, c.Moved, c.Skipped, c.Errors, notify.HumanBytes(uint64(c.Bytes)))
	}
	for _, s := range r.ErrorSamples {
		fmt.Println("  !", s)
	}
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon, disk and last backup status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			st, err := daemon.NewClient(cfg.EffectiveStateDir()).Status()
			running := err == nil
			if !running {
				info := disk.Probe(cfg.TargetPath(), cfg.RequireMount)
				st = &status.Daemon{Target: cfg.TargetPath(), Disk: status.DiskState{Path: info.Path, Present: info.Present,
					Reason: info.Reason, DiskID: info.DiskID, Free: info.Free, Total: info.Total}, Account: auth.New(cfg).Account()}
				if r, _ := history.Open(cfg.EffectiveStateDir()).Last(1); len(r) > 0 {
					st.Last = &r[0]
				}
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			if running {
				fmt.Printf("daemon:    running (pid %d, %s, since %s)\n", st.PID, st.Version, st.Started.Format(time.RFC1123))
			} else {
				fmt.Println("daemon:    not running")
			}
			fmt.Printf("account:   %s\n", orDash(st.Account))
			if st.AuthError != "" {
				fmt.Printf("auth:      PROBLEM: %s\n", st.AuthError)
			}
			d := st.Disk
			if d.Present {
				fmt.Printf("disk:      %s (id %s) %s free of %s\n", d.Path, orDash(d.DiskID), notify.HumanBytes(d.Free), notify.HumanBytes(d.Total))
			} else {
				fmt.Printf("disk:      %s NOT AVAILABLE (%s)\n", d.Path, d.Reason)
			}
			if !st.NextRun.IsZero() {
				fmt.Printf("next run:  %s (%s)\n", st.NextRun.Format("Mon Jan 2 15:04"), st.NextRunWhy)
			}
			if p := st.Running; p != nil {
				fmt.Printf("running:   %s · %s · new %d upd %d del %d err %d · %s\n", p.Service, p.Phase,
					p.Totals.New, p.Totals.Updated, p.Totals.Deleted, p.Totals.Errors, notify.HumanBytes(uint64(p.Totals.Bytes)))
			}
			if l := st.Last; l != nil {
				fmt.Printf("last run:  %s %s (%s, %s)\n", l.Started.Format("Mon Jan 2 15:04"), strings.ToUpper(l.Status), l.Trigger, l.Duration().Round(time.Second))
				if l.Error != "" {
					fmt.Printf("           %s\n", l.Error)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine readable output")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newTUICmd() *cobra.Command {
	var setup bool
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive terminal dashboard and setup (opens setup when no config file exists)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runTUI(cmd, setup) },
	}
	cmd.Flags().BoolVar(&setup, "setup", false, "open the setup screen")
	return cmd
}

// runTUI starts the TUI; it opens the setup screen if asked or if there is no config file yet.
func runTUI(cmd *cobra.Command, setup bool) error {
	if _, err := os.Stat(gf.configPath); errors.Is(err, os.ErrNotExist) {
		setup = true
	}
	// A missing file is fine here even with an explicit --config: setup creates it.
	cfg, err := loadConfigOptional(cmd)
	if err != nil {
		return err
	}
	return tui.Run(cfg, tui.Options{
		ConfigPath: gf.configPath,
		Setup:      setup,
		Reload:     func() (*config.Config, error) { return loadConfig(cmd) },
	})
}

func newHistoryCmd() *cobra.Command {
	var n int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List recent backup runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			runs, err := history.Open(cfg.EffectiveStateDir()).Last(n)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(runs)
			}
			fmt.Printf("%-17s %-15s %-8s %9s %7s %7s %7s %5s %10s  %s\n", "STARTED", "TRIGGER", "STATUS", "DURATION", "NEW", "UPD", "DEL", "ERR", "DOWNLOAD", "ERROR")
			for _, r := range runs {
				fmt.Printf("%-17s %-15s %-8s %9s %7d %7d %7d %5d %10s  %s\n", r.Started.Format("2006-01-02 15:04"), r.Trigger, r.Status,
					r.Duration().Round(time.Second), r.Totals.New, r.Totals.Updated, r.Totals.Deleted, r.Totals.Errors,
					notify.HumanBytes(uint64(r.Totals.Bytes)), r.Error)
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "number", "n", 20, "number of runs")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine readable output")
	return cmd
}

func newReportCmd() *cobra.Command {
	var print bool
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Send (or --print) the weekly statistics report now",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if !print {
				err := daemon.NewClient(cfg.EffectiveStateDir()).Report()
				if err == nil {
					fmt.Println("Weekly report sent.")
					return nil
				}
				if !errors.Is(err, daemon.ErrNotRunning) {
					return err
				}
			}
			info := disk.Probe(cfg.TargetPath(), cfg.RequireMount)
			to := time.Now()
			from := to.AddDate(0, 0, -7)
			runs, _ := history.Open(cfg.EffectiveStateDir()).Since(from)
			size, files := daemon.DataSize(info)
			subj, body := notify.WeeklyMessage(runs, from, to, status.DiskState{Path: info.Path, Present: info.Present, Reason: info.Reason,
				DiskID: info.DiskID, Free: info.Free, Total: info.Total}, size, files)
			if print {
				fmt.Println("Subject:", subj)
				fmt.Println()
				fmt.Println(body)
				return nil
			}
			return sendMail(cfg, subj, body)
		},
	}
	cmd.Flags().BoolVar(&print, "print", false, "print the report instead of mailing it")
	return cmd
}

func sendMail(cfg *config.Config, subj, body string) error {
	if err := cfg.Validate(false); err != nil {
		return err
	}
	s := notify.New(cfg, func(ctx context.Context) (*graph.Client, error) { return graphFor(ctx, cfg) })
	if s == nil {
		return errors.New("email is not configured (email.to)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.Send(ctx, subj, body); err != nil {
		return err
	}
	fmt.Printf("Sent to %s via %s.\n", strings.Join(cfg.Email.To, ", "), cfg.EmailVia())
	return nil
}

func newTestEmailCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-email",
		Short: "Send a test notification",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			h, _ := os.Hostname()
			return sendMail(cfg, "[ms365backupd] Test notification from "+h,
				"This is a test message from ms365backupd.\n\nFailure alerts and weekly reports will be delivered like this one.\n")
		},
	}
}

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(*cobra.Command, []string) { fmt.Println("ms365backupd", version) },
	}
}
