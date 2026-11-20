package localcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback"
)

const (
	persistEvery     = 64 << 20        // persist the frontier to the catalog this often
	firstReadTimeout = 2 * time.Minute // slot admission may block; give up and retry later
	throttlePause    = 5 * time.Second
	requeueBackoff   = 30 * time.Second
	pullBudgetWindow = time.Hour
)

// Reasons a pull is requested.
const (
	ReasonWatch    = "watch"
	ReasonPrefetch = "prefetch"
	ReasonGrab     = "grab"
	ReasonManual   = "manual"
	// ReasonSeason is a speculative season-fill pull (docs/28 §17): queued behind every
	// live-session pull and counted against local_cache.speculative_gb_per_day.
	ReasonSeason = "season"
)

// speculative reports whether a pull reason is background work (capped, low priority).
func speculative(reason string) bool { return reason == ReasonSeason }

// specWindow is the rolling window of local_cache.speculative_gb_per_day.
const specWindow = 24 * time.Hour

// Enqueue asks for one file to be pulled. It applies every static guard (docs/28
// §4.3) and returns false, with a skipped metric, when the file is not eligible.
// Duplicates (already local, pulling or queued) return false without a metric.
func (e *Engine) Enqueue(hash string, fileID int, reason string) bool {
	rel, files, err := e.store.GetRelease(hash)
	if err != nil || rel == nil {
		return false
	}
	var f *catalog.File
	for i := range files {
		if files[i].FileID == fileID {
			f = &files[i]
		}
	}
	if f == nil {
		return false
	}
	return e.enqueueFile(rel, *f, reason)
}

func (e *Engine) enqueueFile(rel *catalog.Release, f catalog.File, reason string) bool {
	key := fileKey{rel.Hash, f.FileID}
	switch {
	case e.cfg.TorrentOnly && rel.Source != catalog.SourceTorrent:
		metrics.IncLocalSkipped("source")
		return false
	case !rel.Cached:
		metrics.IncLocalSkipped("uncached")
		return false
	case rel.CacheStatus == catalog.CacheStatusEvicted:
		metrics.IncLocalSkipped("evicted")
		return false
	case rel.State == catalog.StateError:
		metrics.IncLocalSkipped("error")
		return false
	case f.Size < int64(e.cfg.MinFileMiB)<<20:
		metrics.IncLocalSkipped("size")
		return false
	}
	if _, err := safeRel(f.RelPath); err != nil {
		metrics.IncLocalSkipped("path")
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	if fs, ok := e.files[key]; ok && fs.State == catalog.LocalComplete {
		return false
	}
	if e.queued[key] {
		if !speculative(reason) {
			e.promoteLocked(key, reason) // a viewer now wants a file season fill queued
		}
		return false
	}
	if _, running := e.pulling[key]; running {
		return false
	}
	e.insertLocked(pullReq{key: key, rel: f.RelPath, size: f.Size, reason: reason, name: rel.Name})
	e.queued[key] = true
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return true
}

// insertLocked queues req: live-session reasons go ahead of every speculative entry
// (FIFO among themselves), speculative ones at the tail. Caller holds e.mu.
func (e *Engine) insertLocked(req pullReq) {
	if speculative(req.reason) {
		e.queue = append(e.queue, req)
		return
	}
	i := 0
	for i < len(e.queue) && !speculative(e.queue[i].reason) {
		i++
	}
	e.queue = append(e.queue, pullReq{})
	copy(e.queue[i+1:], e.queue[i:])
	e.queue[i] = req
}

// promoteLocked turns a queued speculative request into a live-session one and moves
// it ahead of the speculative tail. Caller holds e.mu.
func (e *Engine) promoteLocked(key fileKey, reason string) {
	for i, r := range e.queue {
		if r.key != key || !speculative(r.reason) {
			continue
		}
		e.queue = append(e.queue[:i], e.queue[i+1:]...)
		r.reason = reason
		e.insertLocked(r)
		return
	}
}

// specBudgetOK reports whether a speculative pull of size bytes fits in the rolling
// 24 h budget, and if so records it.
func (e *Engine) specBudgetOK(now time.Time, size int64) bool {
	limit := int64(e.cfg.SpeculativeGBPerDay) << 30
	e.mu.Lock()
	defer e.mu.Unlock()
	cut := now.Add(-specWindow)
	kept := e.specLog[:0]
	var used int64
	for _, s := range e.specLog {
		if s.at.After(cut) {
			kept = append(kept, s)
			used += s.bytes
		}
	}
	e.specLog = kept
	if limit <= 0 || used+size > limit {
		return false
	}
	e.specLog = append(e.specLog, specPull{at: now, bytes: size})
	return true
}

// runPuller is one worker: pop → pull → repeat until ctx ends.
func (e *Engine) runPuller(ctx context.Context) {
	for {
		req, ok := e.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-time.After(requeueBackoff):
			}
			continue
		}
		if err := e.pull(ctx, req); err != nil && !errors.Is(err, context.Canceled) {
			e.log.Warn("localcache: pull failed", "hash", short(req.key.hash), "file_id", req.key.fileID, "reason", req.reason, "err", err)
		}
	}
}

