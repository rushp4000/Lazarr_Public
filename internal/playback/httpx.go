package playback

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewHTTPClient returns the client the media-server/arr packages share: a bounded
// timeout, no redirects (a LAN service never needs them; following one could walk
// off-host), and default keep-alives.
func NewHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// JoinURL appends path + query to a base URL without double slashes.
func JoinURL(base, path string, q url.Values) string {
	u := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// MaxBody caps how much of a media-server response is read (a session list or an
// episode list is kilobytes; a full library dump is not something we ever ask for).
const MaxBody = 32 << 20

// Do runs the request and returns the body for 2xx, or an error carrying the status.
// Callers never log the request URL (it may carry a token in a query string).
func Do(ctx context.Context, hc *http.Client, req *http.Request) ([]byte, string, error) {
	// The request URL is built from operator configuration (playback.*.url, validated
	// as http(s) at load time) plus a fixed path; nothing user-supplied reaches it.
	resp, err := hc.Do(req.WithContext(ctx)) //nolint:gosec // G704: operator-configured LAN servers
	if err != nil {
		return nil, "", fmt.Errorf("%s %s: %w", req.Method, redact(req.URL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return nil, "", fmt.Errorf("%s %s: read body: %w", req.Method, redact(req.URL), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", &StatusError{Method: req.Method, URL: redact(req.URL), Code: resp.StatusCode}
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// StatusError is Do's error for a non-2xx response; errors.As it to branch on the
// code (e.g. a 404 from a DELETE whose target is already gone). URL is redacted.
type StatusError struct {
	Method string
	URL    string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.Code)
}

// redact strips the query string (where a token could live) from a URL for errors.
func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.RawQuery = ""
	c.User = nil
	return c.String()
}
