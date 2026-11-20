package qbit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/torbox"
)

// handOffAdded runs a cached manual add for sonarr_main through to an "added" hand-off
// (the arr has no grab of the hash).
func handOffAdded(t *testing.T, se *slowEnv, target *catalog.SlowLaneTarget) {
	t.Helper()
	se.regr.err = arr.ErrNoGrab
	se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Lindy Mercer S01-S02")}
	_, err := se.srv.SlowLaneEnqueue(magnetURI(slowHash2, "Lindy Mercer S01-S02"), "sonarr_main", target)
	require.NoError(t, err)
	se.tick()
	it := se.slow.get(t, slowHash2)
	require.Equal(t, catalog.SlowHandedOff, it.State)
	require.Equal(t, "added", it.HandOff)
}

func TestSlowLane_AutoImportAfterDelay(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, &catalog.SlowLaneTarget{SeriesID: 91, Season: 2, Title: " Lindy Mercer "})
	assert.Equal(t, &catalog.SlowLaneTarget{SeriesID: 91, Season: 2, Title: "Lindy Mercer"}, se.slow.get(t, slowHash2).Target)

	se.now = se.now.Add(10 * time.Minute)
	se.tick()
	assert.Empty(t, se.regr.imports, "the arr's own import gets auto_import_after first")

	se.regr.importRes = arr.ImportResult{Candidates: 65, Selected: 34, Imported: 34, SeriesID: 91,
		Episodes: []arr.EpisodeNum{{Season: 2, Episode: 1}, {Season: 2, Episode: 34}}, Command: "completed",
		Skipped: map[string]int{"other season": 31}}
	se.now = se.now.Add(6 * time.Minute)
	se.tick()
	require.Len(t, se.regr.imports, 1)
	call := se.regr.imports[0]
	assert.Equal(t, "/data/symlinks/sonarr_main/Lindy Mercer S01-S02", call.folder, "the content_path the arr sees")
	assert.Equal(t, slowHash2, call.hash)
	assert.Equal(t, arr.ImportTarget{SeriesID: 91, Season: 2}, call.target)

	it := se.slow.get(t, slowHash2)
	assert.Equal(t, 1, it.AutoImportAttempts)
	assert.True(t, it.AutoImportDone)
	assert.Equal(t, "auto manual import: 34 file(s) S02E01–E34 (skipped: 31 other season)", it.AutoImportNote)

	se.now = se.now.Add(time.Hour)
	se.tick()
	assert.Len(t, se.regr.imports, 1, "done: never re-run")
}

func TestSlowLane_AutoImportAttemptsCap(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, nil)
	se.regr.importErr = errors.New("sonarr: GET /api/v3/manualimport: HTTP 500")
	for range 6 {
		se.now = se.now.Add(16 * time.Minute)
		se.tick()
	}
	require.Len(t, se.regr.imports, 3, "at most 3 attempts")
	assert.Equal(t, arr.ImportTarget{}, se.regr.imports[0].target)
	it := se.slow.get(t, slowHash2)
	assert.Equal(t, 3, it.AutoImportAttempts)
	assert.True(t, it.AutoImportDone)
	assert.Contains(t, it.AutoImportNote, "HTTP 500")
	assert.Contains(t, it.AutoImportNote, "gave up after 3 attempts")
}

func TestSlowLane_AutoImportSpacing(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, nil)
	se.regr.importRes = arr.ImportResult{Candidates: 2, Skipped: map[string]int{"rejected": 2}}
	se.now = se.now.Add(16 * time.Minute)
	se.tick()
	se.now = se.now.Add(5 * time.Minute)
	se.tick()
	assert.Len(t, se.regr.imports, 1, "attempts are auto_import_after apart")
	it := se.slow.get(t, slowHash2)
	assert.False(t, it.AutoImportDone, "rejections may clear: retried")
	assert.Equal(t, "auto manual import: nothing to import (skipped: 2 rejected)", it.AutoImportNote)
	se.now = se.now.Add(11 * time.Minute)
	se.tick()
	assert.Len(t, se.regr.imports, 2)
}

func TestSlowLane_AutoImportAmbiguousStops(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, nil)
	se.regr.importRes = arr.ImportResult{Candidates: 4, Ambiguous: true}
	for range 3 {
		se.now = se.now.Add(16 * time.Minute)
		se.tick()
	}
	require.Len(t, se.regr.imports, 1)
	it := se.slow.get(t, slowHash2)
	assert.True(t, it.AutoImportDone)
	assert.Contains(t, it.AutoImportNote, "more than one series/movie")
}

func TestSlowLane_AutoImportDisabled(t *testing.T) {
	se := newSlowEnv(t, func(c *config.Config) { c.SlowLane.AutoImport = false })
	handOffAdded(t, se, nil)
	se.now = se.now.Add(time.Hour)
	se.tick()
	assert.Empty(t, se.regr.imports)
	assert.Zero(t, se.slow.get(t, slowHash2).AutoImportAttempts)
}

// A regrab hand-off goes through the arr's own grab: never auto-imported.
func TestSlowLane_AutoImportOnlyForAdded(t *testing.T) {
	se := newSlowEnv(t, nil)
	se.torbox.items = map[string]torbox.CachedItem{slowHash2: cachedItem(slowHash2, "Show S01")}
	_, err := se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", nil)
	require.NoError(t, err)
	se.tick()
	require.Equal(t, "regrab", se.slow.get(t, slowHash2).HandOff)
	se.now = se.now.Add(time.Hour)
	se.tick()
	assert.Empty(t, se.regr.imports)
}