func (e *Engine) pop() (pullReq, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 {
		return pullReq{}, false
	}
	req := e.queue[0]
	e.queue = e.queue[1:]
	delete(e.queued, req.key)
	return req, true
}

// requeue puts a request back at the tail (after a soft failure such as no slot).
func (e *Engine) requeue(req pullReq) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.queued[req.key] {
		return
	}
	e.insertLocked(req)
	e.queued[req.key] = true
}

// budgetOK reports whether another pull may start inside the hourly budget.
func (e *Engine) budgetOK(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	cut := now.Add(-pullBudgetWindow)
	kept := e.pullLog[:0]
	for _, t := range e.pullLog {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.pullLog = kept
	return len(e.pullLog) < e.cfg.MaxPullsPerHour
}

// pull streams one file through the inner engine into the cache (docs/28 §4.3).
func (e *Engine) pull(ctx context.Context, req pullReq) error {
	now := e.now()
	// Dynamic guards: budget and disk reserve.
	if !e.budgetOK(now) {
		metrics.IncLocalSkipped("budget")
		e.log.Info("localcache: hourly pull budget reached, deferring", "hash", short(req.key.hash))
		e.deferRequeue(ctx, req, 10*time.Minute)
		return nil
	}
	if speculative(req.reason) && !e.specBudgetOK(now, req.size) {
		metrics.IncLocalSkipped("speculative_budget")
		e.log.Info("localcache: speculative daily budget reached, deferring", "hash", short(req.key.hash), "reason", req.reason)
		e.deferRequeue(ctx, req, time.Hour)
		return nil
	}
	if !e.admitDisk(ctx, req) {
		return nil // the planner will ask again when there is room
	}

	// Register the row (resume from a prior frontier if any).
	e.mu.Lock()
	fs, ok := e.files[req.key]
	if !ok {
		fs = &fileState{LocalFile: catalog.LocalFile{Hash: req.key.hash, FileID: req.key.fileID, Size: req.size}, rel: req.rel}
		e.files[req.key] = fs
	}
	fs.State = catalog.LocalPulling
	fs.Reason = req.reason
	fs.Error = ""
	if fs.Size == 0 {
		fs.Size = req.size
	}
	frontier := fs.Frontier
	row := fs.LocalFile
	pctx, cancel := context.WithCancel(ctx)
	e.pulling[req.key] = &pullProgress{req: req, started: now, cancel: cancel}
	e.pullLog = append(e.pullLog, now)
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.pulling, req.key)
		e.mu.Unlock()
	}()
	if err := e.store.UpsertLocalFile(&row); err != nil {
		return err
	}

	f, err := e.disk.openPart(req.key.hash, req.rel, frontier)
	if err != nil {
		e.failPull(req.key, err)
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	window := int64(e.cfg.PullWindowMiB) << 20
	buf := make([]byte, window)
	off := frontier
	sincePersist := int64(0)
	first := frontier == 0
	for off < req.size {
		if err := pctx.Err(); err != nil {
			e.persistFrontier(req.key, off)
			return err
		}
		if e.inner.Throttled() {
			select {
			case <-pctx.Done():
				e.persistFrontier(req.key, off)
				return pctx.Err()
			case <-time.After(throttlePause):
			}
			continue
		}
		want := window
		if rem := req.size - off; rem < want {
			want = rem
		}
		rctx := pctx
		var rcancel context.CancelFunc
		if first {
			rctx, rcancel = context.WithTimeout(pctx, firstReadTimeout)
		}
		n, rerr := e.inner.ReadAtCtx(rctx, req.key.hash, req.key.fileID, buf[:want], off)
		if rcancel != nil {
			rcancel()
		}
		if rerr != nil {
			if pctx.Err() != nil {
				// Cancelled (shutdown, eviction, forget): keep the row resumable.
				e.persistFrontier(req.key, off)
				return pctx.Err()
			}
			if first && errors.Is(rerr, context.DeadlineExceeded) {
				// No slot came free in time: not a failure, try again later.
				e.persistFrontier(req.key, off)
				e.deferRequeue(ctx, req, requeueBackoff)
				return nil
			}
			e.persistFrontier(req.key, off)
			e.failPull(req.key, rerr)
			return rerr
		}
		first = false
		if n == 0 {
			err := fmt.Errorf("localcache: short read at %d of %d: %w", off, req.size, io.ErrUnexpectedEOF)
			e.failPull(req.key, err)
			return err
		}
		if _, werr := f.Write(buf[:n]); werr != nil {
			e.failPull(req.key, werr)
			return werr
		}
		off += int64(n)
		sincePersist += int64(n)
		e.mu.Lock()
		if fs := e.files[req.key]; fs != nil {
			fs.Frontier = off
		}
		e.mu.Unlock()
		if sincePersist >= persistEvery {
			if err := f.Sync(); err != nil {
				e.failPull(req.key, err)
				return err
			}
			e.persistFrontier(req.key, off)
			sincePersist = 0
		}
	}

	mtime := time.Unix(0, 0)
	if e.cfg.StableMtime {
		if rel, _, err := e.store.GetRelease(req.key.hash); err == nil && rel != nil && rel.AddedOn > 0 {
			mtime = time.Unix(rel.AddedOn, 0)
		}
	}
	if _, err := e.disk.finalize(f, req.key.hash, req.rel, mtime); err != nil {
		e.failPull(req.key, err)
		return err
	}
	closed = true

	e.mu.Lock()
	fs = e.files[req.key]
	if fs == nil {
		fs = &fileState{LocalFile: catalog.LocalFile{Hash: req.key.hash, FileID: req.key.fileID}, rel: req.rel}
		e.files[req.key] = fs
	}
	fs.State = catalog.LocalComplete
	fs.Frontier = req.size
	fs.Size = req.size
	fs.PulledAt = e.now().Unix()
	fs.Error = ""
	row = fs.LocalFile
	e.mu.Unlock()
	if err := e.store.UpsertLocalFile(&row); err != nil {
		return err
	}
	metrics.IncLocalPull(req.reason)
	e.log.Info("localcache: pulled", "hash", short(req.key.hash), "file_id", req.key.fileID, "bytes", req.size, "reason", req.reason, "took", e.now().Sub(now).Round(time.Second))

	e.linkToCache(req.key, req.rel)
	return nil
}

