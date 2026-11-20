package qbit_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/qbit"
	"github.com/your-username/lazarr/internal/torbox"
)

const slowHash2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// fakeSlowStore implements catalog.SlowLaneStore in memory.
type fakeSlowStore struct {
	mu    sync.Mutex
	items map[string]catalog.SlowLaneItem
}

func newFakeSlowStore() *fakeSlowStore {
	return &fakeSlowStore{items: map[string]catalog.SlowLaneItem{}}
}

func (f *fakeSlowStore) UpsertSlowLane(it *catalog.SlowLaneItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[it.Hash] = *it
	return nil
}

func (f *fakeSlowStore) GetSlowLane(hash string) (*catalog.SlowLaneItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[hash]
	if !ok {
		return nil, fmt.Errorf("%s: %w", hash, catalog.ErrNotFound)
	}
	return &it, nil
}

func (f *fakeSlowStore) ListSlowLane(states ...catalog.SlowLaneState) ([]*catalog.SlowLaneItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*catalog.SlowLaneItem
	for _, it := range f.items {
		if len(states) > 0 {
			ok := false
			for _, st := range states {
				ok = ok || it.State == st
			}
			if !ok {
				continue
			}
		}
		cp := it
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].QueuedAt != out[j].QueuedAt {
			return out[i].QueuedAt < out[j].QueuedAt
		}
		return out[i].Hash < out[j].Hash
	})
	return out, nil
}

func (f *fakeSlowStore) PruneSlowLane(before int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for h, it := range f.items {
		if !it.State.Active() && it.FinishedAt > 0 && it.FinishedAt < before {
			delete(f.items, h)
			n++
		}
	}
	return n, nil
}

func (f *fakeSlowStore) get(t *testing.T, hash string) catalog.SlowLaneItem {
	t.Helper()
	it, err := f.GetSlowLane(hash)
	require.NoError(t, err)
	return *it
}

// fakeRegrab is a qbit.SlowLaneArr and a qbit.SlowLaneImporter.
type fakeRegrab struct {
	calls []string
	err   error

	imports   []importCall
	importRes arr.ImportResult
	importErr error
}

type importCall struct {
	folder, hash string
	target       arr.ImportTarget
}

func (f *fakeRegrab) ImportMissing(_ context.Context, folder, hash string, t arr.ImportTarget) (arr.ImportResult, error) {
	f.imports = append(f.imports, importCall{folder, hash, t})
	return f.importRes, f.importErr
}

func (f *fakeRegrab) Regrab(_ context.Context, hash string) (arr.RegrabResult, error) {
	f.calls = append(f.calls, hash)
	if f.err != nil {
		return arr.RegrabResult{}, f.err
	}
	return arr.RegrabResult{SourceTitle: "Slow Release", BlocklistCleared: 1, Searched: []int{11, 12}}, nil
}

type slowEnv struct {
	*testEnv
	slow  *fakeSlowStore
	links *fakeLinks
	regr  *fakeRegrab
	srv   qbit.Server
	now   time.Time
}

