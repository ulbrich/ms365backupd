package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ulbrich/ms365backupd/internal/status"
)

// serve starts the control API on a unix socket (only accessible by the user).
func (d *Daemon) serve(sock string) (io.Closer, error) {
	_ = os.Remove(sock) // stale socket; we hold the daemon lock so it isn't in use
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	_ = os.Chmod(sock, 0o600)
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, d.Status()) })
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 20
		}
		runs, err := d.hist.Last(n)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, runs)
	})
	mux.HandleFunc("POST /backup", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := RunRequest{Trigger: TriggerManual, FullResync: q.Get("full") == "1"}
		if s := q.Get("services"); s != "" {
			req.Only = strings.Split(s, ",")
		}
		d.mu.Lock()
		busy := d.engine != nil
		d.mu.Unlock()
		if busy {
			http.Error(w, "a backup is already running", http.StatusConflict)
			return
		}
		if !d.Request(req) {
			http.Error(w, "request queue full", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, map[string]string{"result": "started"})
	})
	mux.HandleFunc("POST /cancel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"cancelled": d.Cancel()})
	})
	mux.HandleFunc("POST /report", func(w http.ResponseWriter, r *http.Request) {
		if err := d.sendWeekly(r.Context(), true); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"result": "sent"})
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		err := d.RequestReload(ctx)
		switch {
		case err == nil:
			writeJSON(w, map[string]string{"result": "applied"})
		case errors.Is(err, ErrReloadDeferred):
			writeJSON(w, map[string]string{"result": "deferred"})
		default:
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return closerFunc(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := srv.Shutdown(ctx)
		_ = os.Remove(sock)
		return err
	}), nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// ErrNotRunning means no daemon answers on the socket.
var ErrNotRunning = errors.New("daemon is not running")

// Client talks to a running daemon.
type Client struct {
	hc *http.Client
}

// NewClient connects to the daemon socket in stateDir.
func NewClient(stateDir string) *Client {
	sock := SocketPath(stateDir)
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", sock)
	}}
	return &Client{hc: &http.Client{Transport: tr, Timeout: 3 * time.Minute}}
}

func (c *Client) call(method, path string, out any) error {
	req, _ := http.NewRequest(method, "http://daemon"+path, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		var ne *net.OpError
		if errors.As(err, &ne) || errors.Is(err, os.ErrNotExist) {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Status fetches the daemon status.
func (c *Client) Status() (*status.Daemon, error) {
	var s status.Daemon
	if err := c.call("GET", "/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// History fetches the last n runs (newest first).
func (c *Client) History(n int) ([]status.RunResult, error) {
	var r []status.RunResult
	err := c.call("GET", "/history?n="+strconv.Itoa(n), &r)
	return r, err
}

// Backup asks the daemon to start a backup now.
func (c *Client) Backup(services []string, full bool) error {
	p := "/backup?services=" + strings.Join(services, ",")
	if full {
		p += "&full=1"
	}
	return c.call("POST", p, nil)
}

// Cancel stops a running backup.
func (c *Client) Cancel() (bool, error) {
	var r map[string]bool
	err := c.call("POST", "/cancel", &r)
	return r["cancelled"], err
}

// Reload makes the daemon re-read its configuration. deferred is true if it will be
// applied after the running backup.
func (c *Client) Reload() (deferred bool, err error) {
	var r map[string]string
	err = c.call("POST", "/reload", &r)
	return r["result"] == "deferred", err
}

// Report sends the weekly report now.
func (c *Client) Report() error { return c.call("POST", "/report", nil) }
