package missing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/playback/arr"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// fakeSource is an in-memory arr.
type fakeSource struct {
	mu       sync.Mutex
	eps      []arr.MissingEpisode
	movies   []arr.MissingMovie
	profiles map[int]string
	queue    []arr.QueueRecord
	err      error
	calls    atomic.Int32 // MissingEpisodes/MissingMovies calls (one per refresh)
	block    chan struct{}
	searches []string
}

func (f *fakeSource) fail() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeSource) MissingEpisodes(context.Context) ([]arr.MissingEpisode, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.eps, f.fail()
}
func (f *fakeSource) MissingMovies(context.Context) ([]arr.MissingMovie, error) {
	f.calls.Add(1)
	return f.movies, f.fail()
}
func (f *fakeSource) QualityProfiles(context.Context) (map[int]string, error) {
	return f.profiles, nil
}
func (f *fakeSource) Queue(context.Context) ([]arr.QueueRecord, error) { return f.queue, nil }
func (f *fakeSource) SearchSeason(_ context.Context, sid, season int) (int, error) {
	f.searches = append(f.searches, fmt.Sprintf("SeasonSearch %d S%d", sid, season))
	return 11, nil
}
func (f *fakeSource) SearchEpisodes(_ context.Context, ids []int) (int, error) {
	f.searches = append(f.searches, fmt.Sprintf("EpisodeSearch %v", ids))
	return 12, nil
}
func (f *fakeSource) SearchMovies(_ context.Context, ids []int) (int, error) {
	f.searches = append(f.searches, fmt.Sprintf("MoviesSearch %v", ids))
	return 13, f.fail()
}

func ep(id, series, season, num int, aired time.Time) arr.MissingEpisode {
	return arr.MissingEpisode{ID: id, SeriesID: series, SeasonNumber: season, EpisodeNumber: num, AirDateUtc: aired,
		Series: arr.MissingSeries{ID: series, Title: "Grady's Atlas: " + strconv.Itoa(series), Year: 2005, TVDBID: 9101,
			IMDBID: "tt9000003", QualityProfileID: 13, SeasonMonitored: map[int]bool{2: true}}}
}

func newTestCollector(arrs ...Arr) (*Collector, *time.Time) {
	c := New(arrs, 30*time.Minute)
	now := t0
	c.now = func() time.Time { return now }
	return c, &now
}

func sonarrFake() *fakeSource {
	day := 24 * time.Hour
	return &fakeSource{
		profiles: map[int]string{13: "1080p Classic SD"},
		eps: []arr.MissingEpisode{
			ep(110, 1, 2, 10, t0.Add(-10*day)),
			ep(101, 1, 2, 1, t0.Add(-40*day)),
			ep(102, 1, 2, 2, t0.Add(-39*day)),
			ep(103, 1, 2, 3, t0.Add(-38*day)),
			ep(104, 1, 2, 4, t0.Add(-37*day)),
			ep(108, 1, 2, 8, t0.Add(-20*day)),
			ep(111, 1, 2, 11, t0.Add(-9*day)),
			ep(112, 1, 2, 12, t0.Add(-8*day)),
			ep(113, 1, 2, 13, t0.Add(day)),         // unaired: dropped
			ep(114, 1, 2, 14, time.Time{}),         // TBA: dropped
			ep(301, 3, 1, 5, t0.Add(-100*day)),     // a second series, season 1
			ep(201, 1, 3, 1, t0.Add(-2*time.Hour)), // season 3 of series 1
		},
		queue: []arr.QueueRecord{
			{ID: 1, SeriesID: 1, EpisodeID: 108, SeasonNumber: 2},
			{ID: 2, SeriesID: 3, SeasonNumber: 1}, // no episode id: covers the season
			{ID: 3, SeriesID: 1, EpisodeID: 999},  // not missing
		},
	}
}