// newSlowEnv builds a server with the slow lane enabled (wait mode for adoption tests).
func newSlowEnv(t *testing.T, mut func(*config.Config)) *slowEnv {
	t.Helper()
	e := newWaitEnv("wait")
	e.cfg.SlowLane.Enabled = true
	if mut != nil {
		mut(e.cfg)
	}
	se := &slowEnv{testEnv: e, slow: newFakeSlowStore(), links: &fakeLinks{links: map[string][]libscan.Link{}},
		regr: &fakeRegrab{}, now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	se.srv = qbit.New(qbit.Deps{
		Config: e.cfg, Store: e.store, TorBox: e.torbox, Symlink: e.symlink, Links: se.links,
		SlowLane: se.slow, SlowLaneArrs: map[string]qbit.SlowLaneArr{"sonarr_main": se.regr},
	})
	qbit.SetClockForTest(se.srv, func() time.Time { return se.now })
	e.handler = se.srv
	return se
}

func (se *slowEnv) tick() { qbit.SlowTickForTest(se.srv) }

func cachedItem(hash, name string, files ...torbox.CachedFile) torbox.CachedItem {
	if len(files) == 0 {
		files = []torbox.CachedFile{{ID: 0, Name: name + "/S01E01.mkv", Size: 400 << 20}, {ID: 1, Name: name + "/S02E01.mkv", Size: 410 << 20}}
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	return torbox.CachedItem{Hash: hash, Name: name, Size: size, Files: files}
}

func TestSlowLaneEnqueue(t *testing.T) {
	b32 := "VKVKVKVKVKVKVKVKVKVKVKVKVKVKVKVK" // base32 of 20 x 0xaa = uncachedHash
	cases := []struct {
		name     string
		disabled bool
		input    string
		category string
		wantErr  string
		wantHash string
		wantName string
	}{
		{name: "magnet with name", input: magnetURI(uncachedHash, "Lindy Mercer S01-S02"), category: "sonarr_main",
			wantHash: uncachedHash, wantName: "Lindy Mercer S01-S02"},
		{name: "bare hex hash", input: "  " + slowHash2 + " ", category: "sonarr_main", wantHash: slowHash2},
		{name: "base32 hash", input: b32, category: "sonarr_main", wantHash: uncachedHash},
		{name: "garbage", input: "not a hash", category: "sonarr_main", wantErr: "not a magnet link or infohash"},
		{name: "no category", input: slowHash2, wantErr: "category is required"},
		{name: "unknown category", input: slowHash2, category: "lidarr", wantErr: "unknown category"},
		{name: "disabled", disabled: true, input: slowHash2, category: "sonarr_main", wantErr: "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.Enabled = !tc.disabled })
			it, err := se.srv.SlowLaneEnqueue(tc.input, tc.category, nil)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, se.slow.items)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantHash, it.Hash)
			assert.Equal(t, tc.wantName, it.Name)
			stored := se.slow.get(t, tc.wantHash)
			assert.Equal(t, catalog.SlowQueued, stored.State)
			assert.Equal(t, "manual", stored.Origin)
			assert.Contains(t, stored.Magnet, tc.wantHash)
			assert.Empty(t, se.torbox.createCalls, "enqueue never touches TorBox")
		})
	}
}

func TestSlowLaneEnqueue_DuplicateActiveAndRequeueFinished(t *testing.T) {
	se := newSlowEnv(t, nil)
	_, err := se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)
	require.NoError(t, err)
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)
	require.ErrorIs(t, err, qbit.ErrSlowLaneActive)

	it := se.slow.get(t, slowHash2)
	it.State, it.FinishedAt = catalog.SlowFailed, 1
	require.NoError(t, se.slow.UpsertSlowLane(&it))
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "radarr_hq", nil)
	require.NoError(t, err, "a finished item can be queued again")
	assert.Equal(t, "radarr_hq", se.slow.get(t, slowHash2).Category)
}

// Already cached at start: no TorBox add at all; handed straight to the category as a
// normal cached release (symlinks, virtual).
func TestSlowLane_AlreadyCachedHandsOffWithoutDownload(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.regr.err = arr.ErrNoGrab // the arr never grabbed this hash
	se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Lindy.Mercer.S01-S02")}
	_, err := se.srv.SlowLaneEnqueue(magnetURI(slowHash2, "Lindy Mercer S01-S02"), "sonarr_main", nil)
	require.NoError(t, err)

	se.tick()

	assert.Empty(t, se.torbox.createCalls)
	it := se.slow.get(t, slowHash2)
	assert.Equal(t, catalog.SlowHandedOff, it.State)
	assert.Contains(t, it.Note, "completed download")
	rel, files, err := se.store.GetRelease(slowHash2)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateVirtual, rel.State)
	assert.True(t, rel.Cached)
	assert.Equal(t, "sonarr_main", rel.Category)
	assert.Equal(t, "Lindy Mercer S01-S02", rel.Name, "the magnet's name names the folder")
	assert.Len(t, files, 2)
	assert.Contains(t, se.symlink.created, slowHash2)
	assert.Equal(t, []string{slowHash2}, se.regr.calls, "the arr is asked first; no grab -> plain completed download")
}

