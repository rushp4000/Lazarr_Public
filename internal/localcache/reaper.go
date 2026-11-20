package localcache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/constants"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback"
)

// runReaper drives the retention sweep on the shared reaper cadence and the link
// index walk on its own interval.
func (e *Engine) runReaper(ctx context.Context) {
	t := time.NewTicker(constants.DefaultReaperEvery)
	defer t.Stop()
	walkEvery := e.cfg.LinkIndexInterval.D()
	if walkEvery <= 0 {
		walkEvery = 10 * time.Minute
	}
	e.walkLinks() // boot: learn where the arrs put things before the first sweep
	nextWalk := e.now().Add(walkEvery)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if e.now().After(nextWalk) {
				e.walkLinks()
				nextWalk = e.now().Add(walkEvery)
			}
			e.Sweep(ctx)
		}
	}
}

// walkLinks re-indexes the library symlinks and reconciles any complete file whose
// links still point at FUSE (e.g. a re-import after the pull finished).
func (e *Engine) walkLinks() {
	if e.links == nil {
		return
	}
	n, r, err := e.links.walk()
	if err != nil {
		e.log.Warn("localcache: link index walk", "err", err)
	}
	if r > 0 {
		e.log.Warn("localcache: repaired library links that pointed at missing cache files (back on FUSE)", "links", r)
	}
	e.mu.Lock()
	e.lastWalk = e.now()
	e.linksN = n
	var relink []fileKey
	rels := map[fileKey]string{}
	for k, fs := range e.files {
		if fs.State == catalog.LocalComplete {
			relink = append(relink, k)
			rels[k] = fs.rel
		}
	}
	e.mu.Unlock()
	for _, k := range relink {
		e.linkToCache(k, rels[k])
	}
}

// Sweep runs one retention pass (docs/28 §7.1). Exported for tests and the UI.
func (e *Engine) Sweep(ctx context.Context) { e.sweep(ctx, 0) }

// sweep is one retention pass. incoming is bytes about to be written by a pull that
// has not started yet (the puller's make-room pass); the policy sees the volume as if
// they had already landed. 0 for the regular reaper pass.
func (e *Engine) sweep(ctx context.Context, incoming uint64) {
	e.sweepMu.Lock()
	defer e.sweepMu.Unlock()
	if e.mountOK != nil && !e.mountOK() {
		e.log.Warn("localcache: skipping sweep — FUSE mount unhealthy")
		return
	}
	now := e.now()
	disk, err := e.statDisk()
	if err != nil {
		e.log.Warn("localcache: statfs", "err", err)
		disk = DiskStat{}
	}
	if incoming == 0 {
		metrics.SetLocalDiskFree(disk.Free)
	}
	disk.Pending = incoming

	rows, err := e.store.ListLocalFiles("")
	if err != nil {
		e.log.Warn("localcache: list local files", "err", err)
		return
	}
	activeSeries := e.activeSeries(now)
	holds := e.analysisHolds(now)
	unref := map[fileKey]time.Time{} // this sweep's unrefSince (see unreferencedSince)
	defer func() {
		e.mu.Lock()
		e.unrefSince = unref
		e.mu.Unlock()
	}()
	byState := map[string]int{}
	var bytes int64
	cands := make([]Candidate, 0, len(rows))
	relCache := map[string]*catalog.Release{}
	for _, lf := range rows {
		byState[string(lf.State)]++
		if lf.State == catalog.LocalComplete {
			bytes += lf.Size
		} else {
			bytes += lf.Frontier
		}
		if lf.State == catalog.LocalPulling {
			continue // in flight (or resumable); never evicted mid-pull
		}
		rel, ok := relCache[lf.Hash]
		if !ok {
			rel, _, err = e.store.GetRelease(lf.Hash)
			if errors.Is(err, catalog.ErrNotFound) {
				// Release forgotten and the cascade missed this row: links first, then the row.
				e.dropLocal(fileKey{lf.Hash, lf.FileID}, "release forgotten")
				continue
			}
			if err != nil || rel == nil {
				continue // transient catalog error: decide next sweep, never delete on a guess
			}
			relCache[lf.Hash] = rel
		}
		watch, _ := e.store.ListWatchByFile(lf.Hash, lf.FileID)
		links, unrefSince := 1, time.Time{} // no index (publish failed): never unreferenced
		if e.links != nil {
			links = e.links.libraryLinks(lf.Hash, lf.FileID)
			if links == 0 {
				unrefSince = e.unreferencedSince(fileKey{lf.Hash, lf.FileID}, unref)
			}
		}
		c := Candidate{
			File:           *lf,
			Release:        *rel,
			Wanted:         e.isWanted(fileKey{lf.Hash, lf.FileID}, now),
			Links:          links,
			LinksCheckedAt: unrefSince,
			AnalysisHold:   holds[fileKey{lf.Hash, lf.FileID}],
		}
		for _, w := range watch {
			c.Watch = append(c.Watch, *w)
			if w.SeriesKey != "" && activeSeries[seriesID{w.Server, w.SeriesKey}] {
				c.SeriesActive = true
			}
		}
		c.ReleaseDate, c.IsMovie, c.Newness = e.newness(rel, now)
		cands = append(cands, c)
	}
	if incoming == 0 {
		metrics.SetLocalBytes(bytes)
		for _, st := range []catalog.LocalState{catalog.LocalComplete, catalog.LocalPulling, catalog.LocalFailed} {
			metrics.SetLocalFiles(string(st), byState[string(st)])
		}
	}

	evict, classes := Decide(cands, disk, now, e.cfg)
	if incoming == 0 {
		e.warnIfStuck(cands, evict, classes, disk, now)
	}
	if len(evict) > 0 && e.links != nil {
		// Never act on a stale index: an arr rename since the last walk would leave the
		// renamed link pointing into the cache after its bytes are deleted.
		e.walkLinks()
	}
	for _, c := range cands {
		class := classes[[2]any{c.File.Hash, c.File.FileID}]
		if c.File.Class != class.String() {
			c.File.Class = class.String()
			row := c.File
			_ = e.store.UpsertLocalFile(&row)
			e.mu.Lock()
			if fs := e.files[fileKey{c.File.Hash, c.File.FileID}]; fs != nil {
				fs.Class = class.String()
			}
			e.mu.Unlock()
		}
	}
	for _, d := range evict {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := e.evict(fileKey{d.Hash, d.FileID}, d.Class.String(), d.Reason); err != nil {
			e.log.Warn("localcache: evict", "hash", short(d.Hash), "file_id", d.FileID, "err", err)
		}
	}
	e.sweepOrphans()
}

