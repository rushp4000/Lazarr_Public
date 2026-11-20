package torbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/metrics"
)

// TorBox's API limiter (verified live 2026-10-11/12): 300 requests per fixed 60 s
// window PER API KEY, reported on every response as
//
//	x-ratelimit-limit: 300
//	x-ratelimit-remaining: 299
//	x-ratelimit-reset: 1791807817.137529   (unix epoch seconds, fractional)
//	retry-after: 61                        (seconds; sent even on 200)
//
// Uncached createtorrent is additionally capped at 60 per hour ("60 per 1 hour"), which
// is a separate, endpoint-specific limit and must NOT pause the whole API.
//
// Pre-1.2.8 Lazarr ignored these headers: one stuck import probed every minute by an arr
// turned into several requestdl calls per read, every one of them a 429 once the window
// was spent, and the window never got a chance to clear.

// ErrAPIBackoff is returned WITHOUT any network I/O while the client is backing off
// after TorBox signalled its per-key rate limit (HTTP 429, a rate-limit detail, or
// x-ratelimit-remaining: 0), or when the local token bucket cannot grant a request
// soon enough. The concrete error is a *BackoffError carrying the resume time.
var ErrAPIBackoff = errors.New("torbox: API rate-limit backoff")

// BackoffError is the concrete ErrAPIBackoff: when calls may resume and why.
type BackoffError struct {
	Until  time.Time
	Reason string
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("torbox: API rate-limit backoff until %s (%s)",
		e.Until.UTC().Format(time.RFC3339), e.Reason)
}

// Is makes errors.Is(err, ErrAPIBackoff) match.
func (e *BackoffError) Is(target error) bool { return target == ErrAPIBackoff }

const (
	// DefaultMaxRequestsPerMinute leaves 20% of TorBox's 300/min per-key window for
	// anything else using the key (the owner's scripts, a second app) and for retries.
	DefaultMaxRequestsPerMinute = 240
	// tokenMaxWait: a caller waits at most this long for a bucket token before getting
	// ErrAPIBackoff. FUSE reads that block a few seconds look like buffering; minutes
	// would pile up goroutines.
	tokenMaxWait = 15 * time.Second
	// defaultTripBackoff is used when a 429 carries no usable reset / retry-after.
	defaultTripBackoff = 60 * time.Second
	// maxTripBackoff caps how long a (buggy or hostile) header can pause the client.
	maxTripBackoff = 10 * time.Minute
)

