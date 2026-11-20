package torbox_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

// fakeClock is a manual clock whose sleep advances time instead of blocking.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1791807800, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.slept += d
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newServerOnly(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// governedClient wires a client with a fake clock to a counting test server.
func governedClient(t *testing.T, rpm int, clk *fakeClock, h http.HandlerFunc) (torbox.Client, *int32) {
	t.Helper()
	var hits int32
	srv := newServerOnly(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		h(w, r)
	})
	cfg := config.TorBox{APIKey: fakeAPIKey, APIBase: srv, MaxRequestsPerMinute: rpm}
	return torbox.NewForTest(cfg, torbox.WithClock(clk.now, clk.sleep)), &hits
}

func okRequestDL(w http.ResponseWriter, remaining int) {
	w.Header().Set("x-ratelimit-limit", "300")
	w.Header().Set("x-ratelimit-remaining", strconv.Itoa(remaining))
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"success":true,"detail":"ok","data":"https://x.tb-cdn.io/f?token=t"}`))
}

// A 429 with TorBox's headers: exactly one request goes out; every call until the
// window resets fails fast with ErrAPIBackoff and NO request; after reset calls flow.
func TestGovernor_429BacksOffUntilReset(t *testing.T) {
	clk := newFakeClock()
	reset := clk.now().Add(45 * time.Second)
	limited := true
	c, hits := governedClient(t, 240, clk, func(w http.ResponseWriter, _ *http.Request) {
		if limited {
			w.Header().Set("x-ratelimit-limit", "300")
			w.Header().Set("x-ratelimit-remaining", "0")
			w.Header().Set("x-ratelimit-reset", fmt.Sprintf("%.6f", float64(reset.UnixNano())/1e9))
			w.Header().Set("retry-after", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"success":false,"error":"RATE_LIMITED","detail":"Too many requests."}`))
			return
		}
		okRequestDL(w, 299)
	})

	_, err := c.RequestDL(1, 0)
	require.ErrorIs(t, err, torbox.ErrAPIBackoff)
	var be *torbox.BackoffError
	require.True(t, errors.As(err, &be))
	assert.WithinDuration(t, reset, be.Until, time.Second, "x-ratelimit-reset (later than retry-after) wins")

	for i := 0; i < 10; i++ {
		_, err = c.RequestDL(1, 0)
		require.ErrorIs(t, err, torbox.ErrAPIBackoff)
		_, err = c.CheckCached([]string{"abc"})
		require.ErrorIs(t, err, torbox.ErrAPIBackoff, "the backoff is global, every endpoint")
	}
	assert.EqualValues(t, 1, atomic.LoadInt32(hits), "exactly one request until reset")

	limited = false
	clk.advance(46 * time.Second)
	url, err := c.RequestDL(1, 0)
	require.NoError(t, err)
	assert.Contains(t, url, "tb-cdn.io")
	assert.EqualValues(t, 2, atomic.LoadInt32(hits))
}

// x-ratelimit-remaining: 0 on a SUCCESSFUL response arms the backoff pre-emptively,
// so the next call is not a guaranteed 429.
func TestGovernor_RemainingZeroPreempts(t *testing.T) {
	clk := newFakeClock()
	c, hits := governedClient(t, 240, clk, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-reset", strconv.FormatInt(clk.now().Add(20*time.Second).Unix(), 10))
		okRequestDL(w, 0)
	})
	_, err := c.RequestDL(1, 0)
	require.NoError(t, err, "the call itself succeeded")
	_, err = c.RequestDL(1, 0)
	require.ErrorIs(t, err, torbox.ErrAPIBackoff)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
	until, on := torbox.BackoffUntilForTest(c)
	assert.True(t, on)
	assert.WithinDuration(t, clk.now().Add(20*time.Second), until, time.Second)
}

