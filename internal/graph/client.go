// Package graph is a small, resilient Microsoft Graph client: retries with
// exponential backoff, honours Retry-After on throttling, follows paging and
// delta links, and streams downloads with stall detection and resume.
package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
)

// DefaultBaseURL is the Graph v1.0 endpoint.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// APIError is a non-2xx Graph response.
type APIError struct {
	Status  int
	Code    string
	Message string
	URL     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("graph %d %s: %s", e.Status, e.Code, e.Message)
}

// IsResync reports whether a delta token expired and a full resync is needed.
func IsResync(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status == http.StatusGone {
		return true
	}
	c := strings.ToLower(ae.Code)
	return strings.Contains(c, "resync") || strings.Contains(c, "syncstatenotfound") || strings.Contains(c, "syncstateinvalid")
}

// IsPermanentItemError reports whether retrying the same item will not help
// (4xx other than throttling/timeout/auth). Such items are skipped, not fatal.
func IsPermanentItemError(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Status {
	case 401, 408, 409, 423, 429:
		return false
	}
	return ae.Status >= 400 && ae.Status < 500
}

// IsAuth reports authentication problems that need a new login (refresh token
// expired or revoked, consent withdrawn, app disabled). Temporary failures of the
// token endpoint (5xx, throttling, network) are not auth problems.
func IsAuth(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 401 {
		return true
	}
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return false
	}
	if re.Response != nil && (re.Response.StatusCode >= 500 || re.Response.StatusCode == 429) {
		return false
	}
	return true
}

// Client talks to Graph.
type Client struct {
	Base       string
	ts         oauth2.TokenSource
	api        *http.Client // bounded request time
	dl         *http.Client // no overall timeout, for large downloads
	log        *slog.Logger
	MaxRetries int
	// StallTimeout aborts a download when no bytes arrive for this long.
	StallTimeout time.Duration
	// LoginCmd is suggested when the sign-in has to be renewed.
	LoginCmd string
}

// New creates a client.
func New(base string, ts oauth2.TokenSource, log *slog.Logger) *Client {
	if base == "" {
		base = DefaultBaseURL
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		Base:         strings.TrimRight(base, "/"),
		ts:           ts,
		api:          &http.Client{Transport: tr, Timeout: 5 * time.Minute},
		dl:           &http.Client{Transport: tr},
		log:          log,
		MaxRetries:   8,
		StallTimeout: 2 * time.Minute,
		LoginCmd:     "ms365backupd login",
	}
}

func (c *Client) url(u string) string {
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return c.Base + "/" + strings.TrimLeft(u, "/")
}

func retryable(status int) bool {
	return status == 429 || status == 408 || status == 500 || status == 502 || status == 503 || status == 504
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(min(attempt, 7))) * time.Second
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return min(time.Duration(n)*time.Second, 10*time.Minute)
		}
	}
	return 0
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Request options.
type reqOpts struct {
	headers map[string]string
}

// Option customises a request.
type Option func(*reqOpts)

// WithHeader adds a request header.
func WithHeader(k, v string) Option { return func(o *reqOpts) { o.headers[k] = v } }

// do performs a request with retries. The caller must close the body of a 2xx response.
func (c *Client) do(ctx context.Context, hc *http.Client, method, u string, body []byte, hdr map[string]string) (*http.Response, error) {
	full := c.url(u)
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			if ra, ok := lastErr.(*retryAfterErr); ok && ra.wait > 0 {
				wait = ra.wait
			}
			c.log.Debug("graph retry", "url", full, "attempt", attempt, "wait", wait, "err", lastErr)
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, full, rdr)
		if err != nil {
			return nil, err
		}
		tok, err := c.ts.Token()
		if err != nil {
			if IsAuth(err) {
				return nil, fmt.Errorf("auth: %w (run `%s`)", err, c.LoginCmd)
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Token endpoint unreachable or temporarily failing: retry with backoff.
			lastErr = fmt.Errorf("token refresh: %w", err)
			continue
		}
		tok.SetAuthHeader(req)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		apiErr := readError(resp, full)
		if retryable(resp.StatusCode) {
			lastErr = &retryAfterErr{err: apiErr, wait: retryAfter(resp)}
			continue
		}
		return nil, apiErr
	}
	if ra, ok := lastErr.(*retryAfterErr); ok {
		return nil, ra.err
	}
	return nil, fmt.Errorf("giving up after %d retries: %w", c.MaxRetries, lastErr)
}

