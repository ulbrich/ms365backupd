package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/ulbrich/ms365backupd/internal/auth"
	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/status"
)

func writeToken(t *testing.T, cfg *config.Config, saved time.Time) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"token":     &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)},
		"client_id": cfg.ClientID, "account": "me@example.com", "saved": saved,
	})
	if err := os.MkdirAll(filepath.Dir(auth.TokenPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth.TokenPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// After a run failed with "needs login", signing in (new token) and reloading must
// clear the login problem; a reload without a new sign-in must keep it.
func TestReloadClearsLoginProblemAfterSignIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	cfg := config.Default()
	cfg.ClientID = "11111111-2222-3333-4444-555555555555"
	cfg.Target = dir
	cfg.TenantID = "contoso.onmicrosoft.com"
	d := New(cfg, "", "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	d.Reload = func() (*config.Config, error) { return cfg, nil }

	failed := time.Now().Add(-time.Minute)
	d.last = &status.RunResult{Finished: failed, Status: status.StatusFailed, NeedsLogin: true, Error: "not logged in"}
	d.authErr = "not logged in"

	writeToken(t, cfg, failed.Add(-time.Hour)) // stale token from before the failure
	if err := d.applyReload(); err != nil {
		t.Fatal(err)
	}
	if d.authErr == "" {
		t.Fatal("login problem cleared without a new sign-in")
	}

	writeToken(t, cfg, time.Now()) // signed in after the failure
	if err := d.applyReload(); err != nil {
		t.Fatal(err)
	}
	if d.authErr != "" {
		t.Fatalf("login problem not cleared after sign-in: %q", d.authErr)
	}
}