// A 429 without any usable header backs off for the default 60 s.
func TestGovernor_429WithoutHeadersDefaultBackoff(t *testing.T) {
	clk := newFakeClock()
	c, _ := governedClient(t, 240, clk, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.MyList(0)
	require.ErrorIs(t, err, torbox.ErrAPIBackoff)
	until, on := torbox.BackoffUntilForTest(c)
	require.True(t, on)
	assert.WithinDuration(t, clk.now().Add(60*time.Second), until, time.Second)
}

// The uncached-createtorrent hourly cap is endpoint-local: ErrRateLimited, and the rest
// of the API keeps working.
func TestGovernor_HourlyCreateLimitIsNotGlobal(t *testing.T) {
	clk := newFakeClock()
	c, hits := governedClient(t, 240, clk, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("retry-after", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"success":null,"detail":"60 per 1 hour","data":null}`))
			return
		}
		okRequestDL(w, 250)
	})
	_, _, err := c.CreateTorrent("magnet:?xt=urn:btih:abc", false)
	require.ErrorIs(t, err, torbox.ErrRateLimited)
	assert.False(t, errors.Is(err, torbox.ErrAPIBackoff))
	_, on := torbox.BackoffUntilForTest(c)
	assert.False(t, on, "no global backoff for the hourly create cap")
	_, err = c.RequestDL(1, 0)
	require.NoError(t, err)
	assert.EqualValues(t, 2, atomic.LoadInt32(hits))
}

// A 2xx body naming the rate limit also trips the global backoff.
func TestGovernor_RateLimitDetailTrips(t *testing.T) {
	clk := newFakeClock()
	c, hits := governedClient(t, 240, clk, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("retry-after", "10")
		_, _ = w.Write([]byte(`{"success":false,"detail":"You have hit the rate limit (300 per 1 minute)."}`))
	})
	_, err := c.UserMe()
	require.ErrorIs(t, err, torbox.ErrAPIBackoff)
	_, err = c.UserMe()
	require.ErrorIs(t, err, torbox.ErrAPIBackoff)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
}

// The token bucket caps Lazarr's own rate: at 60/min (burst 5) twenty calls must take at
// least 15 s of (fake) time — 5 from the burst, then one per second.
func TestGovernor_BucketCapsRate(t *testing.T) {
	clk := newFakeClock()
	c, hits := governedClient(t, 60, clk, func(w http.ResponseWriter, _ *http.Request) {
		okRequestDL(w, 299)
	})
	for i := 0; i < 20; i++ {
		_, err := c.RequestDL(1, 0)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 20, atomic.LoadInt32(hits))
	assert.InDelta(t, 15.0, clk.slept.Seconds(), 0.01, "15 tokens had to be waited for at 1/s")
}

// A caller is never parked longer than the governor's max wait: it gets ErrAPIBackoff.
func TestGovernor_BucketExhaustedFailsFast(t *testing.T) {
	clk := newFakeClock()
	c, _ := governedClient(t, 1, clk, func(w http.ResponseWriter, _ *http.Request) {
		okRequestDL(w, 299)
	})
	// rpm 1 -> burst 1, one token per minute: the 2nd call would wait 60 s > max wait.
	_, err := c.RequestDL(1, 0)
	require.NoError(t, err)
	_, err = c.RequestDL(1, 0)
	require.ErrorIs(t, err, torbox.ErrAPIBackoff)
	assert.Zero(t, clk.slept)
}

// mylist exposes the swarm (seeds/peers) that the wait-mode dead-swarm bail reads.
func TestMyListByID_SeedsPeers(t *testing.T) {
	clk := newFakeClock()
	c, _ := governedClient(t, 240, clk, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"detail":"ok","data":{"id":4000105,"hash":"x","name":"Sample.Show.B.S01E05","download_state":"stalled (no seeds)","progress":0.0,"eta":8640000,"seeds":0,"peers":2}}`))
	})
	d, err := c.MyListByID(4000105)
	require.NoError(t, err)
	assert.Equal(t, 0, d.Seeds)
	assert.Equal(t, 2, d.Peers)
}

// The live 2026-10-12 mylist capture decodes created_at / seeds / peers.
func TestMyList_LiveCaptureFields(t *testing.T) {
	clk := newFakeClock()
	body := readFixture(t, "mylist_account_20261012.json")
	c, _ := governedClient(t, 240, clk, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	items, err := c.MyList(0)
	require.NoError(t, err)
	require.Len(t, items, 9)
	assert.Equal(t, int64(4000105), items[4].ID)
	assert.Equal(t, time.Date(2026, 6, 17, 21, 42, 4, 0, time.UTC), items[4].CreatedAt.UTC())
	assert.Equal(t, 1, items[4].Seeds)
	assert.Equal(t, 1, items[4].Peers)
}