// stuckWarnEvery rate-limits the "nothing left to evict" warning.
const stuckWarnEvery = 15 * time.Minute

// warnIfStuck logs at WARN when the volume stays at or below free_low_pct even after
// every evictable file is gone, i.e. only wanted and pinned files are left (docs/28
// §7.1 step 3). Nothing else happens: playback of anything not local keeps streaming
// through FUSE, and new pulls are held back by reserve_pct.
func (e *Engine) warnIfStuck(cands []Candidate, evict []Decision, classes map[[2]any]Class, disk DiskStat, now time.Time) {
	going := make(map[[2]any]bool, len(evict))
	var freed uint64
	for _, d := range evict {
		going[[2]any{d.Hash, d.FileID}] = true
	}
	kept := map[string]int{}
	for _, c := range cands {
		k := [2]any{c.File.Hash, c.File.FileID}
		if going[k] {
			freed += uint64(c.File.Size) //nolint:gosec // sizes are non-negative
			continue
		}
		kept[classes[k].String()]++
	}
	if !lowWater(disk, freed, e.cfg) {
		return
	}
	e.mu.Lock()
	if !e.stuckWarnAt.IsZero() && now.Sub(e.stuckWarnAt) < stuckWarnEvery {
		e.mu.Unlock()
		return
	}
	e.stuckWarnAt = now
	e.mu.Unlock()
	free := DiskStat{Free: disk.Free + freed, Total: disk.Total, Pending: disk.Pending}.FreePct()
	e.log.Warn("localcache: below the low-water mark with only wanted/pinned files left — nothing more to evict; non-local files keep streaming",
		"free_pct", fmt.Sprintf("%.2f", free), "free_low_pct", e.cfg.FreeLowPct, "evicting", len(evict),
		"wanted", kept[ClassWanted.String()], "watching", kept[ClassWatching.String()], "other", len(cands)-len(evict)-kept[ClassWanted.String()]-kept[ClassWatching.String()])
}

type seriesID struct{ server, key string }

// activeSeries is the set of series some viewer is mid-way through within active_window.
func (e *Engine) activeSeries(now time.Time) map[seriesID]bool {
	out := map[seriesID]bool{}
	since := now.Add(-e.cfg.ActiveWindow.D()).Unix()
	rows, err := e.store.ListWatchSince(since)
	if err != nil {
		return out
	}
	for _, w := range rows {
		if w.SeriesKey != "" && !w.Completed {
			out[seriesID{w.Server, w.SeriesKey}] = true
		}
	}
	return out
}

// unreferencedSince returns when key was first seen with no library link, carrying
// the previous sweep's clock into next (this sweep's map). The clock starts at the
// last link walk — the walk that found the links gone — so the grace measures time
// since the arr removed them, not since the most recent walk. Zero (never
// unreferenced) until a walk has run.
func (e *Engine) unreferencedSince(key fileKey, next map[fileKey]time.Time) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	since, ok := e.unrefSince[key]
	if !ok {
		if e.lastWalk.IsZero() {
			return time.Time{}
		}
		since = e.lastWalk
	}
	next[key] = since
	return since
}