// Full lifecycle: start (uncached add) -> progress -> finished -> account copy
// released -> hand-off -> imported once a library link appears.
func TestSlowLane_DownloadLifecycle(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.regr.err = arr.ErrNoGrab
	se.torbox.createOK = true
	_, err := se.srv.SlowLaneEnqueue(magnetURI(uncachedHash, "Old Show S01"), "sonarr_main", nil)
	require.NoError(t, err)

	se.tick() // start
	it := se.slow.get(t, uncachedHash)
	require.Equal(t, catalog.SlowDownloading, it.State)
	assert.EqualValues(t, 9001, it.TorBoxID)
	require.Len(t, se.torbox.createCached, 1)
	assert.False(t, se.torbox.createCached[0], "slow lane adds WITHOUT add_only_if_cached")
	ids, err := se.srv.SlowLaneHeldIDs()
	require.NoError(t, err)
	assert.Equal(t, []int64{9001}, ids, "the ToS audit must see the download as held")

	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading", Progress: 0.3, ETA: 40000, Seeds: 1, Peers: 4, DownloadSpeed: 1000}
	}
	se.now = se.now.Add(10 * time.Minute)
	se.tick() // progress, no budget: a 11-hour ETA is fine
	it = se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowDownloading, it.State)
	assert.InDelta(t, 0.3, it.Progress, 0.001)
	assert.Equal(t, 1, it.Seeds)
	assert.Empty(t, se.torbox.deletedIDs)

	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "completed", DownloadFinished: true, Progress: 1}
	}
	se.torbox.nowCached = map[string]bool{uncachedHash: true}
	se.now = se.now.Add(10 * time.Hour)
	se.tick() // finished
	assert.Equal(t, []int64{9001}, se.torbox.deletedIDs, "account copy released once cached")
	it = se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowHandedOff, it.State)
	assert.Zero(t, it.TorBoxID)
	rel, _, err := se.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateVirtual, rel.State)

	se.links.links[uncachedHash] = []libscan.Link{{Path: "/tv/Old Show/S01E01.mkv"}}
	se.now = se.now.Add(10 * time.Minute)
	se.tick()
	assert.Equal(t, catalog.SlowImported, se.slow.get(t, uncachedHash).State)
}

// max_concurrent bounds simultaneous TorBox downloads.
func TestSlowLane_RespectsMaxConcurrent(t *testing.T) {
	se := newSlowEnv(t, nil) // default max_concurrent 1
	se.torbox.createOK = true
	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading", Progress: 0.1}
	}
	_, err := se.srv.SlowLaneEnqueue(uncachedHash, "sonarr_main", nil)
	require.NoError(t, err)
	se.now = se.now.Add(time.Second)
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)
	require.NoError(t, err)

	se.tick()
	se.tick()
	assert.Len(t, se.torbox.createCalls, 1, "second item waits for a free slot")
	assert.Equal(t, catalog.SlowDownloading, se.slow.get(t, uncachedHash).State, "oldest first")
	assert.Equal(t, catalog.SlowQueued, se.slow.get(t, slowHash2).State)
}

// TorBox "not now" answers defer the start to the next tick without counting an attempt.
func TestSlowLane_StartDeferredOnRateLimitAndBackoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"createtorrent hourly cap", torbox.ErrRateLimited},
		{"parked upstream", torbox.ErrAlreadyQueued},
		{"api backoff", &torbox.BackoffError{Until: time.Now().Add(time.Minute), Reason: "429"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.MaxConcurrent = 2 })
			se.torbox.createErr = tc.err
			_, _ = se.srv.SlowLaneEnqueue(uncachedHash, "sonarr_main", nil)
			se.now = se.now.Add(time.Second)
			_, _ = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)

			se.tick()
			assert.Len(t, se.torbox.createCalls, 1, "stop starting more after a deferral")
			it := se.slow.get(t, uncachedHash)
			assert.Equal(t, catalog.SlowQueued, it.State)
			assert.Zero(t, it.Attempts, "a deferral is not a failed attempt")
			assert.NotEmpty(t, it.LastError)
		})
	}
}

