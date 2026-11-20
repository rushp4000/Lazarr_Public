package qbit_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/qbit"
	"github.com/your-username/lazarr/internal/torbox"
)

// newWaitEnv is newTestEnv with on_cache_miss configured.
func newWaitEnv(mode string) *testEnv {
	e := newTestEnv(false)
	e.cfg.Policy.OnCacheMiss = mode
	e.cfg.Policy.CacheWaitBudget = config.Duration(15 * time.Minute)
	e.cfg.Policy.MaxWaitDownloads = 1
	return e
}

// TestCacheMiss_Reject: the add is refused ("Fails.") so the arr instantly falls
// back to its next release candidate; nothing is stored or symlinked.
func TestCacheMiss_Reject(t *testing.T) {
	e := newWaitEnv("reject")
	body, ct := formBody("urls", magnetURI(uncachedHash, "Some+Release"), "category", "radarr_hq")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Fails.", rec.Body.String())

	_, _, err := e.store.GetRelease(uncachedHash)
	assert.Error(t, err, "rejected add must not be stored")
	assert.Empty(t, e.symlink.created)
}

// TestCacheMiss_WaitHappyPath: miss -> uncached TorBox add -> StateDownloading with
// live progress in torrents/info -> poller sees it finished -> account copy released,
// grab flipped to a normal cached virtual import with symlinks.
func TestCacheMiss_WaitHappyPath(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true

	body, ct := formBody("urls", magnetURI(uncachedHash, "Waited+Release"), "category", "radarr_hq")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Ok.", rec.Body.String())
	require.NotEmpty(t, e.torbox.createCached)
	assert.False(t, e.torbox.createCached[0], "wait mode must add WITHOUT add_only_if_cached")

	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateDownloading, rel.State)
	assert.EqualValues(t, 9001, rel.TorBoxID)

	// While downloading: poller observes 40% / 60s ETA -> torrents/info shows it.
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading", Progress: 0.4, ETA: 60}
	}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	rec = e.do("GET", "/api/v2/torrents/info?category=radarr_hq", nil, "")
	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	require.Len(t, infos, 1)
	assert.Equal(t, "downloading", infos[0]["state"])
	assert.InDelta(t, 0.4, infos[0]["progress"], 0.001)

	// Download completes: poller must delete the account copy, re-checkcached, and
	// flip to a normal virtual import.
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "completed", DownloadFinished: true, Progress: 1}
	}
	e.torbox.nowCached = map[string]bool{uncachedHash: true}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	assert.Contains(t, e.torbox.deletedIDs, int64(9001), "account copy must be released on completion")
	rel, files, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateVirtual, rel.State)
	assert.True(t, rel.Cached)
	assert.EqualValues(t, 0, rel.TorBoxID)
	assert.NotEmpty(t, files)
	assert.Contains(t, e.symlink.created, uncachedHash)
}

// TestCacheMiss_WaitBailsWhenETAOverBudget: TorBox predicts the download cannot
// finish inside cache_wait_budget -> delete + error state.
func TestCacheMiss_WaitBailsWhenETAOverBudget(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true
	body, ct := formBody("urls", magnetURI(uncachedHash, "Slow+Release"), "category", "radarr_hq")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading", Progress: 0.01, ETA: 7200}
	}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	assert.Contains(t, e.torbox.deletedIDs, int64(9001))
	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateError, rel.State)
}

// TestCacheMiss_WaitIgnoresUnknownETASentinel: right after an uncached add TorBox
// reports ETA=8640000 (100 days, "no estimate yet"). That must not bail — the
// download keeps waiting until a real ETA or the hard budget decides.
func TestCacheMiss_WaitIgnoresUnknownETASentinel(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true
	body, ct := formBody("urls", magnetURI(uncachedHash, "Fresh+Add"), "category", "radarr_hq")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading", Progress: 0, ETA: 8640000}
	}
	qbit.PollWaitDownloadsForTest(e.handler.(qbit.Server))

	assert.NotContains(t, e.torbox.deletedIDs, int64(9001), "unknown-ETA sentinel must not trigger a bail")
	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateDownloading, rel.State)
}

