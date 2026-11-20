package qbit

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/metrics"
)

// deleteLinkCheckTimeout bounds the synchronous library walk a torrents/delete may run
// (measured live: ~60 ms for ~8.7k symlinks). Past it the answer is "unknown", which
// keeps the row.
const deleteLinkCheckTimeout = 20 * time.Second

// deleteOne handles torrents/delete for one hash.
//
// Why a delete is not always a delete (v1.2.8): an arr removes a download from the
// client after a PARTIAL import too (a season pack whose other episodes were not
// imported, a queue cleaner, a human). Pre-1.2.8 that dropped the catalog row, so the
// FUSE tree stopped serving the hash and every library symlink the arr had already
// imported dangled (57 links on 2026-10-09). With policy.detach_on_delete the row is
// kept but hidden from the qBit API while library links still reference it.
//
// A downloading row (on_cache_miss=wait) is a live TorBox transfer: it is removed from
// the account first, so the arr dropping the grab never leaks it.
func (s *server) deleteOne(ctx context.Context, h string) {
	unlock := s.locks.lock(h)
	defer unlock()

	rel, _, err := s.deps.Store.GetRelease(h)
	switch {
	case err == nil && rel != nil:
		if rel.State == catalog.StateDownloading {
			s.cancelWaitDownload(rel)
			return
		}
		if s.deps.Config.Policy.DetachOnDelete && s.deps.Links != nil {
			cctx, cancel := context.WithTimeout(ctx, deleteLinkCheckTimeout)
			links, lerr := s.deps.Links.LinksTo(cctx, h)
			cancel()
			if lerr != nil || len(links) > 0 {
				s.detach(rel, links, lerr)
				return
			}
		}
	case err != nil && !errors.Is(err, catalog.ErrNotFound) && s.deps.Config.Policy.DetachOnDelete:
		// The store could not say what this is; deleting blind could break a library.
		slog.Warn("qbit: delete: release lookup failed, leaving it in place", "hash", h, "err", err)
		metrics.IncQbitDelete("lookup_error")
		return
	}
	s.purge(h)
	metrics.IncQbitDelete("purged")
}

// purge is the full delete: local cache forget, download-dir symlinks, engine release,
// catalog row (cascading files, links, local_file rows).
func (s *server) purge(h string) {
	if s.deps.ForgetHook != nil {
		s.deps.ForgetHook(h) // local cache: retarget links to FUSE + drop local bytes first
	}
	if err := s.deps.Symlink.Remove(h); err != nil {
		slog.Warn("qbit: Symlink.Remove", "hash", h, "err", err)
	}
	// S2: release any in-flight (or store-only) materialization BEFORE deleting the row.
	// Order matters — Release consults the store for store-only leftovers (B2), so
	// deleting the row first would orphan the TorBox item. A release error must not fail
	// the delete (the arr expects 200); log and continue.
	if s.deps.Engine != nil {
		if err := s.deps.Engine.Release(h); err != nil {
			slog.Warn("qbit: engine release on delete", "hash", h, "err", err)
		}
	}
	if err := s.deps.Store.DeleteRelease(h); err != nil && !errors.Is(err, catalog.ErrNotFound) {
		slog.Warn("qbit: DeleteRelease", "hash", h, "err", err)
	}
	s.waits.drop(h)
}

// detach keeps the release row (so FUSE keeps serving the imported files) but hides it
// from the arr. Only Lazarr's own download_dir symlinks go; the local cache is left
// alone because library links may point at its bytes. lerr != nil means the link check
// itself failed: the row is kept as the safe answer.
func (s *server) detach(rel *catalog.Release, links []libscan.Link, lerr error) {
	h := rel.Hash
	chained := false
	for _, l := range links {
		chained = chained || l.Chained
	}
	if !chained {
		if err := s.deps.Symlink.Remove(h); err != nil {
			slog.Warn("qbit: Symlink.Remove", "hash", h, "err", err)
		}
	}
	// Frees an idle TorBox copy and its slot; a release being read right now is left
	// alone (refs > 0) and the next read re-materializes as usual.
	if s.deps.Engine != nil {
		if err := s.deps.Engine.Release(h); err != nil {
			slog.Warn("qbit: engine release on detach", "hash", h, "err", err)
		}
	}
	if rel.DetachedAt == 0 {
		if err := s.deps.Store.SetDetached(h, s.now().Unix()); err != nil {
			slog.Warn("qbit: set detached", "hash", h, "err", err)
		}
	}
	s.waits.drop(h)
	metrics.IncQbitDelete("detached")
	if lerr != nil {
		slog.Warn("qbit: delete detached (library link check failed, keeping the release)",
			"hash", h, "name", rel.Name, "category", rel.Category, "err", lerr)
		return
	}
	slog.Info("qbit: delete detached (library links: "+strconv.Itoa(len(links))+")",
		"hash", h, "name", rel.Name, "category", rel.Category,
		"links", len(links), "chained", chained)
}

