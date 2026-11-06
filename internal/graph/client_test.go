package graph

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

type flakyTS struct {
	fails int
	err   error
	n     int
}

func (f *flakyTS) Token() (*oauth2.Token, error) {
	f.n++
	if f.n <= f.fails {
		return nil, f.err
	}
	return &oauth2.Token{AccessToken: "ok"}, nil
}

func newTestClient(t *testing.T, ts oauth2.TokenSource) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"1"}`)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, ts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.MaxRetries = 2
	return c
}

func TestTransientTokenErrorIsRetried(t *testing.T) {
	ts := &flakyTS{fails: 1, err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: 503}}}
	var v map[string]string
	if err := newTestClient(t, ts).GetJSON(context.Background(), "me", &v); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if ts.n != 2 {
		t.Fatalf("token calls = %d", ts.n)
	}
}

func TestRevokedTokenNeedsLogin(t *testing.T) {
	ts := &flakyTS{fails: 99, err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: 400}, ErrorCode: "invalid_grant"}}
	var v map[string]string
	err := newTestClient(t, ts).GetJSON(context.Background(), "me", &v)
	if !IsAuth(err) {
		t.Fatalf("expected auth error, got %v", err)
	}
	if ts.n != 1 {
		t.Fatalf("permanent auth error was retried %d times", ts.n)
	}
	if IsAuth(errors.New("dial tcp: no route to host")) {
		t.Fatal("network error classified as auth")
	}
}
