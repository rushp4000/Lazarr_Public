// Package missing keeps the Web UI "Missing" tab's list: what each arr in
// playback.arrs still wants — monitored, already aired (Sonarr) or available (Radarr),
// and without a file — grouped by series + season or movie, so the owner no longer has
// to work out by hand what to look for before handing a release to the slow lane.
//
// The collector is read-only towards the arrs except for Search, a button-triggered
// SeasonSearch / EpisodeSearch / MoviesSearch command. A refresh asks each arr, one
// after the other, for its wanted/missing list (paged, 500 per page), its quality
// profiles and its queue: about three requests per arr. It runs every
// missing.refresh_interval (default 30m) and on demand from the tab, at most once per
// MinRefreshGap. A failing arr keeps its previous list and reports the error; the
// others are unaffected.
package missing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback/arr"
)

const (
	// MinRefreshGap is the least time between the start of two refreshes requested
	// from the UI (joining one already running is always allowed).
	MinRefreshGap = time.Minute
	// arrTimeout bounds the whole collection from one arr.
	arrTimeout = 2 * time.Minute
	// firstRefreshDelay lets boot finish before the first refresh.
	firstRefreshDelay = 30 * time.Second
	// searchCooldown is how long one series season / movie cannot be searched again.
	searchCooldown = 5 * time.Minute
	// searchGap is the least time between any two search commands.
	searchGap = 3 * time.Second
	// searchTimeout bounds one search command request.
	searchTimeout = 30 * time.Second
	// maxSearchEpisodes caps an explicit EpisodeSearch.
	maxSearchEpisodes = 200
)

// ErrInvalid marks a search request that cannot be served (unknown category, wrong
// target for the arr kind). Errors wrapping it are client errors.
var ErrInvalid = errors.New("missing: invalid request")

// RateLimitedError is returned when a refresh or search comes too soon after the last.
type RateLimitedError struct {
	What string
	Wait time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%s rate-limited: try again in %s", e.What, e.Wait.Round(time.Second))
}

// Source is the slice of an arr client the collector needs (satisfied by *arr.Client).
type Source interface {
	MissingEpisodes(ctx context.Context) ([]arr.MissingEpisode, error)
	MissingMovies(ctx context.Context) ([]arr.MissingMovie, error)
	QualityProfiles(ctx context.Context) (map[int]string, error)
	Queue(ctx context.Context) ([]arr.QueueRecord, error)
	SearchSeason(ctx context.Context, seriesID, season int) (int, error)
	SearchEpisodes(ctx context.Context, episodeIDs []int) (int, error)
	SearchMovies(ctx context.Context, movieIDs []int) (int, error)
}

// Arr is one configured arr instance, labelled by its primary category.
type Arr struct {
	Category string
	Kind     string // arr.KindSonarr | arr.KindRadarr
	Client   Source
}

// Item is one row of the tab: a series season with missing episodes, or a movie.
type Item struct {
	Type   string `json:"type"` // "season" | "movie"
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	TVDBID int    `json:"tvdb_id,omitempty"`
	TMDBID int    `json:"tmdb_id,omitempty"`
	IMDBID string `json:"imdb_id,omitempty"`
	// Profile is the arr's quality profile name ("" if the id is unknown).
	Profile string `json:"profile"`

	// Seasons (Sonarr). Season is also present (0) on movies; check Type.
	SeriesID        int    `json:"series_id,omitempty"`
	Season          int    `json:"season"`
	SeasonMonitored bool   `json:"season_monitored,omitempty"`
	Episodes        []int  `json:"episodes,omitempty"`    // missing episode numbers, ascending
	EpisodeIDs      []int  `json:"episode_ids,omitempty"` // the arr's ids, parallel to Episodes
	EpisodeRanges   string `json:"episode_ranges,omitempty"`
	FirstAired      string `json:"first_aired,omitempty"` // YYYY-MM-DD (UTC)
	LastAired       string `json:"last_aired,omitempty"`

	// Movies (Radarr).
	MovieID int `json:"movie_id,omitempty"`

	Count  int `json:"count"`  // missing episodes (1 for a movie)
	Queued int `json:"queued"` // how many of them are in the arr's queue right now

	// SearchText is what to type into an indexer ("Show Name S02", "Movie Name 2004").
	SearchText string `json:"search_text"`
	// TargetTitle labels a slow-lane target ("Show Name S02", "Movie Name (2004)").
	TargetTitle string `json:"target_title"`
	// Whether an active slow-lane item targets the row is worked out in the browser
	// from GET /api/slowlane (items[].target), so this package stays arr-only.
}