// cancelWaitDownload removes a downloading (on_cache_miss=wait) release: the TorBox
// transfer first, then the row. If TorBox refuses, the row is detached (hidden from the
// arr, still counted as held by the ToS audit) and the wait poller retries the delete on
// every tick until it succeeds.
func (s *server) cancelWaitDownload(rel *catalog.Release) {
	h := rel.Hash
	if rel.TorBoxID != 0 {
		if err := s.deps.TorBox.ControlDelete(rel.TorBoxID); err != nil {
			if rel.DetachedAt == 0 {
				if serr := s.deps.Store.SetDetached(h, s.now().Unix()); serr != nil {
					slog.Warn("qbit: set detached", "hash", h, "err", serr)
				}
			}
			metrics.IncQbitDelete("wait_cancel_retry")
			slog.Warn("qbit: delete of a wait download: TorBox delete failed, retrying on the next wait-poll tick",
				"hash", h, "torbox_id", rel.TorBoxID, "err", err)
			return
		}
	}
	s.waits.drop(h)
	if err := s.deps.Symlink.Remove(h); err != nil {
		slog.Warn("qbit: Symlink.Remove", "hash", h, "err", err)
	}
	if err := s.deps.Store.DeleteRelease(h); err != nil && !errors.Is(err, catalog.ErrNotFound) {
		slog.Warn("qbit: DeleteRelease", "hash", h, "err", err)
	}
	metrics.IncQbitDelete("wait_cancelled")
	slog.Info("qbit: delete cancelled wait download (TorBox transfer removed)",
		"hash", h, "name", rel.Name, "torbox_id", rel.TorBoxID)
}

// SweepDetached deletes detached releases whose library links are all gone. A link
// check that errors keeps the row (unknown is not "gone"). Downloading rows are the
// wait poller's job.
func (s *server) SweepDetached(ctx context.Context) int {
	if s.deps.Links == nil {
		return 0
	}
	rows, err := s.deps.Store.DetachedReleases()
	if err != nil {
		slog.Warn("qbit: detached sweep: list", "err", err)
		return 0
	}
	removed := 0
	for _, rel := range rows {
		if ctx.Err() != nil {
			break
		}
		if rel.State == catalog.StateDownloading {
			continue
		}
		links, lerr := s.deps.Links.LinksTo(ctx, rel.Hash)
		if lerr != nil || len(links) > 0 {
			continue
		}
		if s.sweepOne(rel.Hash) {
			removed++
		}
	}
	if removed > 0 {
		slog.Info("qbit: detached sweep removed releases with no library links left", "removed", removed)
	}
	return removed
}

// sweepOne purges one detached release under its hash lock, re-checking that it is
// still detached (a re-grab in between clears detached_at and must win).
func (s *server) sweepOne(h string) bool {
	unlock := s.locks.lock(h)
	defer unlock()
	cur, _, err := s.deps.Store.GetRelease(h)
	if err != nil || cur == nil || cur.DetachedAt == 0 || cur.State == catalog.StateDownloading {
		return false
	}
	s.purge(h)
	metrics.IncQbitDelete("swept")
	slog.Info("qbit: detached release has no library links left, removed",
		"hash", h, "name", cur.Name, "category", cur.Category)
	return true
}

// hashLocks serializes the lifecycle of one infohash across the add handler, the
// delete handler, the wait poller and the detached sweep.
type hashLocks struct {
	mu sync.Mutex
	m  map[string]*hashLock
}

type hashLock struct {
	mu   sync.Mutex
	refs int
}

func newHashLocks() *hashLocks { return &hashLocks{m: make(map[string]*hashLock)} }

// lock acquires the lock for h and returns its release func.
func (l *hashLocks) lock(h string) func() {
	l.mu.Lock()
	hl := l.m[h]
	if hl == nil {
		hl = &hashLock{}
		l.m[h] = hl
	}
	hl.refs++
	l.mu.Unlock()

	hl.mu.Lock()
	return func() {
		hl.mu.Unlock()
		l.mu.Lock()
		hl.refs--
		if hl.refs == 0 {
			delete(l.m, h)
		}
		l.mu.Unlock()
	}
}