// Other createtorrent errors count attempts; the fifth fails the item.
func TestSlowLane_StartErrorsFailAfterMaxAttempts(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.createErr = errors.New("torbox: 500")
	_, _ = se.srv.SlowLaneEnqueue(uncachedHash, "sonarr_main", nil)
	for i := 0; i < 5; i++ {
		se.tick()
	}
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowFailed, it.State)
	assert.Contains(t, it.LastError, "after 5 attempts")
}

// The content gate runs on the real file list: a fake (.exe) never reaches the arr.
// A manual add whose category has no arr configured goes straight to a completed download.
func TestSlowLane_HandOffWithoutArr(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Movie.2001")}
	_, err := se.srv.SlowLaneEnqueue(slowHash2, "radarr_hq", nil) // no SlowLaneArrs entry
	require.NoError(t, err)
	se.tick()
	assert.Empty(t, se.regr.calls)
	assert.Equal(t, catalog.SlowHandedOff, se.slow.get(t, slowHash2).State)
	rel, _, err := se.store.GetRelease(slowHash2)
	require.NoError(t, err)
	assert.Equal(t, "radarr_hq", rel.Category)
}

func TestSlowLane_ContentGateRefusal(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Fake",
		torbox.CachedFile{ID: 0, Name: "Fake/Show.S01E01.exe", Size: 900 << 20})}
	_, _ = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)
	se.tick()
	it := se.slow.get(t, slowHash2)
	assert.Equal(t, catalog.SlowFailed, it.State)
	assert.Contains(t, it.LastError, "content gate")
	_, _, err := se.store.GetRelease(slowHash2)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
}

// max_age deletes a download that never finishes; a failed delete keeps retrying.
func TestSlowLane_MaxAge(t *testing.T) {
	se := newSlowEnv(t, nil) // max_age 7d
	se.torbox.createOK = true
	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading", Progress: 0.01}
	}
	_, _ = se.srv.SlowLaneEnqueue(uncachedHash, "sonarr_main", nil)
	se.tick()
	require.Equal(t, catalog.SlowDownloading, se.slow.get(t, uncachedHash).State)

	se.now = se.now.Add(7*24*time.Hour + time.Minute)
	se.torbox.deleteErr = errors.New("torbox down")
	se.tick()
	assert.Equal(t, catalog.SlowDownloading, se.slow.get(t, uncachedHash).State, "never drop the item while its download may still exist")

	se.torbox.deleteErr = nil
	se.tick()
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowFailed, it.State)
	assert.Contains(t, it.LastError, "max_age")
	assert.Contains(t, se.torbox.deletedIDs, int64(9001))
}

// Restart recovery: a downloading item persisted by a previous process is resumed by
// the first tick of a new server.
func TestSlowLane_ResumesAfterRestart(t *testing.T) {
	se := newSlowEnv(t, nil)
	require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: uncachedHash, Category: "sonarr_main",
		Origin: "manual", State: catalog.SlowDownloading, TorBoxID: 4242, QueuedAt: se.now.Unix()}))
	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading", Progress: 0.5}
	}
	se.tick()
	assert.Equal(t, []int64{4242}, se.torbox.myListCalls)
	assert.InDelta(t, 0.5, se.slow.get(t, uncachedHash).Progress, 0.001)
	assert.Empty(t, se.torbox.createCalls, "no second add for a resumed download")
}

// A download that vanished from the account before it finished (deleted on the
// dashboard, or removed by TorBox's stalled sweep) fails without being re-added; a
// checkcached error is transient and leaves it downloading.
func TestSlowLane_VanishedFails(t *testing.T) {
	se := newSlowEnv(t, nil)
	require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: uncachedHash, Category: "sonarr_main",
		State: catalog.SlowDownloading, TorBoxID: 4242, QueuedAt: se.now.Unix()}))
	se.torbox.createOK = true

	se.torbox.checkErr = errors.New("torbox down")
	se.tick() // MyListByID -> nil (vanished), checkcached fails
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowDownloading, it.State)
	assert.Equal(t, int64(4242), it.TorBoxID)

	se.torbox.checkErr = nil
	se.tick() // vanished, not cached
	it = se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowFailed, it.State)
	assert.Zero(t, it.TorBoxID)
	assert.Contains(t, it.LastError, "removed from the TorBox account")
	assert.Empty(t, se.torbox.createCalls, "never re-add a download that was removed")
	assert.Empty(t, se.torbox.deletedIDs, "nothing left to delete")
}

