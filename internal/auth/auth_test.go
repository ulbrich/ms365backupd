package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/ulbrich/ms365backupd/internal/config"
)

// TestRefreshPersistsRotatedToken checks that an expired access token is renewed
// with the refresh token and that the rotated refresh token is written to disk,
// so the daemon survives restarts without a new login.
func TestRefreshPersistsRotatedToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-1" || r.Form.Get("client_id") != "cid" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600, "token_type": "Bearer"})
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.ClientID = "cid"
	cfg.TenantID = "contoso.onmicrosoft.com"
	a := New(cfg)
	a.oc.Endpoint.TokenURL = srv.URL
	expired := &oauth2.Token{AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Hour)}
	if err := a.save(&tokenFile{Token: expired, ClientID: "cid", Account: "me@example.com"}); err != nil {
		t.Fatal(err)
	}
	ts, err := a.TokenSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-2" || calls.Load() != 1 {
		t.Fatalf("token not refreshed: %+v", tok)
	}
	tf, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	if tf.Token.RefreshToken != "rt-2" || tf.Account != "me@example.com" {
		t.Fatalf("rotated token not persisted: %+v", tf.Token)
	}
	// Second call uses the cached token, no new refresh.
	if _, err := ts.Token(); err != nil || calls.Load() != 1 {
		t.Fatalf("unexpected refresh: %v %d", err, calls.Load())
	}
}

func TestMissingScopes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config.Default()
	cfg.ClientID = "cid"
	cfg.TenantID = "contoso.onmicrosoft.com"
	a := New(cfg)
	tok := (&oauth2.Token{AccessToken: "a", RefreshToken: "r"}).WithExtra(map[string]any{
		"scope": "https://graph.microsoft.com/Files.Read.All https://graph.microsoft.com/Mail.Read User.Read Contacts.Read profile openid",
	})
	_ = a.save(&tokenFile{Token: tok, ClientID: "cid", Scopes: grantedScopes(tok, nil)})
	miss := a.MissingScopes()
	if len(miss) != 1 || miss[0] != "Calendars.Read" {
		t.Fatalf("missing = %v", miss)
	}
}
