package replsearch

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/arrroots"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback/arr"
)

type fakeArr struct {
	kind       string
	owners     map[int][]int // series/movie id -> episode/movie ids
	ownersErr  error
	states     map[int]arr.ItemState
	statesErr  error
	searchErr  error
	searched   [][]int
	stateCalls int
}

func (f *fakeArr) Kind() string { return f.kind }
func (f *fakeArr) FileOwners(_ context.Context, id int, _ []string) ([]int, error) {
	return f.owners[id], f.ownersErr
}
func (f *fakeArr) ItemStates(_ context.Context, ids []int) (map[int]arr.ItemState, error) {
	f.stateCalls++
	if f.statesErr != nil {
		return nil, f.statesErr
	}
	out := map[int]arr.ItemState{}
	for _, id := range ids {
		if st, ok := f.states[id]; ok {
			out[id] = st
		}
	}
	return out, nil
}
func (f *fakeArr) SearchItems(_ context.Context, ids []int) error {
	if f.searchErr != nil {
		return f.searchErr
	}
	f.searched = append(f.searched, append([]int(nil), ids...))
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openStore(t *testing.T, path string) catalog.ReplacementStore {
	t.Helper()
	st, err := catalog.OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st.(catalog.ReplacementStore)
}

func newSearcher(t *testing.T, arrs map[string]Arr, maxPerHour int) (*Searcher, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)}
	return &Searcher{Store: openStore(t, filepath.Join(t.TempDir(), "db.sqlite")), Arrs: arrs,
		MaxPerHour: maxPerHour, Cooldown: 72 * time.Hour, Now: c.now}, c
}

func missing() arr.ItemState { return arr.ItemState{Monitored: true} }

func target(cat string, id int) arrroots.Target {
	return arrroots.Target{Category: cat, ItemID: id, ArrPaths: []string{"/x"}}
}

// queue runs Collect + Enqueue for the given targets, all rescanned.
func queue(s *Searcher, targets ...arrroots.Target) int {
	return s.Enqueue(s.Collect(context.Background(), targets), targets)
}

func TestEnqueueOnlyAcceptedRescans(t *testing.T) {
	son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: {9007, 9008}, 9: {1}}}
	rad := &fakeArr{kind: arr.KindRadarr, owners: map[int][]int{5: {5}}}
	s, _ := newSearcher(t, map[string]Arr{"sonarr_main": son, "radarr_main": rad}, 10)

	targets := []arrroots.Target{target("sonarr_main", 412), target("sonarr_main", 9), target("radarr_main", 5), target("unknown", 1)}
	cands := s.Collect(context.Background(), targets)
	require.Len(t, cands, 4, "the unknown arr yields nothing")
	n := s.Enqueue(cands, []arrroots.Target{targets[0], targets[2]}) // series 9's rescan failed
	assert.Equal(t, 3, n)

	for _, tc := range []struct {
		cat  string
		id   int
		kind string
		ok   bool
	}{{"sonarr_main", 9007, "episode", true}, {"sonarr_main", 9008, "episode", true},
		{"radarr_main", 5, "movie", true}, {"sonarr_main", 1, "", false}} {
		it, err := s.Store.GetReplacement(tc.cat, tc.id)
		if !tc.ok {
			require.ErrorIs(t, err, catalog.ErrNotFound, "%s/%d", tc.cat, tc.id)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, catalog.ReplacementPending, it.State)
		assert.Equal(t, tc.kind, it.Kind)
	}
	assert.Equal(t, 0, queue(s, targets[0]), "already pending: not queued twice")
}

func TestCollectSkipsArrErrors(t *testing.T) {
	son := &fakeArr{kind: arr.KindSonarr, ownersErr: errors.New("boom")}
	s, _ := newSearcher(t, map[string]Arr{"sonarr_main": son}, 10)
	assert.Empty(t, s.Collect(context.Background(), []arrroots.Target{target("sonarr_main", 1)}))
}

func TestTickOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      *arr.ItemState // nil = the arr no longer knows the item
		statesErr  error
		searchErr  error
		ticks      int
		wantState  catalog.ReplacementState
		wantResult string
		wantSearch bool
	}{
		{"monitored and missing: searched", &arr.ItemState{Monitored: true}, nil, nil, 1, catalog.ReplacementDone, "searched", true},
		{"unmonitored: dropped", &arr.ItemState{}, nil, nil, 1, catalog.ReplacementDone, "unmonitored", false},
		{"unknown to the arr: dropped", nil, nil, nil, 1, catalog.ReplacementDone, "gone", false},
		{"still has a file: retried", &arr.ItemState{Monitored: true, HasFile: true}, nil, nil, 1, catalog.ReplacementPending, "has_file", false},
		{"still has a file after 3 checks: dropped", &arr.ItemState{Monitored: true, HasFile: true}, nil, nil, 3, catalog.ReplacementDone, "has_file", false},
		{"arr error: retried", nil, errors.New("down"), nil, 1, catalog.ReplacementPending, "error", false},
		{"arr error 3 times: dropped", nil, errors.New("down"), nil, 3, catalog.ReplacementDone, "error", false},
		{"search error: retried", &arr.ItemState{Monitored: true}, nil, errors.New("503"), 1, catalog.ReplacementPending, "error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: {7}}, states: map[int]arr.ItemState{},
				statesErr: tc.statesErr, searchErr: tc.searchErr}
			if tc.state != nil {
				son.states[7] = *tc.state
			}
			s, c := newSearcher(t, map[string]Arr{"sonarr_main": son}, 10)
			require.Equal(t, 1, queue(s, target("sonarr_main", 412)))
			for i := 0; i < tc.ticks; i++ {
				c.advance(TickEvery + settle)
				s.Tick(context.Background())
			}
			it, err := s.Store.GetReplacement("sonarr_main", 7)
			require.NoError(t, err)
			assert.Equal(t, tc.wantState, it.State)
			assert.Equal(t, tc.wantResult, it.Result)
			assert.Equal(t, tc.wantSearch, len(son.searched) == 1, "searched: %v", son.searched)
			if tc.wantSearch {
				assert.Equal(t, [][]int{{7}}, son.searched)
				assert.Equal(t, c.now().Unix(), it.SearchedAt)
			}
		})
	}
}

