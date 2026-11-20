package qbit_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/qbit"
	"github.com/your-username/lazarr/internal/torbox"
)

// The live fake of 2026-10-14 (sonarr_main, RSS every ~15 min): one .exe named like the
// episode, refused by the content gate.
const (
	gateFake     = "cccccccccccccccccccccccccccccccccccccccc"
	gateFakeName = "Charter House S15E03 Take the Hit 1080p WEB H264-CAKES"
)

// newGateEnv is a cached-only env whose cached gateFake is an .exe. arrs maps the
// categories that have an arr; blocklist sets policy.wait_fail_blocklist.
func newGateEnv(t *testing.T, blocklist bool, arrs map[string]qbit.ArrFailer) (*testEnv, *time.Time) {
	t.Helper()
	e := newTestEnv(false)
	e.cfg.Policy.WaitFailBlocklist = blocklist
	e.torbox.items = map[string]torbox.CachedItem{gateFake: {
		Hash: gateFake, Name: gateFakeName, Size: 929063766,
		Files: []torbox.CachedFile{{ID: 0, Name: gateFakeName + ".exe", Size: 929063766}},
	}}
	h := qbit.New(qbit.Deps{Config: e.cfg, Store: e.store, TorBox: e.torbox, Symlink: e.symlink, Arrs: arrs})
	qbit.SetArrFailDelaysForTest(h, []time.Duration{0, time.Millisecond, time.Millisecond})
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	qbit.SetClockForTest(h, func() time.Time { return now })
	e.handler = h
	return e, &now
}

// addGateFake posts the fake as the arr's RSS grab would and waits for the background
// arr call, returning the add's answer.
func addGateFake(t *testing.T, e *testEnv, category string) string {
	t.Helper()
	body, ct := formBody("urls", magnetURI(gateFake, "Charter.House.S15E03.1080p.WEB.H264-CAKES"), "category", category)
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	qbit.WaitArrFailsForTest(e.handler.(qbit.Server))
	return rec.Body.String()
}

// arrDeletes is a fakeArr that does what a real arr does on success: torrents/delete.
func arrDeletes(e **testEnv) *fakeArr {
	return &fakeArr{fn: func(_ int, h string) error {
		body, ct := formBody("hashes", h)
		(*e).do("POST", "/api/v2/torrents/delete", body, ct)
		return nil
	}}
}

// TestContentGate_AcceptToFail: with wait_fail_blocklist and an arr for the category, a
// gate refusal is accepted as an empty error row the arr fails (blocklist + search);
// in every other case it stays a plain "Fails.". The fake never gets a file, symlink
// or TorBox add.
func TestContentGate_AcceptToFail(t *testing.T) {
	cases := []struct {
		name      string
		blocklist bool
		category  string // sonarr_main has an arr, radarr_hq does not
		existing  bool   // a live release with the hash is already stored
		wantBody  string
		wantCalls int
	}{
		{"blocklist on, arr known", true, "sonarr_main", false, "Ok.", 1},
		{"blocklist off", false, "sonarr_main", false, "Fails.", 0},
		{"no arr for category", true, "radarr_hq", false, "Fails.", 0},
		{"existing row never overwritten", true, "sonarr_main", true, "Fails.", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e *testEnv
			fa := arrDeletes(&e)
			e, _ = newGateEnv(t, tc.blocklist, map[string]qbit.ArrFailer{"sonarr_main": fa})
			if tc.existing {
				require.NoError(t, e.store.UpsertRelease(&catalog.Release{Hash: gateFake, Name: "Old Grab",
					Category: tc.category, State: catalog.StateVirtual, AddedOn: 1, CreatedAt: 1}, nil))
			}

			assert.Equal(t, tc.wantBody, addGateFake(t, e, tc.category))
			assert.Equal(t, tc.wantCalls, fa.count())
			assert.Empty(t, e.symlink.created, "a gated release is never symlinked")
			assert.Empty(t, e.torbox.createCalls, "a gated release is never added to TorBox")
			rel, _, err := e.store.GetRelease(gateFake)
			if tc.existing {
				require.NoError(t, err)
				assert.Equal(t, catalog.StateVirtual, rel.State, "the existing row is untouched")
				return
			}
			assert.ErrorIs(t, err, catalog.ErrNotFound, "refused, or failed by the arr and deleted")
		})
	}
}