// evict retargets links back to FUSE, deletes the bytes, and drops the row.
func (e *Engine) evict(key fileKey, class, reason string) error {
	e.mu.Lock()
	fs, ok := e.files[key]
	if !ok {
		e.mu.Unlock()
		_ = e.store.DeleteLocalFile(key.hash, key.fileID)
		return nil
	}
	if p, running := e.pulling[key]; running {
		p.cancel() // eviction while pulling (forget/manual): stop the worker first
	}
	rel := fs.rel
	e.mu.Unlock()

	var errs error
	if e.links != nil {
		if _, err := e.links.retarget(key.hash, key.fileID, rel, playback.TargetFuse); err != nil {
			// Deleting the bytes now would leave a library link dangling; keep the file
			// and retry on the next sweep.
			return fmt.Errorf("localcache: evict kept bytes, retarget to fuse failed: %w", err)
		}
	}
	if err := e.disk.remove(key.hash, rel); err != nil {
		errs = errors.Join(errs, err)
	}
	if err := e.store.DeleteLocalFile(key.hash, key.fileID); err != nil {
		errs = errors.Join(errs, err)
	}
	e.mu.Lock()
	delete(e.files, key)
	delete(e.wanted, key)
	e.mu.Unlock()
	e.closeAnalysis(key, "evicted ("+class+")")
	if errs == nil {
		metrics.IncLocalEviction(class)
		e.log.Info("localcache: evicted", "hash", short(key.hash), "file_id", key.fileID, "class", class, "reason", reason)
	}
	return errs
}

// sweepOrphans removes <hash> directories on disk that no row references (a
// forgotten release whose cascade ran while we were down, or a crash mid-delete).
func (e *Engine) sweepOrphans() {
	dirs, err := e.disk.hashDirs()
	if err != nil {
		return
	}
	rows, err := e.store.ListLocalFiles("")
	if err != nil {
		return // cannot tell orphans from live files: delete nothing
	}
	e.mu.Lock()
	live := map[string]bool{}
	for k := range e.files {
		live[k.hash] = true
	}
	for k := range e.pulling {
		live[k.hash] = true
	}
	e.mu.Unlock()
	for _, lf := range rows {
		live[lf.Hash] = true
	}
	removed := 0
	for _, h := range dirs {
		if live[h] {
			continue
		}
		if err := e.disk.removeHash(h); err != nil {
			e.log.Warn("localcache: remove orphan dir", "hash", short(h), "err", err)
		} else {
			removed++
			e.log.Info("localcache: removed orphan cache dir", "hash", short(h))
		}
	}
	if removed > 0 {
		e.walkLinks() // any link still pointing into a removed dir goes back to FUSE now
	}
}

// dropLocal forgets one local file whose bytes are unusable: library links back to
// FUSE first, then the row, then the bytes. The order matters — a link must never
// outlive the file it points at.
func (e *Engine) dropLocal(key fileKey, why string) {
	e.mu.Lock()
	rel := ""
	if fs, ok := e.files[key]; ok {
		rel = fs.rel
	}
	e.mu.Unlock()
	if e.links != nil && rel != "" {
		if _, err := e.links.retarget(key.hash, key.fileID, rel, playback.TargetFuse); err != nil {
			e.log.Warn("localcache: drop: retarget to fuse", "hash", short(key.hash), "file_id", key.fileID, "err", err)
			return
		}
	}
	if rel != "" {
		_ = e.disk.remove(key.hash, rel)
	}
	_ = e.store.DeleteLocalFile(key.hash, key.fileID)
	e.mu.Lock()
	delete(e.files, key)
	delete(e.wanted, key)
	e.mu.Unlock()
	e.closeAnalysis(key, "dropped: "+why)
	// Links the index did not know about (its rows cascaded with the release) now
	// point at a missing file; the walk puts them back on FUSE.
	e.walkLinks()
	e.log.Warn("localcache: dropped local file", "hash", short(key.hash), "file_id", key.fileID, "why", why)
}

// Forget drops every local file of a release (qbit torrents/delete, Web UI forget):
// links back to FUSE first, then bytes, then rows.
func (e *Engine) Forget(hash string) error {
	e.mu.Lock()
	var keys []fileKey
	for k := range e.files {
		if k.hash == hash {
			keys = append(keys, k)
		}
	}
	for k, p := range e.pulling {
		if k.hash == hash {
			p.cancel()
		}
	}
	kept := e.queue[:0]
	for _, r := range e.queue {
		if r.key.hash == hash {
			delete(e.queued, r.key)
			continue
		}
		kept = append(kept, r)
	}
	e.queue = kept
	e.mu.Unlock()
	var errs error
	for _, k := range keys {
		if err := e.evict(k, "forget", "forget"); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	if err := e.disk.removeHash(hash); err != nil {
		errs = errors.Join(errs, err)
	}
	return errs
}

// EvictRelease evicts every local file of one release (Web UI "Evict").
func (e *Engine) EvictRelease(hash string) error {
	e.mu.Lock()
	var keys []fileKey
	for k := range e.files {
		if k.hash == hash {
			keys = append(keys, k)
		}
	}
	e.mu.Unlock()
	var errs error
	for _, k := range keys {
		if err := e.evict(k, "manual", "manual"); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// PullRelease enqueues every eligible file of a release (Web UI "Pull now").
func (e *Engine) PullRelease(hash string) (int, error) {
	rel, files, err := e.store.GetRelease(hash)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		if e.enqueueFile(rel, f, ReasonManual) {
			n++
		}
	}
	return n, nil
}
