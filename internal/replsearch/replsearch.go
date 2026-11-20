// Package replsearch is the replacement search (repair.search_replacement).
//
// When TorBox drops content a library file depends on, Lazarr removes the dead library
// link (policy.repair_unlink_dead or repair.unlink_orphans) and asks the owning arr to
// rescan, so the arr marks the episode/movie missing. Nothing then searches for it
// until the arr's own schedule or a person does. This package closes that gap, within
// hard limits, because TorBox flags misconfigured automated tools:
//
//   - The episodes/movies are read from the arr's file records BEFORE its rescan
//     deletes them (Collect), and queued only for rescans the arr accepted (Enqueue).
//   - A worker (Tick) waits for the rescan to land, then searches only items the arr
//     still monitors and now shows without a file.
//   - One search per item per cooldown, persisted in SQLite, so a replacement that dies
//     again cannot loop; a global per-hour cap; at most maxPending queued items.
//
// It never touches TorBox. The search is a plain EpisodeSearch / MoviesSearch; the arr's
// grab goes through Lazarr's normal add path like any other grab.
package replsearch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/your-username/lazarr/internal/arrroots"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback/arr"
)

const (
	// TickEvery is how often Run processes the queue.
	TickEvery = 5 * time.Minute
	// settle is how long a queued item waits for the arr's rescan to land.
	settle = 10 * time.Minute
	// maxPending bounds the queue; new items beyond it are dropped.
	maxPending = 200
	// maxAttempts bounds arr errors and "still has a file" checks per item.
	maxAttempts = 3
	// expireAfter drops an item that could not be searched for this long.
	expireAfter = 24 * time.Hour
	// batchLimit bounds the rows read per tick.
	batchLimit = 100
)

// Arr is the slice of an arr client the replacement search needs (*arr.Client).
type Arr interface {
	Kind() string
	FileOwners(ctx context.Context, itemID int, arrPaths []string) ([]int, error)
	ItemStates(ctx context.Context, ids []int) (map[int]arr.ItemState, error)
	SearchItems(ctx context.Context, ids []int) error
}

// Candidate is one episode (Sonarr) or movie (Radarr) whose library file was removed,
// with the series/movie (Owner) whose rescan makes it missing.
type Candidate struct {
	Category string
	ItemID   int
	Owner    int
	Kind     string // "episode" | "movie"
}

// Searcher queues and runs replacement searches.
type Searcher struct {
	Store      catalog.ReplacementStore
	Arrs       map[string]Arr // by category
	MaxPerHour int
	Cooldown   time.Duration
	Now        func() time.Time
}

// Collect reads, from each target's arr, which episodes/movies the removed paths were.
// It must run before the rescan (which deletes the arr's file records). Arr errors are
// logged and that target is skipped.
func (s *Searcher) Collect(ctx context.Context, targets []arrroots.Target) []Candidate {
	var out []Candidate
	for _, t := range targets {
		a := s.Arrs[t.Category]
		if a == nil {
			continue
		}
		ids, err := a.FileOwners(ctx, t.ItemID, t.ArrPaths)
		if err != nil {
			slog.Warn("replacement search: read the arr's files before rescan", "arr", t.Category, "id", t.ItemID, "err", err)
			metrics.AddReplacementSearch("error", 1)
			continue
		}
		kind := "movie"
		if a.Kind() == arr.KindSonarr {
			kind = "episode"
		}
		for _, id := range ids {
			out = append(out, Candidate{Category: t.Category, ItemID: id, Owner: t.ItemID, Kind: kind})
		}
	}
	return out
}

// Enqueue queues the candidates whose series/movie rescan the arr accepted. An item
// searched within the cooldown is not queued again. Returns how many were queued.
func (s *Searcher) Enqueue(cands []Candidate, rescanned []arrroots.Target) int {
	type owner struct {
		category string
		id       int
	}
	ok := make(map[owner]bool, len(rescanned))
	for _, t := range rescanned {
		ok[owner{t.Category, t.ItemID}] = true
	}
	now := s.now()
	queued := 0
	for _, c := range cands {
		if !ok[owner{c.Category, c.Owner}] {
			continue
		}
		if s.enqueue(c, now) {
			queued++
		}
	}
	if queued > 0 {
		slog.Info("replacement search: queued", "items", queued)
	}
	return queued
}

func (s *Searcher) enqueue(c Candidate, now time.Time) bool {
	it, err := s.Store.GetReplacement(c.Category, c.ItemID)
	switch {
	case err == nil && it.State == catalog.ReplacementPending:
		return false // already queued
	case err == nil && s.inCooldown(it, now):
		slog.Info("replacement search: skipped, searched recently", "arr", c.Category, "id", c.ItemID,
			"searched_at", time.Unix(it.SearchedAt, 0).UTC())
		metrics.AddReplacementSearch("cooldown", 1)
		return false
	case err != nil && !errors.Is(err, catalog.ErrNotFound):
		slog.Warn("replacement search: read queue", "err", err)
		return false
	}
	if n, cerr := s.Store.CountPendingReplacements(); cerr != nil || n >= maxPending {
		slog.Warn("replacement search: queue full, dropped", "arr", c.Category, "id", c.ItemID, "pending", n, "err", cerr)
		metrics.AddReplacementSearch("dropped", 1)
		return false
	}
	row := &catalog.ReplacementItem{Category: c.Category, ItemID: c.ItemID, Kind: c.Kind,
		State: catalog.ReplacementPending, QueuedAt: now.Unix(), Result: "queued"}
	if it != nil {
		row.SearchedAt = it.SearchedAt // keep the cooldown stamp
	}
	if err := s.Store.UpsertReplacement(row); err != nil {
		slog.Warn("replacement search: queue", "err", err)
		return false
	}
	metrics.AddReplacementSearch("queued", 1)
	return true
}

