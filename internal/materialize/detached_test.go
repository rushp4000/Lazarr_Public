package materialize

import (
	"testing"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
)

// v1.2.8 detach_on_delete keeps detached rows in the catalog. Boot reconcile must treat
// them exactly like attached rows: a detached materialized leftover is still released
// (ToS), a detached virtual row is untouched, and neither loses its detached stamp.
func TestReconcile_DetachedRowsUnaffected(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 3}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })

	store.addRelease(&catalog.Release{Hash: "dm", State: catalog.StateMaterialized, TorBoxID: 1, DetachedAt: 100})
	store.addRelease(&catalog.Release{Hash: "dv", State: catalog.StateVirtual, DetachedAt: 100})

	m.Reconcile()

	if tb.deleteCount() != 1 {
		t.Fatalf("want 1 ControlDelete (the detached materialized leftover), got %d", tb.deleteCount())
	}
	if store.state("dm") != catalog.StateVirtual || store.state("dv") != catalog.StateVirtual {
		t.Fatalf("want both virtual: dm=%q dv=%q", store.state("dm"), store.state("dv"))
	}
	for _, h := range []string{"dm", "dv"} {
		if rel, _, _ := store.GetRelease(h); rel == nil || rel.DetachedAt != 100 {
			t.Fatalf("%s lost its detached stamp: %+v", h, rel)
		}
	}
}

// A detached release still materializes and reads: FUSE serves it for the library.
func TestReadAt_DetachedReleaseStillReads(t *testing.T) {
	content := make([]byte, 1<<16)
	m, store, tb, _ := engineWithCDN(t, content, 3, "")
	newRelease(store, "d1", "magnet:?xt=urn:btih:d1")
	_ = store.SetDetached("d1", 100)

	buf := make([]byte, 4096)
	n, err := m.ReadAt("d1", 0, buf, 0)
	if err != nil || n != len(buf) {
		t.Fatalf("detached release must read: n=%d err=%v", n, err)
	}
	if tb.createCount() != 1 {
		t.Fatalf("want 1 CreateTorrent, got %d", tb.createCount())
	}
}