// admitDisk applies the disk guards to one pull (docs/28 §4.3, §7.1). When the bytes
// about to land would take the volume to or below free_low_pct, the pressure pass runs
// first, as if the pull had already landed: it evicts what the reaper would evict
// right after the pull, in the same class order, never wanted or pinned files. The
// pull then starts only if it leaves at least reserve_pct free.
//
// Without the make-room pass the cache wedges whenever free space sits between the
// low-water mark and size+reserve: the reaper sees no pressure, every pull is refused,
// and evictable files hold the space until their TTLs run out.
func (e *Engine) admitDisk(ctx context.Context, req pullReq) bool {
	d, err := e.statDisk()
	if err != nil {
		return true // statfs failing: no basis to refuse (unchanged behaviour)
	}
	need := e.pullNeed(req)
	if lowWater(DiskStat{Free: d.Free, Total: d.Total, Pending: need}, 0, e.cfg) {
		e.sweep(ctx, need)
		if d2, err := e.statDisk(); err == nil {
			d = d2
		}
		need = e.pullNeed(req) // the pass may have dropped this file's own failed partial
	}
	reserve := d.Total / 100 * uint64(e.cfg.ReservePct) //nolint:gosec // 0..50
	if d.Free < need+reserve {
		metrics.IncLocalSkipped("disk")
		e.log.Warn("localcache: not enough free space to pull", "hash", short(req.key.hash), "need", need, "free", d.Free, "reserve", reserve)
		return false
	}
	return true
}