func TestSonarr_GroupingFilteringQueueProfiles(t *testing.T) {
	src := sonarrFake()
	c, _ := newTestCollector(Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: src})
	snap, err := c.Get(context.Background(), false)
	require.NoError(t, err)
	require.Len(t, snap.Arrs, 1)
	a := snap.Arrs[0]
	assert.Empty(t, a.Error)
	assert.Equal(t, 10, a.TotalMissing, "unaired + TBA dropped")
	assert.Equal(t, 2, a.Queued)
	assert.Equal(t, t0.Unix(), a.RefreshedAt)
	assert.Equal(t, t0.Unix(), snap.RefreshedAt)
	require.Len(t, a.Items, 3)

	s2 := a.Items[0]
	assert.Equal(t, "season", s2.Type)
	assert.Equal(t, "Grady's Atlas: 1", s2.Title)
	assert.Equal(t, 2005, s2.Year)
	assert.Equal(t, 9101, s2.TVDBID)
	assert.Equal(t, "tt9000003", s2.IMDBID)
	assert.Equal(t, "1080p Classic SD", s2.Profile)
	assert.Equal(t, 1, s2.SeriesID)
	assert.Equal(t, 2, s2.Season)
	assert.True(t, s2.SeasonMonitored)
	assert.Equal(t, []int{1, 2, 3, 4, 8, 10, 11, 12}, s2.Episodes)
	assert.Equal(t, []int{101, 102, 103, 104, 108, 110, 111, 112}, s2.EpisodeIDs, "ids parallel to the sorted numbers")
	assert.Equal(t, "E1–4, E8, E10–12", s2.EpisodeRanges)
	assert.Equal(t, 8, s2.Count)
	assert.Equal(t, 1, s2.Queued)
	assert.Equal(t, t0.Add(-40*24*time.Hour).Format(time.DateOnly), s2.FirstAired)
	assert.Equal(t, t0.Add(-8*24*time.Hour).Format(time.DateOnly), s2.LastAired)
	assert.Equal(t, "Gradys Atlas 1 S02", s2.SearchText)
	assert.Equal(t, "Grady's Atlas: 1 S02", s2.TargetTitle)

	s3 := a.Items[1]
	assert.Equal(t, 3, s3.Season)
	assert.False(t, s3.SeasonMonitored)
	assert.Equal(t, "E1", s3.EpisodeRanges)

	other := a.Items[2]
	assert.Equal(t, 3, other.SeriesID)
	assert.Equal(t, 1, other.Queued, "a queue record without an episode id covers its season")
}

func TestRadarr_AvailableOnlyAndQueue(t *testing.T) {
	src := &fakeSource{
		profiles: map[int]string{11: "1080p Quality HDR"},
		movies: []arr.MissingMovie{
			{ID: 202, Title: "I, Rover", Year: 2004, TMDBID: 9002, IMDBID: "tt9000002", QualityProfileID: 11, Available: true},
			{ID: 178, Title: "Vesper", Year: 2026, QualityProfileID: 11, Available: false},
			{ID: 201, Title: "Spiral Arrow", Year: 2001, QualityProfileID: 99, Available: true},
		},
		queue: []arr.QueueRecord{{ID: 1, MovieID: 201}},
	}
	c, _ := newTestCollector(Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: src})
	snap, err := c.Get(context.Background(), false)
	require.NoError(t, err)
	a := snap.Arrs[0]
	assert.Equal(t, 2, a.TotalMissing)
	assert.Equal(t, 1, a.Queued)
	require.Len(t, a.Items, 2)
	robot := a.Items[0]
	assert.Equal(t, Item{Type: "movie", Title: "I, Rover", Year: 2004, TMDBID: 9002, IMDBID: "tt9000002",
		Profile: "1080p Quality HDR", MovieID: 202, Count: 1, SearchText: "I Rover 2004", TargetTitle: "I, Rover (2004)"}, robot)
	assert.Equal(t, 1, a.Items[1].Queued)
	assert.Empty(t, a.Items[1].Profile, "unknown profile id")
}

func TestGet_CachesUntilRefresh(t *testing.T) {
	src := sonarrFake()
	c, _ := newTestCollector(Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: src})
	ctx := context.Background()
	_, err := c.Get(ctx, false)
	require.NoError(t, err)
	_, err = c.Get(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, int32(1), src.calls.Load(), "a cached snapshot is served without asking the arr")
}

