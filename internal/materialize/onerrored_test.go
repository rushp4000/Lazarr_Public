package materialize

import (
	"errors"
	"sync"
	"testing"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

// erroredRecorder records the SetOnErrored hook calls.
type erroredRecorder struct {
	mu    sync.Mutex
	calls []string // "hash|reason"
}

func (r *erroredRecorder) hook(hash, reason string) {
	r.mu.Lock()
	r.calls = append(r.calls, hash+"|"+reason)
	r.mu.Unlock()
}

// TestOnErrored: the engine tells the hook (-> qbit FailInArr -> the arr blocklists the
// release) exactly when it marks a release errored, once per transition; a transient
// failure or the fast-fail of an already-errored release never calls it.
func TestOnErrored(t *testing.T) {
	cases := []struct {
		name  string
		setup func(tb *fakeTorBox)
		want  []string
	}{
		{"dead-cache at createtorrent", func(tb *fakeTorBox) { tb.createErr = torbox.ErrNotFound },
			[]string{"h1|purged on TorBox (dead-cache)"}},
		{"dead-cache at stream (requestdl)", func(tb *fakeTorBox) { tb.requestDLErr = torbox.ErrNotFound },
			[]string{"h1|purged on TorBox (dead-cache)"}},
		{"no longer cached, allow_uncached off", func(tb *fakeTorBox) { tb.createErr = errors.New("torbox: torrent not cached") },
			[]string{"h1|not cached at materialize"}},
		{"transient createtorrent error", func(tb *fakeTorBox) { tb.createErr = errors.New("connection reset") },
			nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			tb := newFakeTorBox()
			tc.setup(tb)
			newRelease(store, "h1", "magnet:?xt=urn:btih:h1")
			m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 2}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			m.SetLogger(quietLogger())
			t.Cleanup(func() { _ = m.Close() })
			rec := &erroredRecorder{}
			m.SetOnErrored(rec.hook)

			p := make([]byte, 16)
			for range 2 { // the second read fast-fails on the errored row: no second call
				_, _ = m.ReadAt("h1", 0, p, 0)
			}
			if len(rec.calls) != len(tc.want) {
				t.Fatalf("hook calls = %v, want %v", rec.calls, tc.want)
			}
			for i := range tc.want {
				if rec.calls[i] != tc.want[i] {
					t.Fatalf("hook call %d = %q, want %q", i, rec.calls[i], tc.want[i])
				}
			}
		})
	}
}

// TestOnErrored_Unset: without a hook the dead-cache path behaves as before.
func TestOnErrored_Unset(t *testing.T) {
	store := newFakeStore()
	tb := newFakeTorBox()
	tb.createErr = torbox.ErrNotFound
	newRelease(store, "h1", "magnet:?xt=urn:btih:h1")
	m, err := New(Deps{Store: store, TorBox: tb, Policy: config.Policy{ActiveSlots: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetLogger(quietLogger())
	t.Cleanup(func() { _ = m.Close() })
	if _, rerr := m.ReadAt("h1", 0, make([]byte, 16), 0); !errors.Is(rerr, ErrPurged) {
		t.Fatalf("ReadAt err = %v, want ErrPurged", rerr)
	}
}
