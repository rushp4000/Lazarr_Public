package materialize

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

// liveAccount loads the 2026-10-12 mylist capture (hashes fabricated).
func liveAccount(t *testing.T) []torbox.TorrentDetail {
	t.Helper()
	b, err := os.ReadFile("../../testdata/torbox/mylist_account_20261012.json")
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data []struct {
			ID            int64  `json:"id"`
			Hash          string `json:"hash"`
			Name          string `json:"name"`
			DownloadState string `json:"download_state"`
			CreatedAt     string `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	var out []torbox.TorrentDetail
	for _, d := range env.Data {
		ts, _ := time.Parse(time.RFC3339, d.CreatedAt)
		out = append(out, torbox.TorrentDetail{ID: d.ID, Hash: d.Hash, Name: d.Name, DownloadState: d.DownloadState, CreatedAt: ts})
	}
	return out
}

func orphanEngine(t *testing.T, deleteAfter time.Duration) (*materializer, *fakeStore, *fakeTorBox, []torbox.TorrentDetail) {
	t.Helper()
	store := newFakeStore()
	tb := newFakeTorBox()
	acct := liveAccount(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	// A fresh add 2 minutes ago (a materialize that may not be registered yet).
	acct = append(acct, torbox.TorrentDetail{ID: 5000199, Hash: "ffffffffffffffffffffffffffffffffffffffff",
		Name: "Fresh.Add", CreatedAt: now.Add(-2 * time.Minute)})
	tb.myList = acct
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}, DeleteOrphansOlderThan: deleteAfter})
	if err != nil {
		t.Fatal(err)
	}
	m.SetLogger(quietLogger())
	m.SetNow(func() time.Time { return now })
	t.Cleanup(func() { _ = m.Close() })

	// Sample Film One is materialized by Lazarr right now (believed held).
	store.addRelease(&catalog.Release{Hash: acct[0].Hash, State: catalog.StateMaterialized, TorBoxID: acct[0].ID,
		MaterializedAt: now.Add(-time.Hour).Unix()})
	// Sample Anime is a Lazarr release whose copy was left behind (row virtual).
	store.addRelease(&catalog.Release{Hash: acct[1].Hash, State: catalog.StateVirtual})
	return m, store, tb, acct
}

// Report only (default): every unreferenced account torrent older than the grace is
// listed — including the 6 June leftovers the scoped leak check could never see — and
// nothing is deleted.
func TestAuditOrphans_ReportOnly(t *testing.T) {
	m, _, tb, acct := orphanEngine(t, 0)
	if err := m.AuditTOS(); err != nil {
		t.Fatal(err)
	}
	rep := m.Orphans()
	if rep.At == 0 || rep.Deletes {
		t.Fatalf("report header: %+v", rep)
	}
	if len(rep.Items) != 8 { // 9 live items - 1 materialized; the fresh add is in grace
		t.Fatalf("orphans = %d, want 8: %+v", len(rep.Items), rep.Items)
	}
	byID := map[int64]AccountOrphan{}
	for _, o := range rep.Items {
		byID[o.TorBoxID] = o
		if o.Deleted {
			t.Fatalf("report-only mode deleted %d", o.TorBoxID)
		}
	}
	if _, ok := byID[acct[0].ID]; ok {
		t.Fatal("a materialized release is not an orphan")
	}
	if !byID[acct[1].ID].InCatalog || byID[acct[2].ID].InCatalog {
		t.Fatalf("in_catalog wrong: %+v / %+v", byID[acct[1].ID], byID[acct[2].ID])
	}
	for _, june := range []int64{4000106, 4000105, 4000104, 4000103, 4000102, 4000101} {
		if _, ok := byID[june]; !ok {
			t.Fatalf("June leftover %d not reported", june)
		}
	}
	if tb.deleteCount() != 0 {
		t.Fatalf("deletes = %d, want 0", tb.deleteCount())
	}
}

// audit.delete_orphans_older_than: only orphans older than the threshold go.
func TestAuditOrphans_DeleteOlderThan(t *testing.T) {
	m, _, tb, _ := orphanEngine(t, 30*24*time.Hour)
	if err := m.AuditTOS(); err != nil {
		t.Fatal(err)
	}
	if tb.deleteCount() != 6 {
		t.Fatalf("deletes = %d, want the 6 June leftovers", tb.deleteCount())
	}
	for _, id := range []int64{5000103, 5000102, 5000101, 5000199} {
		if tb.deleted[id] {
			t.Fatalf("recent torrent %d must not be deleted", id)
		}
	}
	if !m.Orphans().Deletes {
		t.Fatal("report must say deletes are enabled")
	}
}

// Slow-lane downloads (Deps.HeldElsewhere) are held on purpose: never orphans, never
// deleted — even when old enough for audit.delete_orphans_older_than.
func TestAuditOrphans_SlowLaneHeldIsNotAnOrphan(t *testing.T) {
	m, _, tb, _ := orphanEngine(t, 30*24*time.Hour)
	const june = int64(4000106) // a June leftover, old enough to be deleted
	m.heldElsewhere = func() ([]int64, error) { return []int64{june}, nil }
	if err := m.AuditTOS(); err != nil {
		t.Fatal(err)
	}
	if tb.deleted[june] {
		t.Fatal("a slow-lane download was deleted as an orphan")
	}
	for _, o := range m.Orphans().Items {
		if o.TorBoxID == june {
			t.Fatal("a slow-lane download is listed as an orphan")
		}
	}
	if tb.deleteCount() != 5 {
		t.Fatalf("deletes = %d, want the other 5 June leftovers", tb.deleteCount())
	}
}

// An error from HeldElsewhere aborts the audit before anything can be deleted.
func TestAuditOrphans_HeldElsewhereErrorSkipsAudit(t *testing.T) {
	m, _, tb, _ := orphanEngine(t, 30*24*time.Hour)
	m.heldElsewhere = func() ([]int64, error) { return nil, errors.New("store down") }
	if err := m.AuditTOS(); err == nil {
		t.Fatal("audit must fail when the slow-lane ids are unknown")
	}
	if tb.deleteCount() != 0 {
		t.Fatalf("deletes = %d, want 0", tb.deleteCount())
	}
}