// TestContentGate_ArrOutcomes: what is left behind when the arr call does not succeed,
// and that a repeat grab of the same hash does not hammer the arr.
func TestContentGate_ArrOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		arrErr  error
		wantRow bool // the placeholder survives the background call
	}{
		// The arr deleted it itself (removeFromClient=true): row gone via torrents/delete.
		{"removed by arr", nil, false},
		// Never in the arr's queue / the arr keeps failing: the empty row is dropped so it
		// cannot hold the episode as a warning forever.
		{"never in queue", arr.ErrNotInQueue, false},
		{"arr error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e *testEnv
			fa := &fakeArr{}
			fa.fn = func(n int, h string) error {
				if n == 1 {
					// While the arr is being asked, the arr sees an errored, empty download.
					rec := e.do("GET", "/api/v2/torrents/info?hashes="+h, nil, "")
					var infos []map[string]any
					assert.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
					if assert.Len(t, infos, 1) {
						assert.Equal(t, "error", infos[0]["state"])
					}
				}
				if tc.arrErr == nil {
					body, ct := formBody("hashes", h)
					e.do("POST", "/api/v2/torrents/delete", body, ct)
				}
				return tc.arrErr
			}
			e, now := newGateEnv(t, true, map[string]qbit.ArrFailer{"sonarr_main": fa})

			require.Equal(t, "Ok.", addGateFake(t, e, "sonarr_main"))
			_, _, err := e.store.GetRelease(gateFake)
			assert.Equal(t, tc.wantRow, err == nil)
			first := fa.count()

			// The arr grabs the same hash again (RSS) within the TTL: plain refusal, no arr call.
			*now = now.Add(time.Hour)
			assert.Equal(t, "Fails.", addGateFake(t, e, "sonarr_main"))
			assert.Equal(t, first, fa.count(), "no second arr round trip within the TTL")

			// Past the TTL it is handed to the arr once more.
			*now = now.Add(12 * time.Hour)
			assert.Equal(t, "Ok.", addGateFake(t, e, "sonarr_main"))
			assert.Greater(t, fa.count(), first)
			assert.Empty(t, e.symlink.created)
		})
	}
}

// TestFailInArr: the engine's dead-cache hook fails only a visible error row, only with
// wait_fail_blocklist on.
func TestFailInArr(t *testing.T) {
	cases := []struct {
		name      string
		blocklist bool
		row       *catalog.Release // nil = no row
		wantCalls int
	}{
		{"errored row", true, &catalog.Release{State: catalog.StateError}, 1},
		{"blocklist off", false, &catalog.Release{State: catalog.StateError}, 0},
		{"healthy row", true, &catalog.Release{State: catalog.StateVirtual}, 0},
		{"detached row", true, &catalog.Release{State: catalog.StateError, DetachedAt: 1}, 0},
		{"no row", true, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeArr{}
			e, _ := newGateEnv(t, tc.blocklist, map[string]qbit.ArrFailer{"sonarr_main": fa})
			if tc.row != nil {
				r := *tc.row
				r.Hash, r.Name, r.Category, r.AddedOn, r.CreatedAt = cachedHash, "Purged", "sonarr_main", 1, 1
				require.NoError(t, e.store.UpsertRelease(&r, nil))
			}
			srv := e.handler.(qbit.Server)
			srv.FailInArr(cachedHash, "purged on TorBox (dead-cache)")
			qbit.WaitArrFailsForTest(srv)
			assert.Equal(t, tc.wantCalls, fa.count())
			if tc.wantCalls > 0 {
				// A dead-cache row is a real grab, not a gate placeholder: kept as fallback.
				_, _, err := e.store.GetRelease(cachedHash)
				assert.NoError(t, err)
			}
		})
	}
}
