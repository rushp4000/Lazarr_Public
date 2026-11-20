package qbit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback/arr"
)

// policy.wait_fail_blocklist: turn a failed on_cache_miss=wait grab into a FAILED
// download in the arr.
//
// Why Lazarr has to do this itself: Sonarr/Radarr map qBittorrent's "error" state
// to a queue *warning* on purpose ("warning so failed download handling isn't
// triggered", QBittorrent.cs), and no qBit state maps to failed. A bailed wait
// download therefore sat in the arr's queue forever as "qBittorrent is reporting an
// error" and the arr never searched again. Removing the queue item with
// blocklist=true runs the arr's own failed-download handling: blocklist the release,
// search for a replacement, and (removeFromClient=true) call our torrents/delete.
//
// The arr call runs in the background, never on the wait-poller tick: the arr may
// be slow or down, and right after the state flips its queue may not list the item
// yet. Every outcome other than success leaves the plain error state as fallback.
//
// The same mechanism covers two other paths that used to end in a warning or a loop:
//   - content gate at add time (acceptToFail): "Fails." on torrents/add is NOT a failed
//     download in the arr either — it logs "Couldn't add report to download queue" and
//     blocklists nothing, so RSS sync grabbed the same fake every ~15 min forever
//     (Charter House S15E03 -CAKES .exe, 2026-10-14). The add is accepted as an empty
//     error row instead (no files, no symlinks: nothing can reach the library) and
//     failed in the arr like a wait download;
//   - dead-cache at materialize time (FailInArr, called by the engine): a release the
//     engine marks errored because TorBox purged it.
//
// Transient refusals (checkcached error/backoff, wait-cap overflow, failed uncached
// add) deliberately keep "Fails.": the release itself is fine, and the arr retrying it
// on the next RSS sync is what should happen once TorBox or the cap recovers.

// arrFailRetryDelays is the wait before each attempt: try at once (the item has been
// in the arr's queue since the grab), then back off across a few arr queue refreshes
// (~1 min each). Bounded: ~7.5 min and 5 attempts per failed download.
var arrFailRetryDelays = []time.Duration{0, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}

// arrFailAttemptTimeout bounds one attempt (queue scan + delete).
const arrFailAttemptTimeout = 30 * time.Second

// gateFailTTL is how long a hash that the content gate accepted-to-fail is answered a
// plain "Fails." when the arr grabs it again. A blocklisted release is never grabbed
// again, so a repeat means the blocklist did not stick (the arr call gave up, or
// something removed the queue item without blocklisting): one arr round trip per hash
// per window, never one per RSS sync.
const gateFailTTL = 12 * time.Hour

// arrFails tracks the background arr calls: one per hash at a time.
type arrFails struct {
	ctx      context.Context // canceled on shutdown (set by StartWaitPoller)
	delays   []time.Duration
	inflight sync.Map // hash -> struct{}
	wg       sync.WaitGroup

	mu    sync.Mutex
	gated map[string]time.Time // hash -> when the content gate accepted it to fail
}

func newArrFails() *arrFails {
	return &arrFails{ctx: context.Background(), delays: arrFailRetryDelays,
		gated: make(map[string]time.Time)}
}

// gatedRecently reports whether hash was accepted-to-fail within gateFailTTL.
func (f *arrFails) gatedRecently(hash string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.gated[hash]
	return ok && now.Sub(at) < gateFailTTL
}

// markGated records an accepted-to-fail hash, dropping expired entries (bounded map).
func (f *arrFails) markGated(hash string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for h, at := range f.gated {
		if now.Sub(at) >= gateFailTTL {
			delete(f.gated, h)
		}
	}
	f.gated[hash] = now
}

// acceptToFail is the content gate's alternative to "Fails." with
// policy.wait_fail_blocklist: store rel as an empty error row (no files, so no
// symlinks and nothing the arr could import), answer "Ok.", and have the arr fail the
// download (blocklist + search for another). Returns false — the caller answers
// "Fails." as before — when the feature is off, the category has no arr, the hash went
// through this within gateFailTTL, or a row already exists for the hash (a live or
// detached release is never overwritten; an unknown store answer counts as existing).
// Called under the hash lock.
func (s *server) acceptToFail(w http.ResponseWriter, rel *catalog.Release, reason string) bool {
	if !s.deps.Config.Policy.WaitFailBlocklist || s.deps.Arrs[rel.Category] == nil {
		return false
	}
	now := s.now()
	if s.fails.gatedRecently(rel.Hash, now) {
		slog.Info("qbit: content gate: release already handed to the arr to blocklist, refusing again",
			"hash", rel.Hash, "category", rel.Category, "ttl", gateFailTTL)
		return false
	}
	if _, _, err := s.deps.Store.GetRelease(rel.Hash); !errors.Is(err, catalog.ErrNotFound) {
		return false
	}
	rel.State = catalog.StateError
	rel.TorBoxID = 0
	if err := s.deps.Store.UpsertRelease(rel, nil); err != nil {
		slog.Warn("qbit: content gate: store error row", "hash", rel.Hash, "err", err)
		return false
	}
	s.fails.markGated(rel.Hash, now)
	slog.Info("qbit: content gate: accepted as a failed download so the arr blocklists it",
		"hash", rel.Hash, "category", rel.Category, "name", rel.Name, "reason", reason)
	writeText(w, "Ok.")
	s.startArrFail(rel, reason, true)
	return true
}

