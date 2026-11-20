package qbit

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
)

// waitPollInterval is how often the wait-poller checks TorBox's download progress
// for on_cache_miss=wait grabs. One MyListByID call per in-flight download per tick.
const waitPollInterval = 20 * time.Second

// etaUnknownSentinel is the ETA TorBox reports (8640000s = 100 days) while it has no
// real estimate yet — typically the first minutes after an uncached add, before any
// peers connect. It is not a prediction, so it must never trigger the predictive
// ETA-over-budget bail; the hard budget deadline still applies. Observed live
// 2026-09-12: 14 of 15 wait downloads bailed ~18s after the add on this value.
const etaUnknownSentinel = 8640000

// waitStallWindow is how long a wait download may go without its progress rising
// before it counts as stalled. Past cache_wait_budget only a download that advanced
// within this window keeps waiting (up to cache_wait_max). Observed live
// 2026-10-05: The Glass Garden was bailed at 91% exactly at a 10m budget.
const waitStallWindow = 3 * time.Minute

// waitProgress is the in-memory progress view for one downloading release. It is
// deliberately NOT persisted: after a restart the poller re-learns it on the first
// tick from TorBox (the catalog row in StateDownloading is the durable part).
type waitProgress struct {
	Progress    float64   // 0..1
	ETA         int64     // seconds, 0 = unknown
	LastAdvance time.Time // last time Progress rose (zero = never observed)
	Extended    bool      // already logged the past-budget extension
}

// waitPool tracks on_cache_miss=wait downloads: TorBox is fetching an uncached
// torrent; we report real progress to the arr and either complete the grab when the
// content lands in TorBox's cache or bail (delete) when the ETA exceeds the budget.
//
// ToS note: a waiting download legitimately sits on the account while TorBox fetches
// it. The audit counts StateDownloading rows as believed-held, and the poller ALWAYS
// removes the torrent when done — completion flips the grab back to a normal lazy
// (released) import, because finishing the download is exactly what populates
// TorBox's cache.
type waitPool struct {
	mu   sync.Mutex
	prog map[string]waitProgress // hash -> live progress
}

func newWaitPool() *waitPool {
	return &waitPool{prog: make(map[string]waitProgress)}
}

func (w *waitPool) get(hash string) (waitProgress, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.prog[hash]
	return p, ok
}

func (w *waitPool) set(hash string, p waitProgress) {
	w.mu.Lock()
	w.prog[hash] = p
	w.mu.Unlock()
}

func (w *waitPool) drop(hash string) {
	w.mu.Lock()
	delete(w.prog, hash)
	w.mu.Unlock()
}

// startWaitDownload begins an on_cache_miss=wait grab: add the uncached torrent to
// TorBox (it starts downloading server-side) and store the release as
// StateDownloading. When it cannot start it returns false and why
// (wait_overflow | wait_start_failed); the caller refuses the add or errors the row.
func (s *server) startWaitDownload(rel *catalog.Release) (bool, string) {
	rows, err := s.deps.Store.DownloadingReleases()
	if err != nil {
		slog.Warn("qbit: wait start: list downloading", "err", err)
		metrics.IncWait("start_failed", "")
		return false, "wait_start_failed"
	}
	max := s.deps.Config.Policy.MaxWaitDownloads
	if max <= 0 {
		max = 1
	}
	if len(rows) >= max {
		slog.Info("qbit: wait cap reached, not starting another download",
			"hash", rel.Hash, "in_flight", len(rows), "max", max)
		metrics.IncWait("rejected_overflow", "")
		return false, "wait_overflow"
	}

	id, _, err := s.deps.TorBox.CreateTorrent(rel.Magnet, false /* uncached add */)
	if err != nil {
		slog.Warn("qbit: wait start: createtorrent", "hash", rel.Hash, "err", err)
		metrics.IncWait("start_failed", "")
		return false, "wait_start_failed"
	}
	rel.State = catalog.StateDownloading
	rel.TorBoxID = id
	started := s.now()
	rel.MaterializedAt = started.Unix() // on-account-since; budget measured from here
	s.waits.set(rel.Hash, waitProgress{LastAdvance: started})
	slog.Info("qbit: cache miss → TorBox downloading (on_cache_miss=wait)",
		"hash", rel.Hash, "name", rel.Name, "torbox_id", id,
		"budget", s.deps.Config.Policy.CacheWaitBudget.D(),
		"max", s.deps.Config.Policy.EffectiveCacheWaitMax())
	metrics.IncWait("started", "")
	return true, ""
}

// StartWaitPoller launches the background loop that watches StateDownloading
// releases. Safe to call regardless of mode: with nothing downloading, a tick is one
// cheap store query. Stops when ctx is canceled.
func (s *server) StartWaitPoller(ctx context.Context) {
	s.fails.ctx = ctx // set before the poller (the only caller of the fail hook) starts
	go func() {
		t := time.NewTicker(waitPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.pollWaitDownloads()
			}
		}
	}()
}

