package catalog

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlowLane_UpsertGetList(t *testing.T) {
	s := openTestDB(t).(SlowLaneStore)

	_, err := s.GetSlowLane(hashA)
	require.True(t, errors.Is(err, ErrNotFound), "missing item must be ErrNotFound, got %v", err)

	a := &SlowLaneItem{Hash: hashA, Magnet: "magnet:?xt=urn:btih:" + hashA, Name: "Show S01-S02", Category: "sonarr_main",
		Origin: "manual", State: SlowDownloading, TorBoxID: 42, QueuedAt: 100, StartedAt: 110, Progress: 0.25,
		ETA: 3600, Speed: 1 << 20, Seeds: 3, Peers: 7, Attempts: 1, LastError: "x", Note: "n"}
	b := &SlowLaneItem{Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Category: "radarr_main", Origin: "wait", State: SlowQueued, QueuedAt: 50}
	require.NoError(t, s.UpsertSlowLane(a))
	require.NoError(t, s.UpsertSlowLane(b))

	got, err := s.GetSlowLane(hashA)
	require.NoError(t, err)
	assert.Equal(t, a, got, "every column round-trips")

	all, err := s.ListSlowLane()
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, b.Hash, all[0].Hash, "oldest queued first")

	dl, err := s.ListSlowLane(SlowDownloading, SlowCached)
	require.NoError(t, err)
	require.Len(t, dl, 1)
	assert.Equal(t, hashA, dl[0].Hash)

	// Replace keeps one row per hash.
	a.State, a.TorBoxID = SlowCached, 0
	require.NoError(t, s.UpsertSlowLane(a))
	all, err = s.ListSlowLane()
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestSlowLane_PruneOnlyFinished(t *testing.T) {
	s := openTestDB(t).(SlowLaneStore)
	items := []*SlowLaneItem{
		{Hash: "1111111111111111111111111111111111111111", State: SlowHandedOff, FinishedAt: 10},
		{Hash: "2222222222222222222222222222222222222222", State: SlowFailed, FinishedAt: 10},
		{Hash: "3333333333333333333333333333333333333333", State: SlowCancelled, FinishedAt: 500},  // too recent
		{Hash: "4444444444444444444444444444444444444444", State: SlowDownloading, FinishedAt: 10}, // active
		{Hash: "5555555555555555555555555555555555555555", State: SlowImported, FinishedAt: 0},     // no stamp
	}
	for _, it := range items {
		require.NoError(t, s.UpsertSlowLane(it))
	}
	n, err := s.PruneSlowLane(100)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	left, err := s.ListSlowLane()
	require.NoError(t, err)
	assert.Len(t, left, 3)
}

// Items survive a reopen (restart recovery reads them back from disk).
func TestSlowLane_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	st, err := OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, st.(SlowLaneStore).UpsertSlowLane(&SlowLaneItem{Hash: hashA, State: SlowDownloading, TorBoxID: 7}))
	require.NoError(t, st.Close())

	st, err = OpenSQLite(path)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	got, err := st.(SlowLaneStore).GetSlowLane(hashA)
	require.NoError(t, err)
	assert.Equal(t, SlowDownloading, got.State)
	assert.EqualValues(t, 7, got.TorBoxID)
}

// Target and auto-import bookkeeping round-trip; an empty target reads back as nil.
func TestSlowLane_TargetAndAutoImportRoundTrip(t *testing.T) {
	s := openTestDB(t).(SlowLaneStore)
	a := &SlowLaneItem{Hash: hashA, Category: "sonarr_main", State: SlowHandedOff, HandOff: "added",
		Target:             &SlowLaneTarget{SeriesID: 91, Season: 2, Title: "Lindy Mercer"},
		AutoImportAttempts: 2, AutoImportAt: 1234, AutoImportDone: true, AutoImportNote: "auto manual import: 34 file(s)"}
	require.NoError(t, s.UpsertSlowLane(a))
	got, err := s.GetSlowLane(hashA)
	require.NoError(t, err)
	assert.Equal(t, a, got)

	b := &SlowLaneItem{Hash: "cccccccccccccccccccccccccccccccccccccccc", State: SlowQueued, Target: &SlowLaneTarget{}}
	require.NoError(t, s.UpsertSlowLane(b))
	got, err = s.GetSlowLane(b.Hash)
	require.NoError(t, err)
	assert.Nil(t, got.Target, "a zero target is stored as none")
}

// A v1.2.9 slow_lane table gains the v1.2.10 columns on open; old rows read back with
// no target and no auto-import history.
func TestMigrateSlowLaneV129(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v129.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = raw.Exec(`
		CREATE TABLE slow_lane (
		    hash TEXT PRIMARY KEY, magnet TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
		    category TEXT NOT NULL DEFAULT '', origin TEXT NOT NULL DEFAULT 'manual',
		    state TEXT NOT NULL DEFAULT 'queued', torbox_id INTEGER NOT NULL DEFAULT 0,
		    queued_at INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0,
		    cached_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0,
		    updated_at INTEGER NOT NULL DEFAULT 0, progress REAL NOT NULL DEFAULT 0,
		    eta INTEGER NOT NULL DEFAULT 0, speed INTEGER NOT NULL DEFAULT 0,
		    seeds INTEGER NOT NULL DEFAULT 0, peers INTEGER NOT NULL DEFAULT 0,
		    size INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
		    last_error TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO slow_lane (hash, name, state, note) VALUES ('` + hashA + `', 'Old', 'handed_off', 'added');`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	st, err := OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	got, err := st.(SlowLaneStore).GetSlowLane(hashA)
	require.NoError(t, err)
	assert.Equal(t, "Old", got.Name)
	assert.Nil(t, got.Target)
	assert.Empty(t, got.HandOff)
	assert.Zero(t, got.AutoImportAttempts)
	assert.False(t, got.AutoImportDone)
}
