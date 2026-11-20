package catalog

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplacementStore(t *testing.T) {
	s := openTestDB(t).(ReplacementStore)

	_, err := s.GetReplacement("sonarr_main", 1)
	require.ErrorIs(t, err, ErrNotFound)

	for _, it := range []*ReplacementItem{
		{Category: "sonarr_main", ItemID: 1, Kind: "episode", State: ReplacementPending, QueuedAt: 100},
		{Category: "sonarr_main", ItemID: 2, Kind: "episode", State: ReplacementPending, QueuedAt: 50},
		{Category: "radarr_main", ItemID: 9, Kind: "movie", State: ReplacementPending, QueuedAt: 500},
		{Category: "sonarr_main", ItemID: 3, Kind: "episode", State: ReplacementDone, QueuedAt: 10, SearchedAt: 20, Result: "searched"},
	} {
		require.NoError(t, s.UpsertReplacement(it))
	}

	got, err := s.ListPendingReplacements(200, 10)
	require.NoError(t, err)
	require.Len(t, got, 2, "only pending rows queued at or before the cutoff")
	assert.Equal(t, 2, got[0].ItemID, "oldest first")
	assert.Equal(t, 1, got[1].ItemID)

	got, err = s.ListPendingReplacements(1000, 1)
	require.NoError(t, err)
	assert.Len(t, got, 1, "limit applies")

	n, err := s.CountPendingReplacements()
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	got[0].State, got[0].SearchedAt, got[0].Result = ReplacementDone, 300, "searched"
	require.NoError(t, s.UpsertReplacement(got[0]))
	n, err = s.CountReplacementSearchesSince(250)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "searches at or after ts")
	n, err = s.CountReplacementSearchesSince(0)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "never-searched rows do not count")

	pruned, err := s.PruneReplacements(250)
	require.NoError(t, err)
	assert.Equal(t, 1, pruned, "only the done row untouched since before ts")
	_, err = s.GetReplacement("sonarr_main", 3)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestReplacementSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	st, err := OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, st.(ReplacementStore).UpsertReplacement(&ReplacementItem{
		Category: "sonarr_main", ItemID: 7, Kind: "episode", State: ReplacementDone, QueuedAt: 1, SearchedAt: 42, Result: "searched"}))
	require.NoError(t, st.Close())

	st, err = OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	it, err := st.(ReplacementStore).GetReplacement("sonarr_main", 7)
	require.NoError(t, err)
	assert.Equal(t, int64(42), it.SearchedAt, "the cooldown stamp persists across a restart")
	assert.Equal(t, ReplacementDone, it.State)
}