func TestTickWaitsForTheRescanToSettle(t *testing.T) {
	son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: {7}}, states: map[int]arr.ItemState{7: missing()}}
	s, c := newSearcher(t, map[string]Arr{"sonarr_main": son}, 10)
	queue(s, target("sonarr_main", 412))
	c.advance(settle - time.Minute)
	s.Tick(context.Background())
	assert.Zero(t, son.stateCalls, "no arr call before the settle time")
	c.advance(2 * time.Minute)
	s.Tick(context.Background())
	assert.Equal(t, [][]int{{7}}, son.searched)
}

func TestCooldownSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: {7}}, states: map[int]arr.ItemState{7: missing()}}
	c := &clock{t: time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)}
	mk := func() *Searcher {
		return &Searcher{Store: openStore(t, path), Arrs: map[string]Arr{"sonarr_main": son},
			MaxPerHour: 10, Cooldown: 72 * time.Hour, Now: c.now}
	}
	s := mk()
	queue(s, target("sonarr_main", 412))
	c.advance(settle)
	s.Tick(context.Background())
	require.Len(t, son.searched, 1)

	// The replacement dies too; the link is removed again a day later, after a restart.
	c.advance(24 * time.Hour)
	s = mk()
	assert.Equal(t, 0, queue(s, target("sonarr_main", 412)), "searched within the cooldown: not queued")
	c.advance(settle)
	s.Tick(context.Background())
	assert.Len(t, son.searched, 1, "no second search within the cooldown")

	c.advance(48 * time.Hour) // 72h+ since the search
	assert.Equal(t, 1, queue(s, target("sonarr_main", 412)), "queued again after the cooldown")
	c.advance(settle)
	s.Tick(context.Background())
	assert.Len(t, son.searched, 2)
}

func TestHourlyCap(t *testing.T) {
	ids := []int{1, 2, 3, 4, 5}
	states := map[int]arr.ItemState{}
	for _, id := range ids {
		states[id] = missing()
	}
	son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: ids}, states: states}
	s, c := newSearcher(t, map[string]Arr{"sonarr_main": son}, 2)
	require.Equal(t, 5, queue(s, target("sonarr_main", 412)))

	c.advance(settle)
	s.Tick(context.Background())
	assert.Equal(t, [][]int{{1, 2}}, son.searched, "only max_per_hour items")
	c.advance(TickEvery)
	s.Tick(context.Background())
	assert.Len(t, son.searched, 1, "cap still reached within the hour")
	assert.Equal(t, 1, son.stateCalls, "no arr calls while capped")

	c.advance(time.Hour)
	s.Tick(context.Background())
	assert.Equal(t, [][]int{{1, 2}, {3, 4}}, son.searched)
	n, err := s.Store.CountPendingReplacements()
	require.NoError(t, err)
	assert.Equal(t, 1, n, "the rest stays queued")
}

func TestQueueIsBounded(t *testing.T) {
	ids := make([]int, maxPending+5)
	for i := range ids {
		ids[i] = i + 1
	}
	son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: ids}}
	s, _ := newSearcher(t, map[string]Arr{"sonarr_main": son}, 10)
	assert.Equal(t, maxPending, queue(s, target("sonarr_main", 412)))
	n, err := s.Store.CountPendingReplacements()
	require.NoError(t, err)
	assert.Equal(t, maxPending, n)
}

func TestExpiredAndRemovedArr(t *testing.T) {
	son := &fakeArr{kind: arr.KindSonarr, owners: map[int][]int{412: {7}, 413: {8}}, states: map[int]arr.ItemState{7: missing()}}
	s, c := newSearcher(t, map[string]Arr{"sonarr_main": son}, 10)
	queue(s, target("sonarr_main", 412))
	c.advance(expireAfter + time.Minute)
	s.Tick(context.Background())
	it, err := s.Store.GetReplacement("sonarr_main", 7)
	require.NoError(t, err)
	assert.Equal(t, "expired", it.Result)
	assert.Zero(t, son.stateCalls)

	queue(s, target("sonarr_main", 413))
	delete(s.Arrs, "sonarr_main") // the arr was removed from playback.arrs
	c.advance(settle)
	s.Tick(context.Background())
	it, err = s.Store.GetReplacement("sonarr_main", 8)
	require.NoError(t, err)
	assert.Equal(t, "gone", it.Result)
}