func TestRefresh_RateLimited(t *testing.T) {
	src := sonarrFake()
	c, now := newTestCollector(Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: src})
	ctx := context.Background()
	_, err := c.Get(ctx, false)
	require.NoError(t, err)

	*now = now.Add(20 * time.Second)
	snap, err := c.Get(ctx, true)
	var rl *RateLimitedError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, 40*time.Second, rl.Wait)
	assert.Equal(t, 10, snap.Arrs[0].TotalMissing, "the cached snapshot still comes back")
	assert.Equal(t, int32(1), src.calls.Load())

	*now = now.Add(41 * time.Second)
	_, err = c.Get(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, int32(2), src.calls.Load())

	// The background refresh is not rate-limited.
	require.NoError(t, c.Refresh(ctx, false))
	assert.Equal(t, int32(3), src.calls.Load())
}

func TestRefresh_JoinsInFlight(t *testing.T) {
	src := sonarrFake()
	src.block = make(chan struct{})
	c, _ := newTestCollector(Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: src})

	errs := make(chan error, 1)
	go func() { errs <- c.Refresh(context.Background(), false) }()
	require.Eventually(t, func() bool { return src.calls.Load() == 1 }, time.Second, time.Millisecond)
	assert.True(t, c.Snapshot().Refreshing)

	// A manual refresh while one is in flight joins it: no rate limit, no second
	// fetch. A joiner that stops waiting gets its ctx error; the refresh carries on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, c.Refresh(ctx, true), context.Canceled, "joined, not rate-limited")
	require.ErrorIs(t, c.Refresh(ctx, false), context.Canceled)

	close(src.block)
	require.NoError(t, <-errs)
	assert.Equal(t, int32(1), src.calls.Load())
	assert.False(t, c.Snapshot().Refreshing)
	assert.Equal(t, 10, c.Snapshot().Arrs[0].TotalMissing)
}

func TestRefresh_PerArrErrorIsolation(t *testing.T) {
	good := sonarrFake()
	bad := &fakeSource{movies: []arr.MissingMovie{{ID: 1, Title: "Old", Year: 1999, Available: true}}}
	c, now := newTestCollector(
		Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: bad},
		Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: good},
	)
	ctx := context.Background()
	_, err := c.Get(ctx, false)
	require.NoError(t, err)

	bad.setErr(errors.New("radarr: GET /api/v3/wanted/missing: HTTP 503"))
	*now = now.Add(time.Hour)
	snap, err := c.Get(ctx, true)
	require.NoError(t, err)
	r := snap.Arrs[0]
	assert.Equal(t, "radarr_main", r.Category)
	assert.Contains(t, r.Error, "HTTP 503")
	assert.Equal(t, 1, r.TotalMissing, "the previous list is kept")
	assert.Equal(t, t0.Unix(), r.RefreshedAt, "and dated by its last good fetch")
	s := snap.Arrs[1]
	assert.Empty(t, s.Error)
	assert.Equal(t, 10, s.TotalMissing)
	assert.Equal(t, now.Unix(), s.RefreshedAt)
	assert.Equal(t, now.Unix(), snap.RefreshedAt)

	// Recovery clears the error.
	bad.setErr(nil)
	*now = now.Add(time.Hour)
	snap, err = c.Get(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, snap.Arrs[0].Error)
}

func TestRefresh_FirstFailureLeavesEmptyList(t *testing.T) {
	bad := &fakeSource{err: errors.New("boom")}
	c, _ := newTestCollector(Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: bad})
	snap, err := c.Get(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, "boom", snap.Arrs[0].Error)
	assert.Zero(t, snap.Arrs[0].RefreshedAt)
	assert.NotNil(t, snap.Arrs[0].Items)
	assert.Empty(t, snap.Arrs[0].Items)
}

func TestEpisodeRanges(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{5}, "E5"},
		{[]int{1, 2}, "E1–2"},
		{[]int{12, 1, 2, 3, 4, 8, 10, 11, 4}, "E1–4, E8, E10–12"},
		{[]int{0, 2, 4}, "E0, E2, E4"},
	} {
		assert.Equal(t, tc.want, EpisodeRanges(tc.in), "%v", tc.in)
	}
}

func TestSearchable(t *testing.T) {
	assert.Equal(t, "Gradys Atlas", searchable("Grady's Atlas"))
	assert.Equal(t, "Marvels Agents of S.H.I.E.L.D.", searchable("Marvel’s Agents of S.H.I.E.L.D."))
	assert.Equal(t, "Star Wars Andor", searchable("Star Wars: Andor"))
	assert.Equal(t, "Law & Ledger Night", searchable("Law & Ledger: Night"))
}