// TestCacheMiss_WaitCapRefuses: a second miss while one wait download is in flight
// must not start another TorBox download, and (v1.2.8) is refused with "Fails." so the
// arr falls through to its next release instead of keeping a stuck error row.
func TestCacheMiss_WaitCapRefuses(t *testing.T) {
	e := newWaitEnv("wait")
	e.torbox.createOK = true

	body, ct := formBody("urls", magnetURI(uncachedHash, "First"), "category", "radarr_hq")
	e.do("POST", "/api/v2/torrents/add", body, ct)

	second := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	body, ct = formBody("urls", magnetURI(second, "Second"), "category", "radarr_hq")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)

	assert.Len(t, e.torbox.createCalls, 1, "cap=1: only the first miss may start a download")
	assert.Equal(t, "Fails.", rec.Body.String())
	_, _, err := e.store.GetRelease(second)
	assert.Error(t, err, "nothing stored for the overflow")
}

// With refuse_add_errors off the overflow keeps the pre-1.2.8 error row.
func TestCacheMiss_WaitCapFallsBackToError_WhenRefuseOff(t *testing.T) {
	e := newWaitEnv("wait")
	e.cfg.Policy.RefuseAddErrors = false
	e.torbox.createOK = true
	body, ct := formBody("urls", magnetURI(uncachedHash, "First"), "category", "radarr_hq")
	e.do("POST", "/api/v2/torrents/add", body, ct)
	second := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	body, ct = formBody("urls", magnetURI(second, "Second"), "category", "radarr_hq")
	assert.Equal(t, "Ok.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
	rel, _, err := e.store.GetRelease(second)
	require.NoError(t, err)
	assert.Equal(t, catalog.StateError, rel.State)
}

// createtorrent failing for a wait download is refused too.
func TestCacheMiss_WaitCreateFailRefuses(t *testing.T) {
	e := newWaitEnv("wait") // createOK false: the fake's CreateTorrent errors
	body, ct := formBody("urls", magnetURI(uncachedHash, "First"), "category", "radarr_hq")
	assert.Equal(t, "Fails.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
	_, _, err := e.store.GetRelease(uncachedHash)
	assert.Error(t, err)
}

// waitClock is a steppable clock for the budget/stall tests.
type waitClock struct{ t time.Time }

func (c *waitClock) now() time.Time       { return c.t }
func (c *waitClock) step(d time.Duration) { c.t = c.t.Add(d) }

// startClockedWait adds an uncached grab in wait mode (budget 10m, max auto = 30m)
// under a controllable clock and returns the env, clock and a progress setter.
func startClockedWait(t *testing.T) (*testEnv, *waitClock, func(p float64)) {
	t.Helper()
	e := newWaitEnv("wait")
	e.cfg.Policy.CacheWaitBudget = config.Duration(10 * time.Minute)
	e.torbox.createOK = true
	clk := &waitClock{t: time.Unix(1_790_000_000, 0)}
	qbit.SetClockForTest(e.handler.(qbit.Server), clk.now)

	body, ct := formBody("urls", magnetURI(uncachedHash, "Craft+Legacy"), "category", "radarr_main")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, "Ok.", rec.Body.String())

	progress := 0.0
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "downloading",
			Progress: progress, ETA: etaUnknown}
	}
	return e, clk, func(p float64) { progress = p }
}

// etaUnknown is TorBox's "no estimate" sentinel, so only the budget/stall rules act.
const etaUnknown = 8640000

func waitState(t *testing.T, e *testEnv) catalog.State {
	t.Helper()
	rel, _, err := e.store.GetRelease(uncachedHash)
	require.NoError(t, err)
	return rel.State
}