// Cancel when TorBox refuses the delete: it counts as done only when mylist confirms the
// download is gone from the account.
func TestSlowLaneCancel_DeleteFails(t *testing.T) {
	present := func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading"}
	}
	gone := func(int64) *torbox.TorrentDetail { return nil }
	for _, tc := range []struct {
		name      string
		deleteErr error
		mylist    func(int64) *torbox.TorrentDetail
		mylistErr error
		wantErr   bool
		want      catalog.SlowLaneState
	}{
		{name: "delete ok", mylist: present, want: catalog.SlowCancelled},
		{name: "delete 500, already gone", deleteErr: errors.New("torbox: HTTP 500"), mylist: gone, want: catalog.SlowCancelled},
		{name: "delete 500, still present", deleteErr: errors.New("torbox: HTTP 500"), mylist: present, wantErr: true, want: catalog.SlowDownloading},
		{name: "delete 500, presence check fails", deleteErr: errors.New("torbox: HTTP 500"), mylistErr: errors.New("torbox down"), wantErr: true, want: catalog.SlowDownloading},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := newSlowEnv(t, nil)
			require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: uncachedHash, Category: "sonarr_main",
				State: catalog.SlowDownloading, TorBoxID: 4242, QueuedAt: se.now.Unix()}))
			se.torbox.deleteErr, se.torbox.myListByIDFn, se.torbox.myListErr = tc.deleteErr, tc.mylist, tc.mylistErr

			err := se.srv.SlowLaneCancel(uncachedHash)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			it := se.slow.get(t, uncachedHash)
			assert.Equal(t, tc.want, it.State)
			assert.Equal(t, []int64{4242}, se.torbox.deletedIDs)
		})
	}
}

// max_age give-up on a download that is already gone from the account does not get stuck
// retrying a delete TorBox keeps refusing.
func TestSlowLane_GiveUpAlreadyGone(t *testing.T) {
	se := newSlowEnv(t, nil)
	require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: uncachedHash, Category: "sonarr_main",
		State: catalog.SlowDownloading, TorBoxID: 4242, QueuedAt: se.now.Unix()}))
	se.now = se.now.Add(7*24*time.Hour + time.Minute)
	se.torbox.deleteErr = errors.New("torbox: HTTP 500")
	se.tick() // max_age: delete fails, mylist says gone
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowFailed, it.State)
	assert.Contains(t, it.LastError, "max_age")
}

func TestSlowLaneCancel(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.createOK = true
	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading"}
	}
	_, _ = se.srv.SlowLaneEnqueue(uncachedHash, "sonarr_main", nil)
	se.tick()

	se.torbox.deleteErr = errors.New("torbox down")
	require.Error(t, se.srv.SlowLaneCancel(uncachedHash))
	assert.Equal(t, catalog.SlowDownloading, se.slow.get(t, uncachedHash).State, "a failed delete leaves the item active")

	se.torbox.deleteErr = nil
	require.NoError(t, se.srv.SlowLaneCancel(uncachedHash))
	assert.Equal(t, catalog.SlowCancelled, se.slow.get(t, uncachedHash).State)
	require.Error(t, se.srv.SlowLaneCancel(uncachedHash), "cannot cancel twice")
}

// adopt_wait_failures: a wait download bailed for slowness keeps downloading in the
// slow lane (no TorBox delete); the grab still fails in qBit/the arr.
func TestSlowLane_AdoptsSlowWaitDownload(t *testing.T) {
	se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.AdoptWaitFailures = true })
	se.torbox.createOK = true
	bailOverETA(t, se.testEnv, "sonarr_main") // reason: eta over budget

	assert.Empty(t, se.torbox.deletedIDs, "the transfer is kept for the slow lane")
	assert.Equal(t, catalog.StateError, waitState(t, se.testEnv), "the grab still fails")
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowDownloading, it.State)
	assert.EqualValues(t, 9001, it.TorBoxID)
	assert.Equal(t, "wait", it.Origin)
	assert.Contains(t, it.Note, "eta over budget")
}

