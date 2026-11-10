// Package auth implements the OAuth2 device-code login against Microsoft Entra ID
// and persists (rotating) refresh tokens in ~/.config/ms365backupd/token.json.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ulbrich/ms365backupd/internal/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"
)

// ErrNotLoggedIn is returned when no token is stored.
var ErrNotLoggedIn = errors.New("not logged in")

type tokenFile struct {
	Token    *oauth2.Token `json:"token"`
	ClientID string        `json:"client_id"`
	TenantID string        `json:"tenant_id"`
	Scopes   []string      `json:"scopes"`
	Account  string        `json:"account,omitempty"`
	Saved    time.Time     `json:"saved"`
}

// Authenticator manages tokens.
type Authenticator struct {
	oc       *oauth2.Config
	path     string
	tenantID string
	clientID string
	graphURL string
	// LoginCmd is the command that signs in again (shown in error messages).
	LoginCmd string
}

// New builds the authenticator of the main account.
func New(cfg *config.Config) *Authenticator {
	return newAuth(cfg.TenantID, cfg.ClientID, cfg.GraphURL, cfg.Scopes(), TokenPath(), "ms365backupd login")
}

// ForMailbox builds the authenticator of an additional Graph mailbox. It has its own
// token file and only asks for read access to mail.
func ForMailbox(cfg *config.Config, m *config.Mailbox) *Authenticator {
	return newAuth(cfg.MailboxTenant(m), cfg.MailboxClient(m), cfg.GraphURL, config.MailboxScopes,
		MailboxTokenPath(m.Name), "ms365backupd mailbox login "+m.Name)
}

func newAuth(tenantID, clientID, graphURL string, scopes []string, path, loginCmd string) *Authenticator {
	ep := microsoft.AzureADEndpoint(tenantID)
	ep.AuthStyle = oauth2.AuthStyleInParams // public client: no secret, client_id in body
	return &Authenticator{
		tenantID: tenantID, clientID: clientID, graphURL: graphURL, path: path, LoginCmd: loginCmd,
		oc: &oauth2.Config{ClientID: clientID, Endpoint: ep, Scopes: scopes},
	}
}

// TokenPath is the token file location.
func TokenPath() string { return filepath.Join(config.Dir(), "token.json") }

// MailboxTokenPath is the token file of an additional Graph mailbox.
func MailboxTokenPath(name string) string {
	return filepath.Join(config.Dir(), "mailboxes", strings.ToLower(name)+".token.json")
}

// Scopes returns the requested permissions.
func (a *Authenticator) Scopes() []string { return a.oc.Scopes }

// Path returns the token file location.
func (a *Authenticator) Path() string { return a.path }

func (a *Authenticator) notLoggedIn() error {
	return fmt.Errorf("%w: run `%s`", ErrNotLoggedIn, a.LoginCmd)
}

func httpCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 60 * time.Second})
}

// DeviceLogin is a pending device-code sign-in: show URI and Code to the user, then call Wait.
type DeviceLogin struct {
	URI    string
	Code   string
	Expiry time.Time
	a      *Authenticator
	da     *oauth2.DeviceAuthResponse
}

// StartLogin requests a device code.
func (a *Authenticator) StartLogin(ctx context.Context) (*DeviceLogin, error) {
	if a.tenantID == "" {
		return nil, errors.New("tenant_id not configured: set the Directory (tenant) ID of your app registration (see README)")
	}
	if a.clientID == "" {
		return nil, errors.New("client_id not configured (see README: app registration)")
	}
	da, err := a.oc.DeviceAuth(httpCtx(ctx))
	if err != nil {
		return nil, fmt.Errorf("device authorization: %w", err)
	}
	return &DeviceLogin{URI: da.VerificationURI, Code: da.UserCode, Expiry: da.Expiry, a: a, da: da}, nil
}

// Wait blocks until the user has signed in (or the code expires / ctx ends), stores the
// token and returns the signed-in account (user principal name).
func (d *DeviceLogin) Wait(ctx context.Context) (string, error) {
	a := d.a
	tok, err := a.oc.DeviceAccessToken(httpCtx(ctx), d.da)
	if err != nil {
		return "", fmt.Errorf("device token: %w", err)
	}
	tf := &tokenFile{Token: tok, ClientID: a.clientID, TenantID: a.tenantID, Scopes: grantedScopes(tok, a.oc.Scopes)}
	if err := a.save(tf); err != nil {
		return "", err
	}
	acc, err := a.fetchAccount(ctx, tok)
	if err != nil {
		return "", fmt.Errorf("signed in, but reading the profile failed: %w", err)
	}
	tf.Account = acc
	return acc, a.save(tf)
}