// ArrMissing is one arr's part of the snapshot.
type ArrMissing struct {
	Category string `json:"category"`
	Kind     string `json:"kind"`
	// Error is the last refresh's failure; Items then still hold the last good list
	// (RefreshedAt says how old it is; 0 = never fetched).
	Error        string `json:"error,omitempty"`
	TotalMissing int    `json:"total_missing"` // episodes (Sonarr) or movies (Radarr)
	Queued       int    `json:"queued"`        // of those, in the arr's queue
	RefreshedAt  int64  `json:"refreshed_at"`  // unix seconds of this arr's last good fetch
	Items        []Item `json:"items"`
}

// Snapshot is the GET /api/missing body.
type Snapshot struct {
	Enabled         bool         `json:"enabled"`
	RefreshedAt     int64        `json:"refreshed_at"` // unix seconds of the last finished refresh; 0 = none yet
	Refreshing      bool         `json:"refreshing"`
	RefreshInterval string       `json:"refresh_interval"`
	Arrs            []ArrMissing `json:"arrs"`
}

// SearchRequest is the POST /api/missing/search body: {category, series_id, season}
// (optionally episode_ids) for Sonarr, {category, movie_id} for Radarr.
type SearchRequest struct {
	Category   string `json:"category"`
	SeriesID   int    `json:"series_id,omitempty"`
	Season     *int   `json:"season,omitempty"` // pointer: season 0 (specials) is valid
	EpisodeIDs []int  `json:"episode_ids,omitempty"`
	MovieID    int    `json:"movie_id,omitempty"`
}

// SearchResult is what Search asked the arr to do.
type SearchResult struct {
	Category  string `json:"category"`
	Command   string `json:"command"`
	CommandID int    `json:"command_id"`
}

// Collector holds the snapshot and refreshes it.
type Collector struct {
	arrs     []Arr
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex
	base      context.Context // Start's ctx: refreshes outlive the HTTP request that asked
	snap      Snapshot
	running   chan struct{} // closed when the refresh in flight finishes; nil = idle
	lastStart time.Time

	searchMu   sync.Mutex
	lastSearch time.Time
	searched   map[string]time.Time
}

// New builds a collector over arrs, refreshed every interval.
func New(arrs []Arr, interval time.Duration) *Collector {
	c := &Collector{
		arrs:     arrs,
		interval: interval,
		now:      time.Now,
		base:     context.Background(),
		searched: map[string]time.Time{},
	}
	c.snap = Snapshot{Enabled: true, RefreshInterval: interval.String(), Arrs: make([]ArrMissing, len(arrs))}
	for i, a := range arrs {
		c.snap.Arrs[i] = ArrMissing{Category: a.Category, Kind: a.Kind, Items: []Item{}}
	}
	return c
}

