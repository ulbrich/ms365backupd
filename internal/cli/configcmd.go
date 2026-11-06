package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ulbrich/ms365backupd/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect or create the configuration"}

	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a config file with defaults (merged with the given flags)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(gf.configPath); err == nil && !force {
				return fmt.Errorf("%s already exists (use --force to overwrite)", gf.configPath)
			}
			cfg, err := loadConfig(cmd)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if cfg == nil {
				cfg = config.Default()
			}
			if err := cfg.Save(gf.configPath); err != nil {
				return err
			}
			fmt.Println("Wrote", gf.configPath)
			fmt.Println("Next: set client_id, target and email.to, then run `ms365backupd login`.")
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")

	show := &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration (file + flags, secrets redacted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(cfg.Redacted())
		},
	}

	path := &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Run:   func(*cobra.Command, []string) { fmt.Println(gf.configPath) },
	}

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := cfg.Validate(true); err != nil {
				return err
			}
			fmt.Println("Configuration OK.")
			return nil
		},
	}
	cmd.AddCommand(initCmd, show, path, validate)
	return cmd
}