// Run calls Tick every TickEvery until ctx is done.
func (s *Searcher) Run(ctx context.Context) {
	t := time.NewTicker(TickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick processes due pending items within the hourly cap and prunes old rows.
func (s *Searcher) Tick(ctx context.Context) {
	now := s.now()
	if _, err := s.Store.PruneReplacements(now.Add(-(s.Cooldown + 24*time.Hour)).Unix()); err != nil {
		slog.Warn("replacement search: prune", "err", err)
	}
	items, err := s.Store.ListPendingReplacements(now.Add(-settle).Unix(), batchLimit)
	if err != nil {
		slog.Warn("replacement search: list queue", "err", err)
		return
	}
	if len(items) == 0 {
		return
	}
	used, err := s.Store.CountReplacementSearchesSince(now.Add(-time.Hour).Unix())
	if err != nil {
		slog.Warn("replacement search: count recent searches", "err", err)
		return
	}
	budget := s.MaxPerHour - used

	var order []string
	byArr := map[string][]*catalog.ReplacementItem{}
	for _, it := range items {
		if _, seen := byArr[it.Category]; !seen {
			order = append(order, it.Category)
		}
		byArr[it.Category] = append(byArr[it.Category], it)
	}
	deferred := 0
	for _, cat := range order {
		budget, deferred = s.tickArr(ctx, cat, byArr[cat], budget, deferred, now)
	}
	if deferred > 0 {
		slog.Info("replacement search: hourly cap reached, rest deferred", "deferred", deferred, "max_per_hour", s.MaxPerHour)
		metrics.AddReplacementSearch("rate_limited", deferred)
	}
}

func (s *Searcher) tickArr(ctx context.Context, cat string, items []*catalog.ReplacementItem, budget, deferred int, now time.Time) (int, int) {
	a := s.Arrs[cat]
	var check []*catalog.ReplacementItem
	for _, it := range items {
		switch {
		case a == nil:
			s.finish(it, "gone")
		case s.inCooldown(it, now):
			s.finish(it, "cooldown")
		case now.Sub(time.Unix(it.QueuedAt, 0)) > expireAfter:
			s.finish(it, "expired")
		case len(check) >= budget:
			deferred++ // stays pending for a later tick
		default:
			check = append(check, it)
		}
	}
	if len(check) == 0 {
		return budget, deferred
	}
	ids := make([]int, len(check))
	for i, it := range check {
		ids[i] = it.ItemID
	}
	states, err := a.ItemStates(ctx, ids)
	if err != nil {
		slog.Warn("replacement search: read item state", "arr", cat, "err", err)
		s.retry(check, "error")
		return budget, deferred
	}
	var due []*catalog.ReplacementItem
	for _, it := range check {
		st, ok := states[it.ItemID]
		switch {
		case !ok:
			s.finish(it, "gone")
		case !st.Monitored:
			s.finish(it, "unmonitored")
		case st.HasFile:
			// The rescan may not have landed yet, or the arr already replaced the file.
			s.retry([]*catalog.ReplacementItem{it}, "has_file")
		default:
			due = append(due, it)
		}
	}
	if len(due) == 0 {
		return budget, deferred
	}
	dueIDs := make([]int, len(due))
	for i, it := range due {
		dueIDs[i] = it.ItemID
	}
	if err := a.SearchItems(ctx, dueIDs); err != nil {
		slog.Warn("replacement search: search", "arr", cat, "ids", dueIDs, "err", err)
		s.retry(due, "error")
		return budget, deferred
	}
	for _, it := range due {
		it.State, it.SearchedAt, it.Result = catalog.ReplacementDone, now.Unix(), "searched"
		if err := s.Store.UpsertReplacement(it); err != nil {
			slog.Warn("replacement search: record search", "err", err)
		}
	}
	metrics.AddReplacementSearch("searched", len(due))
	slog.Info("replacement search: asked the arr to search for a replacement", "arr", cat, "kind", due[0].Kind, "ids", dueIDs)
	return budget - len(due), deferred
}

// retry counts an attempt; at maxAttempts the item is finished with result.
func (s *Searcher) retry(items []*catalog.ReplacementItem, result string) {
	for _, it := range items {
		it.Attempts++
		if it.Attempts >= maxAttempts {
			s.finish(it, result)
			continue
		}
		it.Result = result
		if err := s.Store.UpsertReplacement(it); err != nil {
			slog.Warn("replacement search: update queue", "err", err)
		}
	}
}

func (s *Searcher) finish(it *catalog.ReplacementItem, result string) {
	it.State, it.Result = catalog.ReplacementDone, result
	if err := s.Store.UpsertReplacement(it); err != nil {
		slog.Warn("replacement search: update queue", "err", err)
	}
	metrics.AddReplacementSearch(result, 1)
	if result != "searched" {
		slog.Info("replacement search: not searched", "arr", it.Category, "id", it.ItemID, "reason", result)
	}
}

func (s *Searcher) inCooldown(it *catalog.ReplacementItem, now time.Time) bool {
	return it.SearchedAt > 0 && now.Sub(time.Unix(it.SearchedAt, 0)) < s.Cooldown
}

func (s *Searcher) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