// TestCacheMiss_WaitExtendsWhileProgressing: the 2026-10-05 Glass Garden case — a
// download at 91% and still rising when the budget elapses keeps waiting and
// completes instead of being thrown away.
func TestCacheMiss_WaitExtendsWhileProgressing(t *testing.T) {
	e, clk, setProgress := startClockedWait(t)
	srv := e.handler.(qbit.Server)

	for _, p := range []float64{0.2, 0.4, 0.6, 0.8, 0.91} { // 2m ticks -> 10m
		clk.step(2 * time.Minute)
		setProgress(p)
		qbit.PollWaitDownloadsForTest(srv)
	}
	clk.step(time.Minute) // 11m: past the 10m budget, advanced 1m ago
	setProgress(0.95)
	qbit.PollWaitDownloadsForTest(srv)
	assert.NotContains(t, e.torbox.deletedIDs, int64(9001), "progressing download must not bail at the budget")
	assert.Equal(t, catalog.StateDownloading, waitState(t, e))

	clk.step(2 * time.Minute)
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, Hash: uncachedHash, DownloadState: "completed", DownloadFinished: true, Progress: 1}
	}
	e.torbox.nowCached = map[string]bool{uncachedHash: true}
	qbit.PollWaitDownloadsForTest(srv)
	assert.Equal(t, catalog.StateVirtual, waitState(t, e), "extended download completes as a normal import")
}

// TestCacheMiss_WaitBailsWhenStalledPastBudget: past the budget, a download whose
// progress has not risen for the stall window is abandoned (the Sleepless in
// Seattle case: 0% the whole time).
func TestCacheMiss_WaitBailsWhenStalledPastBudget(t *testing.T) {
	e, clk, setProgress := startClockedWait(t)
	srv := e.handler.(qbit.Server)

	setProgress(0)
	for range 5 { // 0% at every tick through 10m
		clk.step(2 * time.Minute)
		qbit.PollWaitDownloadsForTest(srv)
	}
	assert.Equal(t, catalog.StateDownloading, waitState(t, e), "never bails before the budget")

	clk.step(time.Minute)
	qbit.PollWaitDownloadsForTest(srv)
	assert.Contains(t, e.torbox.deletedIDs, int64(9001))
	assert.Equal(t, catalog.StateError, waitState(t, e))
}

// TestCacheMiss_WaitBailsAtMaxEvenIfProgressing: cache_wait_max is a hard ceiling.
func TestCacheMiss_WaitBailsAtMaxEvenIfProgressing(t *testing.T) {
	e, clk, setProgress := startClockedWait(t)
	srv := e.handler.(qbit.Server)

	p := 0.0
	for range 14 { // rising every 2m through 28m
		clk.step(2 * time.Minute)
		p += 0.05
		setProgress(p)
		qbit.PollWaitDownloadsForTest(srv)
	}
	assert.Equal(t, catalog.StateDownloading, waitState(t, e), "still progressing inside the 30m ceiling")

	clk.step(2 * time.Minute) // 30m
	setProgress(p + 0.05)
	qbit.PollWaitDownloadsForTest(srv)
	assert.Contains(t, e.torbox.deletedIDs, int64(9001))
	assert.Equal(t, catalog.StateError, waitState(t, e))
}

// TestCacheMiss_WaitMaxEqualsBudgetIsStrict: cache_wait_max == budget restores the
// old bail-at-budget behavior even for a progressing download.
func TestCacheMiss_WaitMaxEqualsBudgetIsStrict(t *testing.T) {
	e, clk, setProgress := startClockedWait(t)
	e.cfg.Policy.CacheWaitMax = config.Duration(10 * time.Minute)
	srv := e.handler.(qbit.Server)

	for _, p := range []float64{0.2, 0.4, 0.6, 0.8, 0.91} {
		clk.step(2 * time.Minute)
		setProgress(p)
		qbit.PollWaitDownloadsForTest(srv)
	}
	assert.Contains(t, e.torbox.deletedIDs, int64(9001))
	assert.Equal(t, catalog.StateError, waitState(t, e))
}