// With the slow lane full the adopted item is queued and the transfer deleted as usual.
func TestSlowLane_AdoptQueuedWhenFull(t *testing.T) {
	se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.AdoptWaitFailures = true })
	se.torbox.createOK = true
	require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: slowHash2, State: catalog.SlowDownloading, TorBoxID: 7}))
	bailOverETA(t, se.testEnv, "sonarr_main")

	assert.Equal(t, []int64{9001}, se.torbox.deletedIDs)
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowQueued, it.State)
	assert.Zero(t, it.TorBoxID)
}

// Adoption is off by default and never applies to non-slowness failures.
func TestSlowLane_NoAdoptionByDefault(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.createOK = true
	bailOverETA(t, se.testEnv, "sonarr_main")
	assert.Equal(t, []int64{9001}, se.torbox.deletedIDs)
	assert.Empty(t, se.slow.items)
}

// Hand-off of an adopted item: the arr's errored row is still there (wait_fail_blocklist
// off) -> revived in place, no arr call.
func TestSlowLane_HandOffRevivesErroredGrab(t *testing.T) {
	se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.AdoptWaitFailures = true })
	se.torbox.createOK = true
	bailOverETA(t, se.testEnv, "sonarr_main")
	se.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadFinished: true, DownloadState: "completed"}
	}
	se.torbox.nowCached = map[string]bool{uncachedHash: true}
	se.tick()

	assert.Equal(t, catalog.StateVirtual, waitState(t, se.testEnv), "the arr's own download completes")
	assert.Empty(t, se.regr.calls)
	it := se.slow.get(t, uncachedHash)
	assert.Equal(t, catalog.SlowHandedOff, it.State)
	assert.Contains(t, it.Note, "in place")
}

// Hand-off of an adopted item whose row the arr removed (wait_fail_blocklist): clear
// the blocklist + re-search through the arr; ErrNoGrab falls back to a completed
// download; other arr errors are retried.
func TestSlowLane_HandOffRegrab(t *testing.T) {
	for _, tc := range []struct {
		name      string
		regrabErr error
		wantState catalog.SlowLaneState
		wantNote  string
		wantRel   bool
	}{
		{"re-search requested", nil, catalog.SlowHandedOff, "search again", false},
		{"no grab in history", arr.ErrNoGrab, catalog.SlowHandedOff, "completed download", true},
		{"arr down, retried", errors.New("sonarr: 503"), catalog.SlowCached, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := newSlowEnv(t, nil)
			se.regr.err = tc.regrabErr
			se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Slow.Release")}
			require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: slowHash2, Category: "sonarr_main",
				Origin: "wait", State: catalog.SlowCached, CachedAt: se.now.Unix(), QueuedAt: se.now.Unix(),
				Magnet: "magnet:?xt=urn:btih:" + slowHash2}))
			se.tick()

			assert.Equal(t, []string{slowHash2}, se.regr.calls)
			it := se.slow.get(t, slowHash2)
			assert.Equal(t, tc.wantState, it.State)
			if tc.wantNote != "" {
				assert.Contains(t, it.Note, tc.wantNote)
			}
			_, _, err := se.store.GetRelease(slowHash2)
			assert.Equal(t, tc.wantRel, err == nil)
			if tc.wantState == catalog.SlowCached {
				assert.Equal(t, 1, it.Attempts)
				assert.Contains(t, it.LastError, "503")
			}
		})
	}
}

// A hash that is already a live release (re-grabbed meanwhile) needs no hand-off.
func TestSlowLane_HandOffExistingRelease(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Slow.Release")}
	require.NoError(t, se.store.UpsertRelease(&catalog.Release{Hash: slowHash2, State: catalog.StateVirtual, Category: "sonarr_main"}, nil))
	_, _ = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)
	se.tick()
	it := se.slow.get(t, slowHash2)
	assert.Equal(t, catalog.SlowHandedOff, it.State)
	assert.Contains(t, it.Note, "already a live release")
	assert.Empty(t, se.symlink.created)
}