// pullNeed is the bytes a pull still has to write (size minus a resumable frontier).
func (e *Engine) pullNeed(req pullReq) uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var have int64
	if fs, ok := e.files[req.key]; ok {
		have = fs.Frontier
	}
	return uint64(max(req.size-have, 0)) //nolint:gosec // non-negative
}

// linkToCache retargets the file's library links at the cache copy (and asks the
// media server to refresh the item if configured).
func (e *Engine) linkToCache(key fileKey, rel string) {
	if e.links == nil || e.noLink.Load() {
		return
	}
	if _, ok := e.disk.stat(key.hash, rel); !ok {
		return // never point a library link at bytes that are not there
	}
	n, err := e.links.retarget(key.hash, key.fileID, rel, playback.TargetCache)
	if err != nil {
		e.log.Warn("localcache: retarget to cache", "hash", short(key.hash), "file_id", key.fileID, "err", err)
	}
	if n == 0 {
		return
	}
	e.mu.Lock()
	var row catalog.LocalFile
	fs, ok := e.files[key]
	if ok {
		fs.Linked = true
		row = fs.LocalFile
	}
	ids := e.itemIDs[key]
	e.mu.Unlock()
	if ok {
		_ = e.store.UpsertLocalFile(&row)
	}
	if e.pb.RefreshOnRetarget {
		e.refreshItem(ids)
	}
	e.enqueueAnalysis(key) // no-op unless playback.analysis is enabled
}

func (e *Engine) refreshItem(ids map[string]string) {
	for _, src := range e.sources {
		if id, ok := ids[src.Name()]; ok && id != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := src.Refresh(ctx, id); err != nil {
				e.log.Debug("localcache: refresh after retarget", "server", src.Name(), "err", err)
			}
			cancel()
		}
	}
}

func (e *Engine) persistFrontier(key fileKey, off int64) {
	e.mu.Lock()
	fs, ok := e.files[key]
	if ok {
		fs.Frontier = off
	}
	var row catalog.LocalFile
	if ok {
		row = fs.LocalFile
	}
	e.mu.Unlock()
	if ok {
		_ = e.store.UpsertLocalFile(&row)
	}
}

func (e *Engine) failPull(key fileKey, cause error) {
	metrics.IncLocalPullFailure()
	e.mu.Lock()
	fs, ok := e.files[key]
	if ok {
		fs.State = catalog.LocalFailed
		fs.Error = cause.Error()
	}
	var row catalog.LocalFile
	if ok {
		row = fs.LocalFile
	}
	e.mu.Unlock()
	if ok {
		_ = e.store.UpsertLocalFile(&row)
	}
}

// deferRequeue re-adds the request after d (or immediately on shutdown, harmlessly).
func (e *Engine) deferRequeue(ctx context.Context, req pullReq, d time.Duration) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			e.requeue(req)
			select {
			case e.wake <- struct{}{}:
			default:
			}
		}
	}()
}
