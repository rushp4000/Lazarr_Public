package localcache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback"
)

// runPoller polls every source on playback.poll_interval (0 = disabled).
func (e *Engine) runPoller(ctx context.Context) {
	iv := e.pb.PollInterval.D()
	if iv <= 0 || len(e.sources) == 0 {
		return
	}
	// First poll shortly after start, then on the interval.
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.PollOnce(ctx)
			t.Reset(iv)
		}
	}
}

// PollOnce reads every source once and plans pulls (docs/28 §7.2). Exported for the
// Web UI's "poll now" and for tests.
func (e *Engine) PollOnce(ctx context.Context) {
	now := e.now()
	var all []playback.Session
	for _, src := range e.sources {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ss, err := src.Sessions(sctx)
		cancel()
		if err != nil {
			metrics.IncPlaybackPollError(src.Name())
			e.log.Warn("localcache: session poll failed", "server", src.Name(), "err", err)
			continue
		}
		for _, s := range ss {
			if s.Server == "" {
				s.Server = src.Name()
			}
			all = append(all, s)
		}
		e.planSessions(ctx, src, ss, now)
	}
	metrics.SetPlaybackSessions(len(all))
	e.mu.Lock()
	e.sessions = all
	e.lastPoll = now
	e.mu.Unlock()
}

// planSessions upserts watch rows, marks the wanted set, and enqueues pulls for
// one source's sessions. Files in the wanted set that no session covers any more
// simply drop out on the next poll (the set is rebuilt each time, per source).
func (e *Engine) planSessions(ctx context.Context, src playback.Source, ss []playback.Session, now time.Time) {
	wanted := make(map[fileKey]struct{})
	for _, s := range ss {
		if s.Path == "" {
			continue
		}
		ref, err := e.resolver.Resolve(s.Path)
		if err != nil {
			if !errors.Is(err, playback.ErrNotLazarr) {
				e.log.Debug("localcache: resolve session path", "path", s.Path, "err", err)
			}
			continue
		}
		rel, files, err := e.store.GetRelease(ref.Hash)
		if err != nil || rel == nil {
			continue
		}
		file, ok := fileByRel(files, ref.RelPath)
		if !ok {
			continue
		}
		key := fileKey{rel.Hash, file.FileID}
		if e.links != nil {
			e.links.observe(ref, file.FileID)
		}
		e.recordWatch(s, rel, file, now)
		e.noteItem(key, s)
		if e.cfg.TorrentOnly && rel.Source != catalog.SourceTorrent {
			metrics.IncLocalSkipped("source")
			continue // observed for the record, but never cached
		}
		wanted[key] = struct{}{}

		// Prefetch first (it is what needs to be ready), then the current file.
		if s.Kind == playback.KindEpisode && e.cfg.PrefetchAhead > 0 {
			nctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			next, nerr := src.NextEpisodes(nctx, s, e.cfg.PrefetchAhead)
			cancel()
			if nerr != nil {
				e.log.Debug("localcache: next episodes", "server", src.Name(), "series", s.SeriesTitle, "err", nerr)
			}
			for _, it := range next {
				nref, err := e.resolver.Resolve(it.Path)
				if err != nil {
					continue
				}
				nrel, nfiles, err := e.store.GetRelease(nref.Hash)
				if err != nil || nrel == nil {
					continue
				}
				nf, ok := fileByRel(nfiles, nref.RelPath)
				if !ok {
					continue
				}
				nkey := fileKey{nrel.Hash, nf.FileID}
				if e.links != nil {
					e.links.observe(nref, nf.FileID)
				}
				if e.cfg.TorrentOnly && nrel.Source != catalog.SourceTorrent {
					metrics.IncLocalSkipped("source")
					continue
				}
				wanted[nkey] = struct{}{}
				e.enqueueFile(nrel, nf, ReasonPrefetch)
			}
		}
		if e.cfg.PullCurrent {
			e.enqueueFile(rel, file, ReasonWatch)
		}
		if s.Kind == playback.KindEpisode {
			e.maybeFillSeason(ctx, src, s, now)
		}
	}
	e.mu.Lock()
	// Merge: keys from other sources stay until their own source's next poll
	// replaces them. Simplicity over precision — a stale wanted key only delays an
	// eviction by one poll interval.
	for k := range e.wanted {
		if e.wantedBy(k) == src.Name() {
			delete(e.wanted, k)
		}
	}
	for k := range wanted {
		e.wanted[k] = struct{}{}
		e.setWantedBy(k, src.Name())
	}
	e.mu.Unlock()
}

// wantedBy / setWantedBy remember which source asserted a wanted key (stored in the
// itemIDs map under a reserved pseudo-server key so no new field is needed).
const wantedByKey = "\x00wanted_by"

func (e *Engine) wantedBy(k fileKey) string {
	if m := e.itemIDs[k]; m != nil {
		return m[wantedByKey]
	}
	return ""
}

func (e *Engine) setWantedBy(k fileKey, src string) {
	if e.itemIDs[k] == nil {
		e.itemIDs[k] = map[string]string{}
	}
	e.itemIDs[k][wantedByKey] = src
}

