package qbit_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/qbit"
	"github.com/your-username/lazarr/internal/torbox"
)

// fakeArr records FailDownload calls; fn decides each call's result (n is 1-based).
type fakeArr struct {
	mu    sync.Mutex
	calls []string
	fn    func(n int, hash string) error
}

func (f *fakeArr) FailDownload(_ context.Context, hash string) error {
	f.mu.Lock()
	f.calls = append(f.calls, hash)
	n := len(f.calls)
	f.mu.Unlock()
	if f.fn == nil {
		return nil
	}
	return f.fn(n, hash)
}

func (f *fakeArr) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newBlocklistEnv is a wait-mode env whose radarr_hq category has a fake arr and
// whose retry schedule is instant. enabled sets policy.wait_fail_blocklist.
func newBlocklistEnv(t *testing.T, enabled bool, fa *fakeArr) *testEnv {
	t.Helper()
	e := newWaitEnv("wait")
	e.cfg.Policy.WaitFailBlocklist = enabled
	e.torbox.createOK = true
	h := qbit.New(qbit.Deps{
		Config: e.cfg, Store: e.store, TorBox: e.torbox, Symlink: e.symlink,
		Arrs: map[string]qbit.ArrFailer{"radarr_hq": fa},
	})
	qbit.SetArrFailDelaysForTest(h, []time.Duration{0, time.Millisecond, time.Millisecond})
	e.handler = h
	return e
}

// bailOverETA adds an uncached grab and runs one poll that bails it (ETA past max).
func bailOverETA(t *testing.T, e *testEnv, category string) {
	t.Helper()
	body, ct := formBody("urls", magnetURI(uncachedHash, "Slow+Release"), "category", category)
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, "Ok.", rec.Body.String())
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading", Progress: 0.01, ETA: 7200}
	}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))
	qbit.WaitArrFailsForTest(e.handler.(qbit.Server))
}

// TestWaitFailBlocklist_DisabledByDefault: without the setting a bail only flips the
// grab to error; the arr is never called.
func TestWaitFailBlocklist_DisabledByDefault(t *testing.T) {
	fa := &fakeArr{}
	e := newBlocklistEnv(t, false, fa)
	bailOverETA(t, e, "radarr_hq")

	assert.Equal(t, catalog.StateError, waitState(t, e))
	assert.Zero(t, fa.count(), "arr must not be called when wait_fail_blocklist is off")
}

// TestWaitFailBlocklist_FiresOnBail: enabled, the bail asks the arr to fail the
// download; the arr then deletes it from the client (removeFromClient=true), which
// must be clean for an errored row: 200, row gone, no further TorBox delete.
func TestWaitFailBlocklist_FiresOnBail(t *testing.T) {
	fa := &fakeArr{}
	e := newBlocklistEnv(t, true, fa)
	fa.fn = func(_ int, h string) error {
		body, ct := formBody("hashes", h)
		rec := e.do("POST", "/api/v2/torrents/delete", body, ct) // what the arr does next
		assert.Equal(t, http.StatusOK, rec.Code)
		return nil
	}
	bailOverETA(t, e, "radarr_hq")

	assert.Equal(t, []string{uncachedHash}, fa.calls)
	assert.Equal(t, []int64{9001}, e.torbox.deletedIDs, "only the bail's delete; the arr's client delete needs no TorBox call")
	_, _, err := e.store.GetRelease(uncachedHash)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
}

// TestWaitFailBlocklist_NotFiredOnSuccess: a completed wait download never calls the arr.
func TestWaitFailBlocklist_NotFiredOnSuccess(t *testing.T) {
	fa := &fakeArr{}
	e := newBlocklistEnv(t, true, fa)
	body, ct := formBody("urls", magnetURI(uncachedHash, "Waited"), "category", "radarr_hq")
	e.do("POST", "/api/v2/torrents/add", body, ct)
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "completed", DownloadFinished: true, Progress: 1}
	}
	e.torbox.nowCached = map[string]bool{uncachedHash: true}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))
	qbit.WaitArrFailsForTest(e.handler.(qbit.Server))

	assert.Equal(t, catalog.StateVirtual, waitState(t, e))
	assert.Zero(t, fa.count())
}

// TestWaitFailBlocklist_RetriesUntilInQueue: the arr may not list the item yet right
// after the flip; not-in-queue is retried on the backoff schedule.
func TestWaitFailBlocklist_RetriesUntilInQueue(t *testing.T) {
	fa := &fakeArr{fn: func(n int, _ string) error {
		if n < 3 {
			return arr.ErrNotInQueue
		}
		return nil
	}}
	e := newBlocklistEnv(t, true, fa)
	bailOverETA(t, e, "radarr_hq")
	assert.Equal(t, 3, fa.count())
}

// TestWaitFailBlocklist_BoundedAttempts: an arr that keeps failing gets exactly one
// attempt per scheduled delay, then the plain error state stays as the fallback.
func TestWaitFailBlocklist_BoundedAttempts(t *testing.T) {
	fa := &fakeArr{fn: func(int, string) error { return errors.New("connection refused") }}
	e := newBlocklistEnv(t, true, fa)
	bailOverETA(t, e, "radarr_hq")

	assert.Equal(t, 3, fa.count(), "len(delays) attempts, no more")
	assert.Equal(t, catalog.StateError, waitState(t, e))
}

// TestWaitFailBlocklist_StopsWhenAlreadyRemoved: if the release was removed some
// other way (Cleanuparr, a human) between attempts, retrying stops.
func TestWaitFailBlocklist_StopsWhenAlreadyRemoved(t *testing.T) {
	fa := &fakeArr{}
	e := newBlocklistEnv(t, true, fa)
	fa.fn = func(int, string) error {
		body, ct := formBody("hashes", uncachedHash)
		e.do("POST", "/api/v2/torrents/delete", body, ct) // removed elsewhere meanwhile
		return arr.ErrNotInQueue
	}
	bailOverETA(t, e, "radarr_hq")
	assert.Equal(t, 1, fa.count(), "row gone after the first attempt: no further arr calls")
}

// TestWaitFailBlocklist_NoArrForCategory: a category without an arr keeps the error
// state and makes no call.
func TestWaitFailBlocklist_NoArrForCategory(t *testing.T) {
	fa := &fakeArr{}
	e := newBlocklistEnv(t, true, fa)
	bailOverETA(t, e, "sonarr_main")

	assert.Equal(t, catalog.StateError, waitState(t, e))
	assert.Zero(t, fa.count())
}

// TestDelete_ErroredWaitReleaseIdempotent: deleting an errored release (what the arr
// does after a blocklist) twice is fine and never touches TorBox.
func TestDelete_ErroredWaitReleaseIdempotent(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true
	bailOverETA(t, e, "radarr_hq")
	require.Equal(t, catalog.StateError, waitState(t, e))
	before := len(e.torbox.deletedIDs)

	for range 2 {
		body, ct := formBody("hashes", uncachedHash)
		rec := e.do("POST", "/api/v2/torrents/delete", body, ct)
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Len(t, e.torbox.deletedIDs, before, "no TorBox call for an errored release")
	_, _, err := e.store.GetRelease(uncachedHash)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
}