func intp(i int) *int { return &i }

func TestSearch_Commands(t *testing.T) {
	son := sonarrFake()
	rad := &fakeSource{movies: []arr.MissingMovie{{ID: 202, Title: "I, Rover", Year: 2004, Available: true}}}
	c, now := newTestCollector(
		Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: son},
		Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: rad},
	)
	ctx := context.Background()
	_, err := c.Get(ctx, false)
	require.NoError(t, err)
	step := func() { *now = now.Add(searchGap) }

	res, err := c.Search(ctx, SearchRequest{Category: "sonarr_main", SeriesID: 1, Season: intp(2)})
	require.NoError(t, err)
	assert.Equal(t, SearchResult{Category: "sonarr_main", Command: "SeasonSearch", CommandID: 11}, res)

	step()
	res, err = c.Search(ctx, SearchRequest{Category: "sonarr_main", SeriesID: 1, Season: intp(3)})
	require.NoError(t, err)
	assert.Equal(t, "EpisodeSearch", res.Command, "a season with one missing episode searches that episode")

	step()
	res, err = c.Search(ctx, SearchRequest{Category: "sonarr_main", SeriesID: 1, EpisodeIDs: []int{102, 101}})
	require.NoError(t, err)
	assert.Equal(t, "EpisodeSearch", res.Command)

	step()
	res, err = c.Search(ctx, SearchRequest{Category: "sonarr_main", SeriesID: 50, Season: intp(0)})
	require.NoError(t, err)
	assert.Equal(t, "SeasonSearch", res.Command, "not in the snapshot: season search")

	step()
	res, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 202})
	require.NoError(t, err)
	assert.Equal(t, SearchResult{Category: "radarr_main", Command: "MoviesSearch", CommandID: 13}, res)

	assert.Equal(t, []string{"SeasonSearch 1 S2", "EpisodeSearch [201]", "EpisodeSearch [101 102]", "SeasonSearch 50 S0"}, son.searches)
	assert.Equal(t, []string{"MoviesSearch [202]"}, rad.searches)
}

func TestSearch_Invalid(t *testing.T) {
	c, _ := newTestCollector(
		Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: sonarrFake()},
		Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: &fakeSource{}},
	)
	ctx := context.Background()
	for _, req := range []SearchRequest{
		{Category: "nope", MovieID: 1},
		{Category: "sonarr_main", MovieID: 1},
		{Category: "sonarr_main", SeriesID: 1},                          // no season, no episodes
		{Category: "sonarr_main", SeriesID: 1, Season: intp(-1)},        // bad season
		{Category: "sonarr_main", SeriesID: 1, EpisodeIDs: []int{1, 0}}, // bad id
		{Category: "radarr_main", SeriesID: 1, Season: intp(1)},
		{Category: "radarr_main"},
	} {
		_, err := c.Search(ctx, req)
		require.ErrorIs(t, err, ErrInvalid, "%+v", req)
	}
}

func TestSearch_RateLimits(t *testing.T) {
	rad := &fakeSource{}
	c, now := newTestCollector(Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: rad})
	ctx := context.Background()
	_, err := c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 1})
	require.NoError(t, err)

	var rl *RateLimitedError
	_, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 2})
	require.ErrorAs(t, err, &rl, "global gap")

	*now = now.Add(searchGap)
	_, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 2})
	require.NoError(t, err)

	*now = now.Add(time.Minute)
	_, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 1})
	require.ErrorAs(t, err, &rl, "per-target cooldown")

	*now = now.Add(searchCooldown)
	_, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 1})
	require.NoError(t, err)

	// A failed command does not start the target's cooldown.
	rad.setErr(errors.New("HTTP 500"))
	*now = now.Add(searchGap)
	_, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 3})
	require.Error(t, err)
	rad.setErr(nil)
	*now = now.Add(searchGap)
	_, err = c.Search(ctx, SearchRequest{Category: "radarr_main", MovieID: 3})
	require.NoError(t, err)
}