// The arr removed the download from Lazarr before the import ran: nothing to scan.
func TestSlowLane_AutoImportDownloadGone(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, nil)
	require.NoError(t, se.store.DeleteRelease(slowHash2))
	se.now = se.now.Add(16 * time.Minute)
	se.tick()
	assert.Empty(t, se.regr.imports)
	it := se.slow.get(t, slowHash2)
	assert.True(t, it.AutoImportDone)
	assert.Contains(t, it.AutoImportNote, "no longer in sonarr_main")
}

func TestSlowLane_EnqueueTargetValidation(t *testing.T) {
	se := newSlowEnv(t, nil)
	_, err := se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", &catalog.SlowLaneTarget{SeriesID: 1, MovieID: 2})
	require.Error(t, err)
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", &catalog.SlowLaneTarget{Season: -1})
	require.Error(t, err)
	it, err := se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", &catalog.SlowLaneTarget{})
	require.NoError(t, err)
	assert.Nil(t, it.Target, "an empty target is none")
}

// An uploaded .torrent carries a target like a magnet add (the Missing tab's "Add
// torrent…" upload), validated the same way.
func TestSlowLaneEnqueueTorrent_Target(t *testing.T) {
	se := newSlowEnv(t, nil)
	it, err := se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Variety Show S01", false), "sonarr_main",
		&catalog.SlowLaneTarget{SeriesID: 7, Season: 1, Title: " The Great Evening Variety Show S01 "})
	require.NoError(t, err)
	assert.Equal(t, &catalog.SlowLaneTarget{SeriesID: 7, Season: 1, Title: "The Great Evening Variety Show S01"},
		se.slow.get(t, it.Hash).Target)

	se = newSlowEnv(t, nil)
	_, err = se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Variety Show S01", false), "sonarr_main",
		&catalog.SlowLaneTarget{SeriesID: 1, MovieID: 2})
	require.Error(t, err)
	_, err = se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Variety Show S01", false), "sonarr_main",
		&catalog.SlowLaneTarget{Season: -1})
	require.Error(t, err)
	assert.Empty(t, se.slow.items, "a bad target queues nothing")
	it, err = se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Variety Show S01", false), "sonarr_main", &catalog.SlowLaneTarget{})
	require.NoError(t, err)
	assert.Nil(t, se.slow.get(t, it.Hash).Target, "an empty target is none")
}

// A series target for a movie arr's category (or a movie target for a Sonarr category)
// is refused on both add paths: a TV episode queued under radarr_main, 2026-10-14.
func TestSlowLane_TargetKindMismatchRefused(t *testing.T) {
	se := newSlowEnv(t, func(c *config.Config) {
		c.Categories = append(c.Categories, "radarr_main", "tv_hd")
		c.Playback.Arrs = []config.Arr{{Category: "tv_hd", Kind: "sonarr"}}
	})
	series := &catalog.SlowLaneTarget{SeriesID: 7, Season: 1}
	movie := &catalog.SlowLaneTarget{MovieID: 42}

	_, err := se.srv.SlowLaneEnqueue(slowHash2, "radarr_main", series)
	require.ErrorContains(t, err, "imports movies")
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "radarr_main", &catalog.SlowLaneTarget{Season: 1})
	require.ErrorContains(t, err, "imports movies", "a season alone is a series target")
	_, err = se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Variety Show S01E02", false), "radarr_main", series)
	require.ErrorContains(t, err, "imports movies")
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "sonarr_main", movie)
	require.ErrorContains(t, err, "imports TV series")
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "tv_hd", movie)
	require.ErrorContains(t, err, "imports TV series", "playback.arrs kind wins over the name")
	assert.Empty(t, se.slow.items)

	// Matching kinds, a title-only target and no target are all fine.
	_, err = se.srv.SlowLaneEnqueue(slowHash2, "radarr_main", movie)
	require.NoError(t, err)
	_, err = se.srv.SlowLaneEnqueueTorrent(slowTorrentFile("Variety Show S01E02", false), "tv_hd", series)
	require.NoError(t, err)
	_, err = se.srv.SlowLaneEnqueue(uncachedHash, "radarr_main", &catalog.SlowLaneTarget{Title: "something"})
	require.NoError(t, err)
}

// Every candidate rejected only because its episodes already have files: final on the
// first attempt, no pointless retries (Still Waters, 2026-10-14: 8 "rejected", 2 retries).
func TestSlowLane_AutoImportNothingLeftIsFinal(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, nil)
	se.regr.importRes = arr.ImportResult{Candidates: 8, Skipped: map[string]int{"has file": 8}}
	for range 3 {
		se.now = se.now.Add(16 * time.Minute)
		se.tick()
	}
	require.Len(t, se.regr.imports, 1, "never retried")
	it := se.slow.get(t, slowHash2)
	assert.True(t, it.AutoImportDone)
	assert.Equal(t, "auto manual import: nothing left to import (skipped: 8 has file)", it.AutoImportNote)
}

// A real rejection next to the has-file ones still retries.
func TestSlowLane_AutoImportMixedRejectionsRetry(t *testing.T) {
	se := newSlowEnv(t, nil)
	handOffAdded(t, se, nil)
	se.regr.importRes = arr.ImportResult{Candidates: 8, Skipped: map[string]int{"has file": 7, "rejected": 1}}
	se.now = se.now.Add(16 * time.Minute)
	se.tick()
	it := se.slow.get(t, slowHash2)
	assert.False(t, it.AutoImportDone)
	assert.Equal(t, "auto manual import: nothing to import (skipped: 7 has file, 1 rejected)", it.AutoImportNote)
	se.now = se.now.Add(16 * time.Minute)
	se.tick()
	assert.Len(t, se.regr.imports, 2)
}