// A checkcached transport error (or an API backoff) is refused in every mode: the
// error says nothing about the release, so no error row may be stored for it.
func TestAdd_CheckCachedErrorRefuses(t *testing.T) {
	for _, mode := range []string{"error", "reject", "wait"} {
		t.Run(mode, func(t *testing.T) {
			e := newWaitEnv(mode)
			e.torbox.checkErr = &torbox.BackoffError{Until: time.Now().Add(time.Minute), Reason: "HTTP 429"}
			body, ct := formBody("urls", magnetURI(cachedHash, "BBB"), "category", "radarr_hq")
			assert.Equal(t, "Fails.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
			_, _, err := e.store.GetRelease(cachedHash)
			assert.Error(t, err)
			assert.Empty(t, e.torbox.createCalls)
		})
	}
}

// Dead swarm: no progress and 0 seeds / 0 peers for wait_no_progress (15m) bails
// early even though the budget (60m here) has time left.
func TestWait_NoProgressNoPeersBailsEarly(t *testing.T) {
	e, clk, _ := startClockedWait(t)
	e.cfg.Policy.CacheWaitBudget = config.Duration(60 * time.Minute)
	srv := e.handler.(qbit.Server)

	qbit.PollWaitDownloadsForTest(srv) // first observation at t0
	clk.step(14 * time.Minute)
	qbit.PollWaitDownloadsForTest(srv)
	assert.Equal(t, catalog.StateDownloading, waitState(t, e), "14m: still inside wait_no_progress")

	clk.step(2 * time.Minute)
	qbit.PollWaitDownloadsForTest(srv)
	assert.Contains(t, e.torbox.deletedIDs, int64(9001), "dead swarm is removed from TorBox")
	assert.Equal(t, catalog.StateError, waitState(t, e))
}

// The same stall with peers connected is NOT a dead swarm (the normal budget rules apply).
func TestWait_NoProgressWithPeersKeepsWaiting(t *testing.T) {
	e, clk, _ := startClockedWait(t)
	e.cfg.Policy.CacheWaitBudget = config.Duration(60 * time.Minute)
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading", ETA: etaUnknown, Seeds: 0, Peers: 3}
	}
	srv := e.handler.(qbit.Server)
	qbit.PollWaitDownloadsForTest(srv)
	clk.step(20 * time.Minute)
	qbit.PollWaitDownloadsForTest(srv)
	assert.NotContains(t, e.torbox.deletedIDs, int64(9001))
	assert.Equal(t, catalog.StateDownloading, waitState(t, e))

	// wait_no_progress 0 disables the rule entirely.
	e.cfg.Policy.WaitNoProgress = 0
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "downloading", ETA: etaUnknown}
	}
	clk.step(10 * time.Minute)
	qbit.PollWaitDownloadsForTest(srv)
	assert.Equal(t, catalog.StateDownloading, waitState(t, e))
}

// Per-category miss policy: the Seerr client's category is cached-only while the
// default client's category keeps waiting.
func TestCacheMiss_PerCategoryPolicy(t *testing.T) {
	e := newWaitEnv("wait")
	e.cfg.Categories = append(e.cfg.Categories, "sonarr_main_seerr")
	e.cfg.Policy.OnCacheMissByCategory = map[string]string{"sonarr_main_seerr": "reject"}
	e.torbox.createOK = true

	body, ct := formBody("urls", magnetURI(uncachedHash, "Seerr+Grab"), "category", "sonarr_main_seerr")
	assert.Equal(t, "Fails.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
	assert.Empty(t, e.torbox.createCalls, "reject never adds to TorBox")

	body, ct = formBody("urls", magnetURI(uncachedHash, "Direct+Grab"), "category", "sonarr_main")
	assert.Equal(t, "Ok.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
	assert.Equal(t, catalog.StateDownloading, waitState(t, e))
}