// Finished history is pruned after the retention window; active items never are.
func TestSlowLane_PrunesOldHistory(t *testing.T) {
	se := newSlowEnv(t, nil)
	old := se.now.Add(-31 * 24 * time.Hour).Unix()
	require.NoError(t, se.slow.UpsertSlowLane(&catalog.SlowLaneItem{Hash: slowHash2, State: catalog.SlowFailed, FinishedAt: old}))
	se.tick()
	_, err := se.slow.GetSlowLane(slowHash2)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
}

// slowTorrentFile is a synthetic public .torrent with two trackers.
func slowTorrentFile(name string, private bool) []byte {
	info := map[string]any{"name": name, "length": 1 << 30, "piece length": 1 << 20, "pieces": "01234567890123456789"}
	if private {
		info["private"] = 1
	}
	return qbit.BencodeForTest(map[string]any{
		"announce":      "udp://tracker.one:1337/announce",
		"announce-list": []any{[]any{"udp://tracker.one:1337/announce", "https://tracker.two/announce"}},
		"info":          info,
	})
}

func TestSlowLaneEnqueueTorrent(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.regr.err = arr.ErrNoGrab
	se.torbox.createOK = true
	it, err := se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Old Show S01-S03", false), " sonarr_main ", nil)
	require.NoError(t, err)
	require.Len(t, it.Hash, 40)
	assert.Equal(t, "Old Show S01-S03", it.Name)

	stored := se.slow.get(t, it.Hash)
	assert.Equal(t, catalog.SlowQueued, stored.State)
	assert.Equal(t, "manual", stored.Origin)
	assert.Equal(t, "sonarr_main", stored.Category)
	assert.Equal(t, "uploaded .torrent (2 trackers)", stored.Note)
	assert.Empty(t, se.torbox.createCalls, "enqueue never touches TorBox")

	se.tick() // start: TorBox gets the magnet WITH the torrent's trackers
	require.Len(t, se.torbox.createCalls, 1)
	m := se.torbox.createCalls[0]
	assert.Contains(t, m, "xt=urn:btih:"+it.Hash)
	assert.Contains(t, m, "dn=Old%20Show%20S01-S03")
	assert.Contains(t, m, "tr=udp%3A%2F%2Ftracker.one%3A1337%2Fannounce")
	assert.Contains(t, m, "tr=https%3A%2F%2Ftracker.two%2Fannounce")
	assert.Equal(t, catalog.SlowDownloading, se.slow.get(t, it.Hash).State)

	// Same content again while active: the magnet path's duplicate semantics.
	old, err := se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Old Show S01-S03", false), "sonarr_main", nil)
	require.ErrorIs(t, err, qbit.ErrSlowLaneActive)
	require.NotNil(t, old)
	assert.Equal(t, catalog.SlowDownloading, old.State)
	_, err = se.srv.SlowLaneEnqueue(it.Hash, "sonarr_main", nil)
	require.ErrorIs(t, err, qbit.ErrSlowLaneActive, "a magnet/hash add of an uploaded torrent is a duplicate too")
}

func TestSlowLaneEnqueueTorrent_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		disabled bool
		torrent  []byte
		category string
		wantErr  string
	}{
		{name: "private", torrent: slowTorrentFile("Private", true), category: "sonarr_main", wantErr: "private torrent refused"},
		{name: "garbage", torrent: []byte("not bencode"), category: "sonarr_main", wantErr: "not a valid .torrent"},
		{name: "no category", torrent: slowTorrentFile("Show", false), wantErr: "category is required"},
		{name: "unknown category", torrent: slowTorrentFile("Show", false), category: "lidarr", wantErr: "unknown category"},
		{name: "disabled", disabled: true, torrent: slowTorrentFile("Show", false), category: "sonarr_main", wantErr: "disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.Enabled = !tc.disabled })
			it, err := se.srv.SlowLaneEnqueueTorrent(tc.torrent, tc.category, nil)
			require.Error(t, err)
			assert.Nil(t, it)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, se.slow.items)
		})
	}
}