// TestEndToEnd_FakeArrs runs a refresh through the real arr client against fake
// Sonarr and Radarr HTTP servers, counting the requests one refresh costs.
func TestEndToEnd_FakeArrs(t *testing.T) {
	var sonarrReqs, radarrReqs atomic.Int32
	sonarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sonarrReqs.Add(1)
		switch r.URL.Path {
		case "/api/v3/wanted/missing":
			_, _ = w.Write([]byte(`{"page":1,"totalRecords":3,"records":[
			  {"id":8647,"seriesId":75,"seasonNumber":5,"episodeNumber":3,"airDateUtc":"2026-10-11T00:00:00Z","hasFile":false,"monitored":true,
			   "series":{"id":75,"title":"The Great Evening Variety Show","year":2024,"tvdbId":9102,"imdbId":"tt9000004","qualityProfileId":14,
			     "seasons":[{"seasonNumber":5,"monitored":true}]}},
			  {"id":8646,"seriesId":75,"seasonNumber":5,"episodeNumber":2,"airDateUtc":"2026-10-04T00:00:00Z","hasFile":false,"monitored":true,
			   "series":{"id":75,"title":"The Great Evening Variety Show","year":2024,"tvdbId":9102,"qualityProfileId":14,"seasons":[{"seasonNumber":5,"monitored":true}]}},
			  {"id":8648,"seriesId":75,"seasonNumber":5,"episodeNumber":4,"airDateUtc":"2026-10-18T00:00:00Z","hasFile":false,"monitored":true,
			   "series":{"id":75,"title":"The Great Evening Variety Show","year":2024,"qualityProfileId":14,"seasons":[]}}]}`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":14,"name":"1080p Foreign Classic SD"}]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"totalRecords":1,"records":[{"id":1,"seriesId":75,"episodeId":8646,"seasonNumber":5}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer sonarr.Close()
	var cmd map[string]any
	radarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		radarrReqs.Add(1)
		switch r.URL.Path {
		case "/api/v3/wanted/missing":
			w.WriteHeader(http.StatusNotFound) // older Radarr: falls back to /movie
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id":202,"title":"I, Rover","year":2004,"monitored":true,"hasFile":false,"isAvailable":true,"qualityProfileId":11}]`))
		case "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[{"id":11,"name":"1080p Quality HDR"}]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"totalRecords":0,"records":[]}`))
		case "/api/v3/command":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &cmd)
			_, _ = w.Write([]byte(`{"id":77}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer radarr.Close()

	c, _ := newTestCollector(
		Arr{Category: "sonarr_main", Kind: arr.KindSonarr, Client: arr.New(arr.KindSonarr, sonarr.URL, "k", nil)},
		Arr{Category: "radarr_main", Kind: arr.KindRadarr, Client: arr.New(arr.KindRadarr, radarr.URL, "k", nil)},
	)
	snap, err := c.Get(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, int32(3), sonarrReqs.Load(), "wanted/missing + qualityprofile + queue")
	assert.Equal(t, int32(4), radarrReqs.Load(), "+1 for the /movie fallback")

	s := snap.Arrs[0]
	require.Len(t, s.Items, 1)
	assert.Equal(t, 2, s.TotalMissing, "E4 airs after now")
	assert.Equal(t, "E2–3", s.Items[0].EpisodeRanges)
	assert.Equal(t, "1080p Foreign Classic SD", s.Items[0].Profile)
	assert.Equal(t, 1, s.Items[0].Queued)
	assert.Equal(t, "The Great Evening Variety Show S05", s.Items[0].SearchText)

	r := snap.Arrs[1]
	require.Len(t, r.Items, 1)
	assert.Equal(t, "I, Rover (2004)", r.Items[0].TargetTitle)

	res, err := c.Search(context.Background(), SearchRequest{Category: "radarr_main", MovieID: 202})
	require.NoError(t, err)
	assert.Equal(t, 77, res.CommandID)
	assert.Equal(t, "MoviesSearch", cmd["name"])

	b, err := json.Marshal(snap)
	require.NoError(t, err)
	for _, k := range []string{`"refreshed_at"`, `"arrs"`, `"total_missing"`, `"episode_ranges":"E2–3"`, `"series_id":75`, `"movie_id":202`} {
		assert.True(t, strings.Contains(string(b), k), "wire format has %s", k)
	}
}