type retryAfterErr struct {
	err  error
	wait time.Duration
}

func (e *retryAfterErr) Error() string { return e.err.Error() }
func (e *retryAfterErr) Unwrap() error { return e.err }

func readError(resp *http.Response, u string) *APIError {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	ae := &APIError{Status: resp.StatusCode, URL: u, Message: http.StatusText(resp.StatusCode)}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error.Code != "" {
		ae.Code, ae.Message = env.Error.Code, env.Error.Message
	}
	return ae
}

// GetJSON GETs u and decodes the JSON response into out.
func (c *Client) GetJSON(ctx context.Context, u string, out any, opts ...Option) error {
	o := reqOpts{headers: map[string]string{}}
	for _, f := range opts {
		f(&o)
	}
	resp, err := c.do(ctx, c.api, http.MethodGet, u, nil, o.headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// GetBytes GETs u and returns the raw body (bounded to max bytes).
func (c *Client) GetBytes(ctx context.Context, u string, max int64, opts ...Option) ([]byte, error) {
	o := reqOpts{headers: map[string]string{}}
	for _, f := range opts {
		f(&o)
	}
	resp, err := c.do(ctx, c.api, http.MethodGet, u, nil, o.headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("response larger than %d bytes", max)
	}
	return b, nil
}

// PostJSON POSTs a JSON body; out may be nil.
func (c *Client) PostJSON(ctx context.Context, u string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, c.api, http.MethodPost, u, b, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Page is one page of a collection response.
type Page[T any] struct {
	Value     []T    `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// Iterate follows @odata.nextLink pages, calling fn for each page, and returns the
// final @odata.deltaLink (empty for non-delta collections).
func Iterate[T any](ctx context.Context, c *Client, u string, fn func([]T) error, opts ...Option) (string, error) {
	for u != "" {
		var p Page[T]
		if err := c.GetJSON(ctx, u, &p, opts...); err != nil {
			return "", err
		}
		if err := fn(p.Value); err != nil {
			return "", err
		}
		if p.NextLink == "" {
			return p.DeltaLink, nil
		}
		u = p.NextLink
	}
	return "", nil
}

// Download streams u into f starting at offset (resume via Range). It returns
// the number of bytes written. If the server ignores the Range header the file
// is truncated and rewritten from the start.
func (c *Client) Download(parent context.Context, u string, f *os.File, offset int64, opts ...Option) (int64, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	o := reqOpts{headers: map[string]string{}}
	for _, fn := range opts {
		fn(&o)
	}
	hdr := o.headers
	if offset > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-", offset)
	}
	resp, err := c.do(ctx, c.dl, http.MethodGet, u, nil, hdr)
	if err != nil {
		var ae *APIError
		if offset > 0 && errors.As(err, &ae) && ae.Status == http.StatusRequestedRangeNotSatisfiable {
			return c.Download(parent, u, f, 0, opts...)
		}
		return 0, err
	}
	defer resp.Body.Close()
	if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		offset = 0
	}
	if err := f.Truncate(offset); err != nil {
		return 0, &DiskError{Err: err}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, &DiskError{Err: err}
	}
	// Stall watchdog: cancel the request if no data arrives for StallTimeout.
	var stalled atomic.Bool
	stall := time.AfterFunc(c.StallTimeout, func() { stalled.Store(true); cancel() })
	defer stall.Stop()
	r := &watchReader{r: resp.Body, onRead: func() { stall.Reset(c.StallTimeout) }}
	n, err := io.CopyBuffer(f, r, make([]byte, 256<<10))
	if err != nil {
		switch {
		case parent.Err() != nil:
			return n, parent.Err()
		case stalled.Load():
			return n, fmt.Errorf("download stalled for %s after %d bytes", c.StallTimeout, offset+n)
		case r.readErr == nil:
			return n, &DiskError{Err: err}
		default:
			return n, fmt.Errorf("download interrupted after %d bytes: %w", offset+n, err)
		}
	}
	return n, nil
}

// DiskError marks a local write failure (disk full, disk removed, I/O error).
type DiskError struct{ Err error }

func (e *DiskError) Error() string { return "disk write failed: " + e.Err.Error() }
func (e *DiskError) Unwrap() error { return e.Err }

type watchReader struct {
	r       io.Reader
	onRead  func()
	readErr error
}

func (w *watchReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.onRead()
	}
	if err != nil && err != io.EOF {
		w.readErr = err
	}
	return n, err
}