// pollWaitDownloads advances every in-flight wait download by one observation.
func (s *server) pollWaitDownloads() {
	rows, err := s.deps.Store.DownloadingReleases()
	if err != nil {
		slog.Warn("qbit: wait-poll: list downloading", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	budget := s.deps.Config.Policy.CacheWaitBudget.D()
	waitMax := s.deps.Config.Policy.EffectiveCacheWaitMax()
	now := s.now()

	for _, rel := range rows {
		s.pollOne(rel, budget, waitMax, now)
	}
}

// pollOne advances one wait download under its hash lock. The row is re-read first:
// a torrents/delete or a re-grab may have changed it since the list query.
func (s *server) pollOne(listed *catalog.Release, budget, waitMax time.Duration, now time.Time) {
	unlock := s.locks.lock(listed.Hash)
	defer unlock()
	rel, _, err := s.deps.Store.GetRelease(listed.Hash)
	if err != nil || rel == nil || rel.State != catalog.StateDownloading {
		return
	}
	if rel.DetachedAt != 0 {
		// The arr deleted it but TorBox refused the transfer delete: retry until it goes.
		s.cancelWaitDownload(rel)
		return
	}
	detail, err := s.deps.TorBox.MyListByID(rel.TorBoxID)
	if err != nil {
		slog.Warn("qbit: wait-poll: mylist byid", "hash", rel.Hash, "torbox_id", rel.TorBoxID, "err", err)
		return // transient; try again next tick (budget still applies)
	}
	if detail == nil {
		// Deleted out-of-band (user, TorBox). Nothing to release; surface as error
		// so the arr/Cleanuparr can re-search.
		slog.Warn("qbit: wait download vanished from account", "hash", rel.Hash, "torbox_id", rel.TorBoxID)
		s.failWaitDownload(rel, "vanished")
		return
	}

	if detail.DownloadFinished || detail.DownloadState == "cached" ||
		detail.DownloadState == "completed" || detail.DownloadState == "uploading" {
		s.finishWaitDownload(rel)
		return
	}

	// Still downloading: refresh the live progress for torrents/info, remembering
	// when progress last rose. With no prior observation (first tick, or after a
	// restart wiped the in-memory pool) give it the benefit of the doubt: now.
	prev, seen := s.waits.get(rel.Hash)
	cur := waitProgress{Progress: detail.Progress, ETA: detail.ETA,
		LastAdvance: prev.LastAdvance, Extended: prev.Extended}
	if !seen || prev.LastAdvance.IsZero() || detail.Progress > prev.Progress {
		cur.LastAdvance = now
	}
	s.waits.set(rel.Hash, cur)

	// Budget enforcement, measured from when the download started
	// (MaterializedAt doubles as on-account-since for downloading rows):
	//  - ceiling: cache_wait_max has elapsed — bail no matter what;
	//  - budget: cache_wait_budget has elapsed AND progress has not risen within
	//    waitStallWindow — a download still moving gets extra time up to the
	//    ceiling instead of being thrown away near the finish line;
	//  - predictive: TorBox's ETA says it cannot finish before the ceiling.
	started := time.Unix(rel.MaterializedAt, 0)
	deadline := started.Add(budget)
	ceiling := started.Add(waitMax)
	stalled := now.Sub(cur.LastAdvance) >= waitStallWindow
	noProgress := s.deps.Config.Policy.WaitNoProgress.D()
	deadSwarm := noProgress > 0 && detail.Seeds == 0 && detail.Peers == 0 &&
		now.Sub(cur.LastAdvance) >= noProgress
	switch {
	case deadSwarm:
		// A 0-seeder release (Prowlarr min-seeders 0 makes these the arr's top pick for
		// ~65 missing items, review §9) would otherwise sit out the whole budget.
		slog.Info("qbit: wait download has no progress and no peers, bailing early",
			"hash", rel.Hash, "progress", detail.Progress,
			"since_advance", now.Sub(cur.LastAdvance).Round(time.Second), "after", noProgress)
		s.bailWaitDownload(rel, "no progress, no peers")
	case !now.Before(ceiling):
		slog.Info("qbit: wait download hit cache_wait_max, bailing",
			"hash", rel.Hash, "progress", detail.Progress, "max", waitMax)
		s.bailWaitDownload(rel, "max wait elapsed")
	case now.After(deadline) && stalled:
		slog.Info("qbit: wait download over budget and stalled, bailing",
			"hash", rel.Hash, "progress", detail.Progress, "budget", budget,
			"since_advance", now.Sub(cur.LastAdvance).Round(time.Second))
		s.bailWaitDownload(rel, "budget elapsed")
	case detail.ETA > 0 && detail.ETA < etaUnknownSentinel &&
		now.Add(time.Duration(detail.ETA)*time.Second).After(ceiling):
		slog.Info("qbit: wait download ETA exceeds max wait, bailing",
			"hash", rel.Hash, "eta_s", detail.ETA, "max", waitMax)
		s.bailWaitDownload(rel, "eta over budget")
	case now.After(deadline) && !cur.Extended:
		cur.Extended = true // log the extension once, not every tick
		s.waits.set(rel.Hash, cur)
		slog.Info("qbit: wait download over budget but progressing, extending",
			"hash", rel.Hash, "progress", detail.Progress, "budget", budget,
			"max", waitMax)
	}
}

// finishWaitDownload completes a wait grab: the content is now in TorBox's cache, so
// release the account copy and flip the grab into the normal lazy-virtual shape
// (checkcached file list -> symlinks -> StateVirtual). Playback later re-adds it
// instantly, exactly like any cached grab.
func (s *server) finishWaitDownload(rel *catalog.Release) {
	if err := s.deps.TorBox.ControlDelete(rel.TorBoxID); err != nil {
		// Keep state=downloading and retry next tick — never leave the account copy
		// behind silently (the audit counts downloading as held, so no false leak).
		slog.Warn("qbit: wait finish: release account copy failed (will retry)",
			"hash", rel.Hash, "torbox_id", rel.TorBoxID, "err", err)
		return
	}

	cachedMap, err := s.deps.TorBox.CheckCached([]string{rel.Hash})
	item, ok := cachedMap[rel.Hash]
	if err != nil || !ok || len(item.Files) == 0 {
		// Downloaded but checkcached can't see it yet — flip to error only after the
		// budget window; before that, retry (cache index can lag).
		slog.Warn("qbit: wait finish: checkcached after download empty",
			"hash", rel.Hash, "err", err)
		s.failWaitDownload(rel, "not cached after download")
		return
	}

	// The file list is only known now: the same content gate as a cached add.
	if reason, file := gateRefusal(s.deps.Config.Policy, item.Files); reason != "" {
		metrics.IncAddRejected("content_after_wait")
		slog.Warn("qbit: wait download finished but fails the content gate", "reason", reason,
			"hash", rel.Hash, "name", rel.Name, "file", file)
		s.failWaitDownload(rel, "content gate: "+reason)
		return
	}

	rel.Name, rel.TorBoxName = s.releaseName(rel.Name, item.Name, rel.Hash), item.Name
	rel.TotalSize = item.Size
	rel.Cached = true
	rel.State = catalog.StateVirtual
	rel.TorBoxID = 0
	rel.MaterializedAt = 0
	files := toCatalogFiles(rel.Hash, item.Files)
	if err := s.deps.Store.UpsertRelease(rel, files); err != nil {
		slog.Error("qbit: wait finish: upsert", "hash", rel.Hash, "err", err)
		return
	}
	if err := s.deps.Symlink.Create(rel, files); err != nil {
		slog.Warn("qbit: wait finish: symlink", "hash", rel.Hash, "err", err)
	}
	s.waits.drop(rel.Hash)
	metrics.IncWait("completed", "")
	slog.Info("qbit: wait download complete → cached, released, import ready",
		"hash", rel.Hash, "name", rel.Name, "size", rel.TotalSize, "files", len(files))
}

// bailWaitDownload aborts a wait download over budget: delete from the account and
// surface the grab as an error.
//
// With slow_lane.adopt_wait_failures a download that is merely slow is handed to the slow
// lane first; when the lane keeps it running, the TorBox transfer is NOT deleted (the
// slow lane owns it now) — the grab still fails in the arr.
func (s *server) bailWaitDownload(rel *catalog.Release, reason string) {
	if s.adoptWaitDownload(rel, reason) {
		s.failWaitDownload(rel, reason)
		return
	}
	if err := s.deps.TorBox.ControlDelete(rel.TorBoxID); err != nil {
		slog.Warn("qbit: wait bail: delete failed (will retry next tick)",
			"hash", rel.Hash, "torbox_id", rel.TorBoxID, "err", err)
		return // stay in downloading; retried next tick
	}
	s.failWaitDownload(rel, reason)
}

// failWaitDownload marks the grab errored (no account copy remains). With
// policy.wait_fail_blocklist it also asks the owning arr, in the background, to
// treat the download as failed (see arrfail.go); otherwise the arr just shows a
// warning until something removes it.
func (s *server) failWaitDownload(rel *catalog.Release, reason string) {
	err := s.deps.Store.SetState(rel.Hash, catalog.StateError, 0)
	if err != nil {
		slog.Error("qbit: wait fail: set state", "hash", rel.Hash, "err", err)
	}
	s.waits.drop(rel.Hash)
	metrics.IncWait("failed", waitReasonLabel(reason))
	slog.Info("qbit: wait download failed", "hash", rel.Hash, "reason", reason)
	if err == nil {
		s.requestArrFail(rel, reason) // only once the error state is durable
	}
}

// waitReasonLabel maps a failure reason to a bounded metric label.
func waitReasonLabel(reason string) string {
	switch {
	case reason == "max wait elapsed":
		return "max_wait"
	case reason == "budget elapsed":
		return "budget"
	case reason == "eta over budget":
		return "eta"
	case reason == "no progress, no peers":
		return "no_peers"
	case reason == "vanished":
		return "vanished"
	case reason == "not cached after download":
		return "not_cached"
	case strings.HasPrefix(reason, "content gate"):
		return "content"
	}
	return "other"
}