// FailInArr asks the owning arr to fail a release another component (the materialize
// engine, on dead-cache) just marked errored. No-op unless policy.wait_fail_blocklist
// is on and the row is still a visible error row. Cheap and non-blocking: one store
// read, then the usual background attempts.
func (s *server) FailInArr(hash, reason string) {
	if !s.deps.Config.Policy.WaitFailBlocklist {
		return
	}
	rel, _, err := s.deps.Store.GetRelease(hash)
	if err != nil || rel == nil || rel.State != catalog.StateError || rel.DetachedAt != 0 {
		return
	}
	s.requestArrFail(rel, reason)
}

// requestArrFail starts the background arr call for a release that just went to
// StateError from a wait download. No-op unless policy.wait_fail_blocklist is on.
func (s *server) requestArrFail(rel *catalog.Release, reason string) {
	if !s.deps.Config.Policy.WaitFailBlocklist {
		return
	}
	s.startArrFail(rel, reason, false)
}

// startArrFail runs the background arr call. gated marks a content-gate placeholder
// row, which is dropped when the arr cannot be made to fail it (dropGatePlaceholder).
func (s *server) startArrFail(rel *catalog.Release, reason string, gated bool) {
	hash, cat, name := rel.Hash, rel.Category, rel.Name // copy: rel is the caller's
	a := s.deps.Arrs[cat]
	if a == nil {
		slog.Info("qbit: wait_fail_blocklist: no arr (url + api key) for category, leaving error state",
			"hash", hash, "category", cat)
		metrics.IncWaitFailBlocklist("no_arr")
		return
	}
	if _, busy := s.fails.inflight.LoadOrStore(hash, struct{}{}); busy {
		return
	}
	s.fails.wg.Add(1)
	go func() {
		defer s.fails.wg.Done()
		defer s.fails.inflight.Delete(hash)
		result := s.runArrFail(a, hash, cat, name, reason)
		if gated && (result == "not_in_queue" || result == "error") && s.fails.ctx.Err() == nil {
			s.dropGatePlaceholder(hash)
		}
		metrics.IncWaitFailBlocklist(result)
	}()
}

// runArrFail makes the bounded attempts and returns the metric result.
func (s *server) runArrFail(a ArrFailer, hash, cat, name, reason string) string {
	ctx := s.fails.ctx
	var lastErr error
	for i, d := range s.fails.delays {
		if d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return "error"
			case <-t.C:
			}
		}
		// Someone else already resolved it (the arr/Cleanuparr/a human deleted it via
		// torrents/delete, or it was re-grabbed): nothing left to fail.
		// A store read error other than not-found is not proof of either; try anyway.
		rel, _, err := s.deps.Store.GetRelease(hash)
		if errors.Is(err, catalog.ErrNotFound) || (err == nil && (rel == nil || rel.State != catalog.StateError || rel.DetachedAt != 0)) {
			slog.Info("qbit: wait_fail_blocklist: release no longer errored, nothing to do",
				"hash", hash, "category", cat)
			return "gone"
		}
		actx, cancel := context.WithTimeout(ctx, arrFailAttemptTimeout)
		err = a.FailDownload(actx, hash)
		cancel()
		if err == nil {
			slog.Info("qbit: wait_fail_blocklist: arr marked download failed (blocklisted, searching for another)",
				"hash", hash, "category", cat, "name", name, "reason", reason, "attempt", i+1)
			return "removed"
		}
		lastErr = err
		if errors.Is(err, arr.ErrNotInQueue) {
			slog.Debug("qbit: wait_fail_blocklist: not in arr queue yet, will retry",
				"hash", hash, "category", cat, "attempt", i+1)
		} else {
			slog.Warn("qbit: wait_fail_blocklist: arr call failed, will retry",
				"hash", hash, "category", cat, "attempt", i+1, "err", err)
		}
	}
	if errors.Is(lastErr, arr.ErrNotInQueue) {
		slog.Info("qbit: wait_fail_blocklist: download never found in arr queue, leaving error state",
			"hash", hash, "category", cat, "attempts", len(s.fails.delays))
		return "not_in_queue"
	}
	slog.Warn("qbit: wait_fail_blocklist: giving up, leaving error state",
		"hash", hash, "category", cat, "attempts", len(s.fails.delays), "err", lastErr)
	return "error"
}

// dropGatePlaceholder removes a content-gate error row the arr could not be made to
// fail. Left in place it would sit in the arr's queue as a warning forever, holding
// the episode; gone, the arr forgets the grab and, should it pick the release again
// within gateFailTTL, gets a plain "Fails.". The row holds no files, symlinks or TorBox
// item; it is re-read under the hash lock so a row that changed meanwhile is kept.
func (s *server) dropGatePlaceholder(hash string) {
	unlock := s.locks.lock(hash)
	defer unlock()
	rel, files, err := s.deps.Store.GetRelease(hash)
	if err != nil || rel == nil || rel.State != catalog.StateError || rel.DetachedAt != 0 || len(files) > 0 {
		return
	}
	s.purge(hash)
	slog.Info("qbit: content gate: arr did not fail the download, removed the empty error row",
		"hash", hash, "category", rel.Category)
}