// Start refreshes shortly after boot, then every interval, until ctx is canceled.
func (c *Collector) Start(ctx context.Context) {
	c.mu.Lock()
	c.base = ctx
	c.mu.Unlock()
	go func() {
		first := time.NewTimer(firstRefreshDelay)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		t := time.NewTicker(c.interval)
		defer t.Stop()
		for {
			c.mu.Lock()
			recent := !c.lastStart.IsZero() && c.now().Sub(c.lastStart) < MinRefreshGap
			c.mu.Unlock()
			if !recent { // e.g. the tab was opened right at boot and fetched it already
				_ = c.Refresh(ctx, false)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Snapshot returns the current snapshot (a copy; item slices are never mutated).
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.snap
	s.Refreshing = c.running != nil
	s.Arrs = slices.Clone(c.snap.Arrs)
	return s
}

// Get returns the snapshot, first running a refresh (and waiting for it while ctx
// lasts) when none has finished yet. On demand (refresh=true) it starts a new one,
// at most once per MinRefreshGap, and waits for it; a refresh already in flight is
// joined instead. When ctx ends first the snapshot comes back with Refreshing set.
func (c *Collector) Get(ctx context.Context, refresh bool) (Snapshot, error) {
	c.mu.Lock()
	never := c.snap.RefreshedAt == 0
	c.mu.Unlock()
	if refresh || never {
		err := c.Refresh(ctx, refresh)
		var rl *RateLimitedError
		if errors.As(err, &rl) {
			return c.Snapshot(), err
		}
	}
	return c.Snapshot(), nil
}

// Refresh starts a refresh, or joins the one in flight, and waits for it while ctx
// lasts. manual applies MinRefreshGap. The collection itself runs on Start's context,
// so a browser that goes away does not cut it short.
func (c *Collector) Refresh(ctx context.Context, manual bool) error {
	c.mu.Lock()
	if c.running == nil {
		if manual && !c.lastStart.IsZero() {
			if wait := MinRefreshGap - c.now().Sub(c.lastStart); wait > 0 {
				c.mu.Unlock()
				return &RateLimitedError{What: "refresh", Wait: wait}
			}
		}
		done := make(chan struct{})
		c.running = done
		c.lastStart = c.now()
		base := c.base
		go c.run(base, done)
	}
	done := c.running
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run collects every arr, one after the other, and publishes the result.
func (c *Collector) run(ctx context.Context, done chan struct{}) {
	start := c.now()
	results := make([]ArrMissing, len(c.arrs))
	errs := make([]error, len(c.arrs))
	for i, a := range c.arrs {
		actx, cancel := context.WithTimeout(ctx, arrTimeout)
		results[i], errs[i] = c.collect(actx, a)
		cancel()
	}

	c.mu.Lock()
	total, failed := 0, 0
	for i, a := range c.arrs {
		if errs[i] != nil {
			failed++
			prev := c.snap.Arrs[i]
			prev.Error = errs[i].Error()
			c.snap.Arrs[i] = prev
			total += prev.TotalMissing
			metrics.IncMissingRefresh(a.Category, "error")
			slog.Warn("missing: refresh failed for arr; keeping its previous list", "arr", a.Category, "err", errs[i])
			continue
		}
		c.snap.Arrs[i] = results[i]
		total += results[i].TotalMissing
		metrics.IncMissingRefresh(a.Category, "ok")
		metrics.SetMissingItems(a.Category, results[i].TotalMissing)
	}
	end := c.now()
	c.snap.RefreshedAt = end.Unix()
	c.running = nil
	c.mu.Unlock()
	slog.Info("missing: refreshed", "arrs", len(c.arrs), "failed", failed, "missing", total,
		"took", end.Sub(start).Round(time.Millisecond))
	close(done)
}

// collect fetches one arr's list.
func (c *Collector) collect(ctx context.Context, a Arr) (ArrMissing, error) {
	out := ArrMissing{Category: a.Category, Kind: a.Kind, Items: []Item{}}
	var err error
	if a.Kind == arr.KindSonarr {
		err = c.collectSonarr(ctx, a.Client, &out)
	} else {
		err = c.collectRadarr(ctx, a.Client, &out)
	}
	if err != nil {
		return out, err
	}
	out.RefreshedAt = c.now().Unix()
	return out, nil
}

func (c *Collector) collectSonarr(ctx context.Context, src Source, out *ArrMissing) error {
	eps, err := src.MissingEpisodes(ctx)
	if err != nil {
		return err
	}
	profiles, err := src.QualityProfiles(ctx)
	if err != nil {
		return err
	}
	queue, err := src.Queue(ctx)
	if err != nil {
		return err
	}
	type key struct{ series, season int }
	queuedEp := map[int]bool{}
	queuedSeason := map[key]bool{} // records without an episode id cover the season
	for _, r := range queue {
		switch {
		case r.EpisodeID > 0:
			queuedEp[r.EpisodeID] = true
		case r.SeriesID > 0:
			queuedSeason[key{r.SeriesID, r.SeasonNumber}] = true
		}
	}

	now := c.now()
	groups := map[key]*Item{}
	type epRef struct {
		num, id int
		aired   time.Time
	}
	epsOf := map[key][]epRef{}
	for _, e := range eps {
		if e.AirDateUtc.IsZero() || !e.AirDateUtc.Before(now) {
			continue // unaired or TBA: nothing to find yet
		}
		k := key{e.SeriesID, e.SeasonNumber}
		it := groups[k]
		if it == nil {
			s := e.Series
			title := s.Title
			if title == "" {
				title = "Series " + strconv.Itoa(e.SeriesID)
			}
			it = &Item{
				Type: "season", Title: title, Year: s.Year, TVDBID: s.TVDBID, IMDBID: s.IMDBID,
				Profile: profiles[s.QualityProfileID], SeriesID: e.SeriesID, Season: e.SeasonNumber,
				SeasonMonitored: s.SeasonMonitored[e.SeasonNumber],
				SearchText:      fmt.Sprintf("%s S%02d", searchable(title), e.SeasonNumber),
				TargetTitle:     fmt.Sprintf("%s S%02d", title, e.SeasonNumber),
			}
			groups[k] = it
		}
		epsOf[k] = append(epsOf[k], epRef{e.EpisodeNumber, e.ID, e.AirDateUtc})
		it.Count++
		out.TotalMissing++
		if queuedEp[e.ID] || queuedSeason[k] {
			it.Queued++
			out.Queued++
		}
	}
	for k, it := range groups {
		refs := epsOf[k]
		sort.Slice(refs, func(i, j int) bool { return refs[i].num < refs[j].num })
		first, last := refs[0].aired, refs[0].aired
		for _, r := range refs {
			it.Episodes = append(it.Episodes, r.num)
			it.EpisodeIDs = append(it.EpisodeIDs, r.id)
			if r.aired.Before(first) {
				first = r.aired
			}
			if r.aired.After(last) {
				last = r.aired
			}
		}
		it.EpisodeRanges = EpisodeRanges(it.Episodes)
		it.FirstAired = first.UTC().Format(time.DateOnly)
		it.LastAired = last.UTC().Format(time.DateOnly)
		out.Items = append(out.Items, *it)
	}
	sortItems(out.Items)
	return nil
}

func (c *Collector) collectRadarr(ctx context.Context, src Source, out *ArrMissing) error {
	movies, err := src.MissingMovies(ctx)
	if err != nil {
		return err
	}
	profiles, err := src.QualityProfiles(ctx)
	if err != nil {
		return err
	}
	queue, err := src.Queue(ctx)
	if err != nil {
		return err
	}
	queued := map[int]bool{}
	for _, r := range queue {
		if r.MovieID > 0 {
			queued[r.MovieID] = true
		}
	}
	for _, m := range movies {
		if !m.Available {
			continue // not released yet (minimum availability): nothing to find
		}
		it := Item{
			Type: "movie", Title: m.Title, Year: m.Year, TMDBID: m.TMDBID, IMDBID: m.IMDBID,
			Profile: profiles[m.QualityProfileID], MovieID: m.ID, Count: 1,
			SearchText:  strings.TrimSpace(searchable(m.Title) + " " + yearStr(m.Year)),
			TargetTitle: m.Title,
		}
		if m.Year > 0 {
			it.TargetTitle = fmt.Sprintf("%s (%d)", m.Title, m.Year)
		}
		if queued[m.ID] {
			it.Queued = 1
			out.Queued++
		}
		out.TotalMissing++
		out.Items = append(out.Items, it)
	}
	sortItems(out.Items)
	return nil
}

func yearStr(y int) string {
	if y <= 0 {
		return ""
	}
	return strconv.Itoa(y)
}

// sortItems orders rows by title (case-insensitive), then year, then season.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := strings.ToLower(items[i].Title), strings.ToLower(items[j].Title)
		if a != b {
			return a < b
		}
		if items[i].Year != items[j].Year {
			return items[i].Year < items[j].Year
		}
		return items[i].Season < items[j].Season
	})
}

// EpisodeRanges renders episode numbers compactly: [1 2 3 4 8 10 11 12] ->
// "E1–4, E8, E10–12". Input need not be sorted; duplicates are ignored.
func EpisodeRanges(nums []int) string {
	if len(nums) == 0 {
		return ""
	}
	s := slices.Clone(nums)
	slices.Sort(s)
	s = slices.Compact(s)
	var parts []string
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && s[j+1] == s[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, fmt.Sprintf("E%d", s[i]))
		} else {
			parts = append(parts, fmt.Sprintf("E%d–%d", s[i], s[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// searchable turns a title into indexer search text: apostrophes dropped
// ("Grady's" -> "Gradys"), other punctuation except '.', '-' and '&' becomes a space.
func searchable(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch {
		case r == '\'' || r == '’':
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '&':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Search asks the arr behind req.Category to search for one row:
//   - Sonarr {series_id, episode_ids}: EpisodeSearch for those episodes;
//   - Sonarr {series_id, season}: EpisodeSearch when the snapshot has exactly one
//     missing episode for that season, otherwise SeasonSearch (what Sonarr's own
//     missing search does for a season with several missing episodes);
//   - Radarr {movie_id}: MoviesSearch.
//
// It is the collector's only write to an arr. One target is searched at most once per
// searchCooldown and any two searches are at least searchGap apart.
func (c *Collector) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	res := SearchResult{Category: req.Category}
	idx := slices.IndexFunc(c.arrs, func(a Arr) bool { return a.Category == req.Category })
	if idx < 0 {
		return res, fmt.Errorf("%w: unknown arr category %q", ErrInvalid, req.Category)
	}
	a := c.arrs[idx]

	var target string
	var run func(ctx context.Context) (int, error)
	switch a.Kind {
	case arr.KindSonarr:
		if req.SeriesID <= 0 || req.MovieID != 0 {
			return res, fmt.Errorf("%w: %s is a Sonarr: give series_id and season (or episode_ids)", ErrInvalid, a.Category)
		}
		switch {
		case len(req.EpisodeIDs) > 0:
			if len(req.EpisodeIDs) > maxSearchEpisodes || slices.ContainsFunc(req.EpisodeIDs, func(id int) bool { return id <= 0 }) {
				return res, fmt.Errorf("%w: episode_ids must be 1..%d positive ids", ErrInvalid, maxSearchEpisodes)
			}
			ids := slices.Clone(req.EpisodeIDs)
			slices.Sort(ids)
			target = fmt.Sprintf("series:%d:eps:%v", req.SeriesID, ids)
			res.Command = "EpisodeSearch"
			run = func(ctx context.Context) (int, error) { return a.Client.SearchEpisodes(ctx, ids) }
		case req.Season != nil && *req.Season >= 0:
			season := *req.Season
			target = fmt.Sprintf("series:%d:season:%d", req.SeriesID, season)
			if it, ok := c.find(idx, func(it Item) bool { return it.SeriesID == req.SeriesID && it.Season == season }); ok && len(it.EpisodeIDs) == 1 {
				id := it.EpisodeIDs[0]
				res.Command = "EpisodeSearch"
				run = func(ctx context.Context) (int, error) { return a.Client.SearchEpisodes(ctx, []int{id}) }
			} else {
				res.Command = "SeasonSearch"
				run = func(ctx context.Context) (int, error) { return a.Client.SearchSeason(ctx, req.SeriesID, season) }
			}
		default:
			return res, fmt.Errorf("%w: give season or episode_ids", ErrInvalid)
		}
	default:
		if req.MovieID <= 0 || req.SeriesID != 0 {
			return res, fmt.Errorf("%w: %s is a Radarr: give movie_id", ErrInvalid, a.Category)
		}
		target = "movie:" + strconv.Itoa(req.MovieID)
		res.Command = "MoviesSearch"
		run = func(ctx context.Context) (int, error) { return a.Client.SearchMovies(ctx, []int{req.MovieID}) }
	}
	key := a.Category + "|" + target

	c.searchMu.Lock()
	now := c.now()
	if t, ok := c.searched[key]; ok && now.Sub(t) < searchCooldown {
		c.searchMu.Unlock()
		return res, &RateLimitedError{What: "search for this item", Wait: searchCooldown - now.Sub(t)}
	}
	if wait := searchGap - now.Sub(c.lastSearch); !c.lastSearch.IsZero() && wait > 0 {
		c.searchMu.Unlock()
		return res, &RateLimitedError{What: "search", Wait: wait}
	}
	c.lastSearch = now
	for k, t := range c.searched { // keep the map small
		if now.Sub(t) >= searchCooldown {
			delete(c.searched, k)
		}
	}
	c.searchMu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	id, err := run(sctx)
	if err != nil {
		metrics.IncMissingSearch(res.Command, "error")
		slog.Warn("missing: search command failed", "arr", a.Category, "command", res.Command, "target", target, "err", err)
		return res, err
	}
	c.searchMu.Lock()
	c.searched[key] = c.now()
	c.searchMu.Unlock()
	res.CommandID = id
	metrics.IncMissingSearch(res.Command, "ok")
	slog.Info("missing: asked the arr to search", "arr", a.Category, "command", res.Command, "target", target, "command_id", id)
	return res, nil
}

// find returns the first item of arr idx in the current snapshot matching f.
func (c *Collector) find(idx int, f func(Item) bool) (Item, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, it := range c.snap.Arrs[idx].Items {
		if f(it) {
			return it, true
		}
	}
	return Item{}, false
}