func fileByRel(files []catalog.File, rel string) (catalog.File, bool) {
	for _, f := range files {
		if f.RelPath == rel {
			return f, true
		}
	}
	return catalog.File{}, false
}

func (e *Engine) recordWatch(s playback.Session, rel *catalog.Release, f catalog.File, now time.Time) {
	pct := s.ProgressPct()
	w := &catalog.Watch{
		Server:      s.Server,
		UserID:      s.UserID,
		UserName:    s.UserName,
		Hash:        rel.Hash,
		FileID:      f.FileID,
		SeriesKey:   s.SeriesKey,
		Season:      s.Season,
		Episode:     s.Episode,
		ProgressPct: pct,
		Completed:   pct >= float64(e.pb.WatchedPct),
		FirstSeen:   now.Unix(),
		LastSeen:    now.Unix(),
	}
	if err := e.store.UpsertWatch(w); err != nil {
		e.log.Warn("localcache: upsert watch", "err", err)
	}
	if !s.ReleaseDate.IsZero() {
		e.mu.Lock()
		e.obsDate[rel.Hash] = s.ReleaseDate
		e.mu.Unlock()
	}
}

func (e *Engine) noteItem(key fileKey, s playback.Session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.itemIDs[key] == nil {
		e.itemIDs[key] = map[string]string{}
	}
	e.itemIDs[key][s.Server] = s.ItemID
}

// isWanted reports whether the last poll placed the file in a viewer's window. A
// stale poll (source down) never keeps files pinned forever.
func (e *Engine) isWanted(key fileKey, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastPoll.IsZero() || now.Sub(e.lastPoll) > 3*e.pb.PollInterval.D()+time.Minute {
		return false
	}
	_, ok := e.wanted[key]
	return ok
}

// OnGrab is the qbit GrabHook (docs/28 §7.3): for categories in pull_on_grab, pull
// every eligible file of the new release immediately.
func (e *Engine) OnGrab(rel *catalog.Release, files []catalog.File) {
	if rel == nil || !e.pullOnGrab(rel.Category) {
		return
	}
	for _, f := range files {
		e.enqueueFile(rel, f, ReasonGrab)
	}
}

func (e *Engine) pullOnGrab(category string) bool {
	for _, c := range e.cfg.PullOnGrab {
		if c == category {
			return true
		}
	}
	return false
}

// seasonFillRecheck bounds how often one viewer's season is re-examined.
const seasonFillRecheck = 30 * time.Minute

// maybeFillSeason queues the rest of the session's season (docs/28 §17) once this
// viewer has finished season_fill.after_completed of its episodes. Speculative:
// queued behind live-session pulls and capped by speculative_gb_per_day.
func (e *Engine) maybeFillSeason(ctx context.Context, src playback.Source, s playback.Session, now time.Time) {
	sf := e.cfg.SeasonFill
	if !sf.Enabled || e.cfg.SpeculativeGBPerDay <= 0 || s.SeriesKey == "" || s.Season <= 0 {
		return
	}
	key := fmt.Sprintf("%s|%s|%s|%d", src.Name(), s.UserID, s.SeriesKey, s.Season)
	e.mu.Lock()
	last, seen := e.seasonFilled[key]
	e.mu.Unlock()
	if seen && now.Sub(last) < seasonFillRecheck {
		return
	}
	if sf.AfterCompleted > 0 {
		rows, err := e.store.ListWatchSince(now.Add(-90 * 24 * time.Hour).Unix())
		if err != nil {
			return
		}
		done := 0
		for _, w := range rows {
			if w.Server == src.Name() && w.UserID == s.UserID && w.SeriesKey == s.SeriesKey && w.Season == s.Season && w.Completed {
				done++
			}
		}
		if done < sf.AfterCompleted {
			return // not committed yet; ask again on a later poll
		}
	}
	nctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	next, err := src.NextEpisodes(nctx, s, sf.MaxEpisodes)
	cancel()
	if err != nil {
		e.log.Debug("localcache: season fill: next episodes", "server", src.Name(), "series", s.SeriesTitle, "err", err)
		return
	}
	queued := 0
	for _, it := range next {
		if it.Season != s.Season {
			continue // the rest of THIS season only
		}
		ref, err := e.resolver.Resolve(it.Path)
		if err != nil {
			continue
		}
		rel, files, err := e.store.GetRelease(ref.Hash)
		if err != nil || rel == nil {
			continue
		}
		f, ok := fileByRel(files, ref.RelPath)
		if !ok {
			continue
		}
		if e.links != nil {
			e.links.observe(ref, f.FileID)
		}
		if e.enqueueFile(rel, f, ReasonSeason) {
			queued++
		}
	}
	e.mu.Lock()
	e.seasonFilled[key] = now
	e.mu.Unlock()
	if queued > 0 {
		e.log.Info("localcache: season fill queued", "server", src.Name(), "series", s.SeriesTitle, "season", s.Season, "episodes", queued)
	}
}
