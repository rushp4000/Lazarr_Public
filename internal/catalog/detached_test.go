package catalog

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A v1.2.7 database has no detached_at column: opening it adds the column with every
// existing row attached (0).
func TestMigrateAddsDetachedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v127.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = raw.Exec(`
		CREATE TABLE release (
		    hash TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', category TEXT NOT NULL DEFAULT '',
		    magnet TEXT NOT NULL DEFAULT '', total_size INTEGER NOT NULL DEFAULT 0,
		    state TEXT NOT NULL DEFAULT 'virtual', cached INTEGER NOT NULL DEFAULT 0,
		    torbox_id INTEGER NOT NULL DEFAULT 0, added_on INTEGER NOT NULL DEFAULT 0,
		    last_access INTEGER NOT NULL DEFAULT 0, materialized_at INTEGER NOT NULL DEFAULT 0,
		    created_at INTEGER NOT NULL DEFAULT 0, cache_status TEXT NOT NULL DEFAULT '',
		    last_cache_check INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT 'torrent'
		);
		INSERT INTO release (hash, name, state) VALUES ('old', 'Old Release', 'virtual');`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	s, err := OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	rel, _, err := s.GetRelease("old")
	require.NoError(t, err)
	assert.Zero(t, rel.DetachedAt)
	rows, err := s.DetachedReleases()
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestDetachedLifecycle(t *testing.T) {
	s := openTestDB(t)
	require.NoError(t, s.UpsertRelease(sampleRelease("h1"), sampleFiles("h1")))
	require.NoError(t, s.UpsertRelease(sampleRelease("h2"), sampleFiles("h2")))

	require.NoError(t, s.SetDetached("h1", 1700000000))
	rel, files, err := s.GetRelease("h1")
	require.NoError(t, err)
	assert.EqualValues(t, 1700000000, rel.DetachedAt)
	assert.Len(t, files, 2, "detaching never touches file rows")

	rows, err := s.DetachedReleases()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "h1", rows[0].Hash)

	// Still listed for the web UI / category scans (qbit filters it, not the store).
	byCat, err := s.ListByCategory("radarr_hq")
	require.NoError(t, err)
	assert.Len(t, byCat, 2)

	// A re-grab (UpsertRelease with DetachedAt 0) re-attaches.
	require.NoError(t, s.UpsertRelease(sampleRelease("h1"), sampleFiles("h1")))
	rel, _, err = s.GetRelease("h1")
	require.NoError(t, err)
	assert.Zero(t, rel.DetachedAt)

	assert.ErrorIs(t, s.SetDetached("missing", 1), ErrNotFound)
}

func TestTorBoxNameRoundTrip(t *testing.T) {
	s := openTestDB(t)
	r := sampleRelease("tn")
	r.TorBoxName = "Show.S01E06.mkv"
	require.NoError(t, s.UpsertRelease(r, sampleFiles("tn")))
	got, _, err := s.GetRelease("tn")
	require.NoError(t, err)
	assert.Equal(t, "Show.S01E06.mkv", got.TorBoxName)
}
