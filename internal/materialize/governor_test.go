package materialize

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

// v1.2.8 item 2: the materialize side of the TorBox governor.

// A requestdl failure is remembered per (hash, file) for requestdl_negative_ttl:
// repeated reads inside the window make NO further TorBox call; after it, one retry.
func TestRequestDL_NegativeCacheSuppressesRepeats(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, make([]byte, 1<<16), 3, "")
	m.policy.RequestDLNegativeTTL = config.Duration(90 * time.Second)
	clock := time.Now()
	m.SetNow(func() time.Time { return clock })
	newRelease(store, "nh", "magnet:?xt=urn:btih:nh")
	tb.mu.Lock()
	tb.requestDLErr = errors.New("torbox: requestdl: torbox: HTTP 500: server error")
	tb.mu.Unlock()

	p := make([]byte, 512)
	for i := 0; i < 5; i++ {
		if _, err := m.ReadAt("nh", 0, p, 1<<12); err == nil {
			t.Fatalf("read %d: want error", i)
		}
	}
	if got := tb.requestDLCount(); got != 1 {
		t.Fatalf("requestdl calls inside the window = %d, want 1", got)
	}

	// Another file of the same release is not affected.
	if _, err := m.ReadAt("nh", 1, p, 1<<12); err == nil {
		t.Fatal("file 1: want error from the fake")
	}
	if got := tb.requestDLCount(); got != 2 {
		t.Fatalf("other file must still call requestdl: calls = %d, want 2", got)
	}

	// Window over and TorBox healthy again: one call, read succeeds.
	clock = clock.Add(91 * time.Second)
	tb.mu.Lock()
	tb.requestDLErr = nil
	tb.mu.Unlock()
	if _, err := m.ReadAt("nh", 0, p, 1<<12); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if got := tb.requestDLCount(); got != 3 {
		t.Fatalf("requestdl calls = %d, want 3", got)
	}
}

// Governor backoffs are already free (no network), so they are never negative-cached:
// the read after the backoff ends must call TorBox again at once.
func TestRequestDL_BackoffNotNegativeCached(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, make([]byte, 1<<16), 3, "")
	m.policy.RequestDLNegativeTTL = config.Duration(90 * time.Second)
	newRelease(store, "bh", "magnet:?xt=urn:btih:bh")
	tb.mu.Lock()
	tb.requestDLOnce = &torbox.BackoffError{Until: time.Now().Add(time.Second), Reason: "test"}
	tb.mu.Unlock()

	p := make([]byte, 512)
	if _, err := m.ReadAt("bh", 0, p, 1<<12); !errors.Is(err, torbox.ErrAPIBackoff) {
		t.Fatalf("want ErrAPIBackoff (fail fast, backoff_read_wait=0), got %v", err)
	}
	if _, err := m.ReadAt("bh", 0, p, 1<<12); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := tb.requestDLCount(); got != 2 {
		t.Fatalf("requestdl calls = %d, want 2", got)
	}
}

// policy.backoff_read_wait: a read waits out a short backoff and retries once.
func TestReadAt_BackoffReadWait(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, make([]byte, 1<<16), 3, "")
	m.policy.BackoffReadWait = config.Duration(2 * time.Second)
	newRelease(store, "wh", "magnet:?xt=urn:btih:wh")
	tb.mu.Lock()
	tb.requestDLOnce = &torbox.BackoffError{Until: time.Now().Add(50 * time.Millisecond), Reason: "test"}
	tb.mu.Unlock()

	p := make([]byte, 512)
	if _, err := m.ReadAt("wh", 0, p, 1<<12); err != nil {
		t.Fatalf("read should wait out the 50ms backoff: %v", err)
	}

	// A backoff longer than the allowance fails at once.
	tb.mu.Lock()
	tb.requestDLOnce = &torbox.BackoffError{Until: time.Now().Add(time.Minute), Reason: "test"}
	tb.mu.Unlock()
	start := time.Now()
	if _, err := m.ReadAt("wh", 1, p, 1<<12); !errors.Is(err, torbox.ErrAPIBackoff) {
		t.Fatalf("want ErrAPIBackoff, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("must not wait for a backoff longer than backoff_read_wait")
	}
}

// Concurrent first reads of one file share ONE requestdl (fillHeader / readahead /
// proxy fall-through used to issue one each).
func TestRequestDL_Singleflight(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, make([]byte, 1<<20), 3, "")
	newRelease(store, "sf", "magnet:?xt=urn:btih:sf")
	p := make([]byte, 16)
	if _, err := m.ReadAt("sf", 0, p, 0); err != nil { // materialize + first link
		t.Fatalf("warm-up: %v", err)
	}
	ent := m.track["sf"]

	release := make(chan struct{})
	tb.mu.Lock()
	tb.requestDLHook = func() { <-release }
	tb.mu.Unlock()
	before := tb.requestDLCount()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.freshLink(testCtx(), ent, 0, true); err != nil {
				t.Errorf("freshLink: %v", err)
			}
		}()
	}
	// Let every goroutine reach the singleflight before releasing the one call.
	deadline := time.Now().Add(2 * time.Second)
	for tb.requestDLCount() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := tb.requestDLCount() - before; got != 1 {
		t.Fatalf("concurrent refreshes issued %d requestdl calls, want 1", got)
	}
}

// A dl_link cached by an EARLIER materialization (dl_link is keyed by hash + file, not
// torbox id) must not be reused after a release + re-materialize: TorBox's link for the
// deleted copy 4xxes, which v1.2.7 logged as "first link expired" ~0.1 s after
// materializing (2026-10-11 live logs, 6 of 7 cases).
func TestFreshLink_IgnoresLinkFromPreviousMaterialization(t *testing.T) {
	m, store, tb, _ := engineWithCDN(t, make([]byte, 1<<16), 3, "")
	clock := time.Unix(1_790_000_000, 0)
	m.SetNow(func() time.Time { return clock })
	newRelease(store, "rm", "magnet:?xt=urn:btih:rm")
	p := make([]byte, 512)
	if _, err := m.ReadAt("rm", 0, p, 1<<12); err != nil {
		t.Fatal(err)
	}
	if err := m.Release("rm"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(3 * time.Hour) // re-watched later; the old link is not yet past expires
	if _, err := m.ReadAt("rm", 0, p, 1<<12); err != nil {
		t.Fatal(err)
	}
	if got := tb.requestDLCount(); got != 2 {
		t.Fatalf("requestdl calls = %d, want 2 (a fresh link for the new account copy)", got)
	}
	// Within one materialization the cached link IS reused.
	if _, err := m.ReadAt("rm", 0, p, 1<<13); err != nil {
		t.Fatal(err)
	}
	if got := tb.requestDLCount(); got != 2 {
		t.Fatalf("requestdl calls = %d, want 2 (same materialization reuses its link)", got)
	}
}