// governor is the per-API-key request governor: a token bucket that keeps Lazarr under
// the window on its own, plus a global backoff armed by TorBox's own signals.
type governor struct {
	mu           sync.Mutex
	rate         float64 // tokens per second
	burst        float64
	tokens       float64
	last         time.Time
	backoffUntil time.Time
	reason       string

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func newGovernor(perMinute int) *governor {
	if perMinute <= 0 {
		perMinute = DefaultMaxRequestsPerMinute
	}
	burst := math.Max(1, float64(perMinute)/12) // 20 at the default: ~5 s of budget
	return &governor{
		rate:   float64(perMinute) / 60,
		burst:  burst,
		tokens: burst,
		now:    time.Now,
		sleep:  sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// acquire grants one request or returns a *BackoffError. During a TorBox-armed backoff
// it returns immediately; otherwise it may wait up to tokenMaxWait for a bucket token
// (tokens are reserved, so concurrent callers queue fairly rather than stampede).
func (g *governor) acquire(ctx context.Context) error {
	g.mu.Lock()
	now := g.now()
	if now.Before(g.backoffUntil) {
		err := &BackoffError{Until: g.backoffUntil, Reason: g.reason}
		g.mu.Unlock()
		return err
	}
	if !g.last.IsZero() {
		g.tokens = math.Min(g.burst, g.tokens+now.Sub(g.last).Seconds()*g.rate)
	}
	g.last = now
	if g.tokens >= 1 {
		g.tokens--
		g.mu.Unlock()
		return nil
	}
	wait := time.Duration((1 - g.tokens) / g.rate * float64(time.Second))
	if wait > tokenMaxWait {
		err := &BackoffError{Until: now.Add(wait), Reason: "local request budget exhausted"}
		g.mu.Unlock()
		return err
	}
	g.tokens-- // reserve: the next caller computes its wait behind this one
	g.mu.Unlock()
	return g.sleep(ctx, wait)
}

// active reports the current TorBox-armed backoff, if any.
func (g *governor) active() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backoffUntil, g.now().Before(g.backoffUntil)
}

// observe reads the rate-limit headers of any response. remaining == 0 arms the
// backoff until the window resets, so the next call is not a guaranteed 429.
func (g *governor) observe(h http.Header) {
	rem, ok := headerInt(h, "x-ratelimit-remaining")
	if !ok {
		return
	}
	metrics.SetTorBoxRateLimitRemaining(rem)
	if rem <= 0 {
		g.trip(h, "x-ratelimit-remaining is 0")
	}
}

// trip arms the global backoff from the response headers: the later of retry-after and
// x-ratelimit-reset, else defaultTripBackoff, capped at maxTripBackoff. Returns the
// resume time. Never shortens an already-armed backoff.
func (g *governor) trip(h http.Header, reason string) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	var until time.Time
	if d, ok := retryAfter(h, now); ok {
		until = now.Add(d)
	}
	if t, ok := resetTime(h, now); ok && t.After(until) {
		until = t
	}
	if !until.After(now) {
		until = now.Add(defaultTripBackoff)
	}
	if until.Sub(now) > maxTripBackoff {
		until = now.Add(maxTripBackoff)
	}
	if until.After(g.backoffUntil) {
		if !now.Before(g.backoffUntil) { // newly armed, not an extension: log once
			slog.Warn("torbox: API rate limit hit, pausing all TorBox calls",
				"reason", reason, "until", until.UTC().Format(time.RFC3339),
				"remaining", h.Get("x-ratelimit-remaining"), "retry_after", h.Get("retry-after"))
		}
		g.backoffUntil = until
		g.reason = reason
	}
	metrics.IncTorBoxBackoffTrip()
	return g.backoffUntil
}

func headerInt(h http.Header, key string) (int, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return int(f), true
}

// retryAfter parses Retry-After as delta seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("retry-after"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
		return time.Duration(secs * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return t.Sub(now), true
	}
	return 0, false
}

// resetTime parses x-ratelimit-reset: TorBox sends an absolute unix epoch (fractional
// seconds). A small value is treated as a delta in seconds, which some limiters send.
func resetTime(h http.Header, now time.Time) (time.Time, bool) {
	v := strings.TrimSpace(h.Get("x-ratelimit-reset"))
	if v == "" {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return time.Time{}, false
	}
	if f > 1e9 {
		sec, frac := math.Modf(f)
		return time.Unix(int64(sec), int64(frac*1e9)), true
	}
	return now.Add(time.Duration(f * float64(time.Second))), true
}

// detailHourlyLimit reports TorBox's uncached-createtorrent cap ("60 per 1 hour"):
// endpoint-specific, so it maps to ErrRateLimited and never pauses the whole API.
func detailHourlyLimit(detail string) bool {
	return strings.Contains(strings.ToLower(detail), "per 1 hour")
}

// detailRateLimited reports a body that names the per-key API limiter.
func detailRateLimited(detail string) bool {
	d := strings.ToLower(detail)
	if detailHourlyLimit(d) {
		return false
	}
	return strings.Contains(d, "rate limit") || strings.Contains(d, "ratelimit") ||
		strings.Contains(d, "too many requests") || strings.Contains(d, "per 1 minute") ||
		strings.Contains(d, "per minute")
}

// endpointLabel trims a request path to a bounded metric label.
func endpointLabel(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return path
}