func (a *Authenticator) fetchAccount(ctx context.Context, tok *oauth2.Token) (string, error) {
	base := a.graphURL
	if base == "" {
		base = "https://graph.microsoft.com/v1.0"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+"/me?$select=userPrincipalName,mail", nil)
	if err != nil {
		return "", err
	}
	tok.SetAuthHeader(req)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /me: %s", resp.Status)
	}
	var me struct {
		UPN  string `json:"userPrincipalName"`
		Mail string `json:"mail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return "", err
	}
	if me.UPN == "" {
		me.UPN = me.Mail
	}
	return strings.ToLower(me.UPN), nil
}

// Login runs the device-code flow on a terminal, printing instructions to out.
func (a *Authenticator) Login(ctx context.Context, out io.Writer) (string, error) {
	dl, err := a.StartLogin(ctx)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "\nTo sign in, open %s and enter the code:\n\n    %s\n\nWaiting for authorization (expires %s)...\n",
		dl.URI, dl.Code, dl.Expiry.Format("15:04:05"))
	return dl.Wait(ctx)
}

// SetAccount records the signed-in account name in the token file.
func (a *Authenticator) SetAccount(account string) error {
	tf, err := a.load()
	if err != nil {
		return err
	}
	tf.Account = account
	return a.save(tf)
}

// Account returns the stored account name, if any.
func (a *Authenticator) Account() string {
	tf, err := a.load()
	if err != nil {
		return ""
	}
	return tf.Account
}

// SavedAt returns when the token was last stored (sign-in or refresh); zero if none.
func (a *Authenticator) SavedAt() time.Time {
	tf, err := a.load()
	if err != nil {
		return time.Time{}
	}
	return tf.Saved
}

// Logout deletes the stored token.
func (a *Authenticator) Logout() error {
	err := os.Remove(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a *Authenticator) load() (*tokenFile, error) {
	b, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, a.notLoggedIn()
	}
	if err != nil {
		return nil, err
	}
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("corrupt token file %s: %w", a.path, err)
	}
	if tf.Token == nil || tf.Token.RefreshToken == "" {
		return nil, a.notLoggedIn()
	}
	return &tf, nil
}

func (a *Authenticator) save(tf *tokenFile) error {
	tf.Saved = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// TokenSource returns a refreshing token source that persists rotated tokens.
func (a *Authenticator) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	tf, err := a.load()
	if err != nil {
		return nil, err
	}
	if a.tenantID == "" {
		return nil, errors.New("tenant_id not configured: set the Directory (tenant) ID of your app registration")
	}
	if tf.ClientID != "" && tf.ClientID != a.clientID {
		return nil, fmt.Errorf("stored token belongs to client_id %s; run `%s` again", tf.ClientID, a.LoginCmd)
	}
	base := a.oc.TokenSource(httpCtx(context.WithoutCancel(ctx)), tf.Token)
	return &savingSource{base: base, last: tf.Token.AccessToken, tf: tf, a: a}, nil
}

// grantedScopes reads the scopes Entra ID actually granted (the "scope" field of the
// token response), falling back to the requested ones.
func grantedScopes(tok *oauth2.Token, requested []string) []string {
	raw, _ := tok.Extra("scope").(string)
	if raw == "" {
		return requested
	}
	var out []string
	for _, s := range strings.Fields(raw) {
		out = append(out, strings.TrimPrefix(s, "https://graph.microsoft.com/"))
	}
	return out
}

// MissingScopes lists configured scopes that were not granted at login.
func (a *Authenticator) MissingScopes() []string {
	tf, err := a.load()
	if err != nil {
		return nil
	}
	have := map[string]bool{"offline_access": true} // never echoed back by Entra ID
	for _, s := range tf.Scopes {
		have[strings.ToLower(s)] = true
	}
	var miss []string
	for _, s := range a.oc.Scopes {
		if !have[strings.ToLower(s)] {
			miss = append(miss, s)
		}
	}
	return miss
}

type savingSource struct {
	mu   sync.Mutex
	base oauth2.TokenSource
	last string
	tf   *tokenFile
	a    *Authenticator
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		cp := *s.tf
		cp.Token = t
		_ = s.a.save(&cp) // best effort; old refresh token usually stays valid for a while
	}
	return t, nil
}
