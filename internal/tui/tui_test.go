package tui

import (
	"testing"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/daemon"
)

func dashboard(t *testing.T, cfg *config.Config) model {
	path := testEnv(t)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return model{cfg: cfg, opts: Options{ConfigPath: path, Reload: func() (*config.Config, error) { return config.Load(path, true) }},
		client: daemon.NewClient(t.TempDir()), width: 100}
}

func TestDashboardKeySOpensSetupAndEscReturns(t *testing.T) {
	m := dashboard(t, config.Default())
	nm, _ := m.Update(key("s"))
	m = nm.(model)
	if m.mode != modeSetup || m.setup == nil || m.setup.firstRun {
		t.Fatalf("s did not open setup (mode %v)", m.mode)
	}
	nm, _ = m.Update(key("esc"))
	if m = nm.(model); m.mode != modeDashboard {
		t.Fatal("esc without changes should return to the dashboard")
	}
}

func TestDashboardKeyLStartsLogin(t *testing.T) {
	m := dashboard(t, config.Default())
	nm, _ := m.Update(key("l"))
	if m = nm.(model); m.mode != modeDashboard || m.flash == "" {
		t.Fatal("l without client ID should explain instead of starting the login")
	}
	cfg := config.Default()
	cfg.TenantID = "contoso.onmicrosoft.com"
	cfg.ClientID = "11111111-2222-3333-4444-555555555555"
	m = dashboard(t, cfg)
	nm, cmd := m.Update(key("l"))
	if m = nm.(model); m.mode != modeLogin || cmd == nil {
		t.Fatal("l did not start the sign-in")
	}
	_ = m.View()
	nm, _ = m.Update(key("esc"))
	if m = nm.(model); m.mode != modeDashboard {
		t.Fatal("esc should cancel the sign-in")
	}
}
