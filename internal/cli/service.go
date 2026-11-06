package cli

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const launchdLabel = "net.ulbrich.ms365backupd"

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Generate/install a systemd (Linux, Raspberry Pi) or launchd (macOS) service",
	}
	show := &cobra.Command{
		Use:   "print",
		Short: "Print the service definition for this OS",
		RunE: func(*cobra.Command, []string) error {
			_, content, err := serviceFile()
			if err != nil {
				return err
			}
			fmt.Print(content)
			return nil
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the service definition (macOS: LaunchAgent; Linux: prints sudo steps)",
		RunE: func(*cobra.Command, []string) error {
			path, content, err := serviceFile()
			if err != nil {
				return err
			}
			if runtime.GOOS == "linux" {
				tmp := filepath.Join(os.TempDir(), "ms365backupd.service")
				if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
					return err
				}
				fmt.Printf("Wrote %s. Install it with:\n\n  sudo install -m644 %s %s\n  sudo systemctl daemon-reload\n  sudo systemctl enable --now ms365backupd\n  journalctl -u ms365backupd -f\n", tmp, tmp, path)
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}
			uid := os.Getuid()
			fmt.Printf("Wrote %s. Load it with:\n\n  launchctl bootstrap gui/%d %s\n  launchctl kickstart -k gui/%d/%s\n", path, uid, path, uid, launchdLabel)
			return nil
		},
	}
	cmd.AddCommand(show, install)
	return cmd
}

func serviceFile() (path, content string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	u, err := user.Current()
	if err != nil {
		return "", "", err
	}
	args := []string{exe}
	if gf.configPath != "" {
		args = append(args, "--config", gf.configPath)
	}
	args = append(args, "run")
	switch runtime.GOOS {
	case "linux":
		return "/etc/systemd/system/ms365backupd.service", fmt.Sprintf(`[Unit]
Description=Microsoft 365 backup daemon
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=%s
Environment=HOME=%s
ExecStart=%s
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=30
Nice=10
IOSchedulingClass=idle
TimeoutStopSec=60
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, u.Username, u.HomeDir, strings.Join(quoteAll(args), " ")), nil
	case "darwin":
		var b strings.Builder
		for _, a := range args {
			fmt.Fprintf(&b, "\t\t<string>%s</string>\n", xmlEscape(a))
		}
		logDir := filepath.Join(u.HomeDir, "Library", "Logs")
		return filepath.Join(u.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist"), fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>LowPriorityIO</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, launchdLabel, b.String(), xmlEscape(filepath.Join(logDir, "ms365backupd.log")), xmlEscape(filepath.Join(logDir, "ms365backupd.log"))), nil
	}
	return "", "", fmt.Errorf("no service template for %s", runtime.GOOS)
}

func quoteAll(a []string) []string {
	out := make([]string, len(a))
	for i, s := range a {
		if strings.ContainsAny(s, " \t\"'\\") {
			s = `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
		}
		out[i] = s
	}
	return out
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
