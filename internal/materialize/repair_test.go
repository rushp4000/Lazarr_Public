package materialize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
)

// TestRepairScan_ReportsPreviousEviction: the first scan to see a hash gone reports
// PrevEvictedAt=0; a later scan reports when the earlier one saw it, which is what
// repair_unlink_dead needs to confirm an eviction.
func TestRepairScan_ReportsPreviousEviction(t *testing.T) {
	store := newFakeStore()
	h := "1111111111111111111111111111111111111111"
	store.releases[h] = &catalog.Release{Hash: h, Name: "Dead", Category: "sonarr_main", State: catalog.StateError}
	m, err := New(Deps{Store: store, TorBox: &fakeTorBox{}, Policy: config.Policy{ActiveSlots: 3}})
	require.NoError(t, err)

	first, err := m.RepairScan(testCtx())
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Zero(t, first[0].PrevEvictedAt, "first sighting")
	assert.Equal(t, catalog.StateError, first[0].State)

	store.releases[h].LastCacheCheck = 1_790_000_000 // pretend the first scan was earlier
	second, err := m.RepairScan(testCtx())
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.EqualValues(t, 1_790_000_000, second[0].PrevEvictedAt, "confirmed by the earlier scan")
}
