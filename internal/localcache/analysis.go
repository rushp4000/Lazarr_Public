package localcache

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback"
)

// Post-pull media-server analysis (docs/28 §16). After a pull completes and the
// library link points at the cache copy, ask each media server that can analyze on
// demand (playback.Analyzer) to run its file analysis for that item — intro/credits
// detection, loudness, thumbnails — so those whole-file reads hit local disk instead
// of the CDN. One item at a time; the file is held from TTL eviction while its
// analysis is pending, bounded by playback.analysis.max_hold.
//
// Hard rule: a request is only ever sent while every known library link of the file
// points at the cache tree. If the file is not local, the analysis is skipped — never
// run through FUSE, where it would cost a full-file CDN read.

// analysisEnabled reports whether the hook is configured and has somewhere to go.
func (e *Engine) analysisEnabled() bool {
	return e.pb.Analysis.Enabled && e.astore != nil && len(e.analyzers) > 0
}

// enqueueAnalysis queues one file for automatic analysis (deduped). Only files never
// analyzed, or whose last attempt was skipped (not local at the time), are queued:
// a terminal result — done, unavailable, timeout, failed — is never retried
// automatically, so a stuck analysis can never re-pin its file (max_hold is final).
// The Web UI "Analyze" action resets rows first. No-op when disabled.
func (e *Engine) enqueueAnalysis(key fileKey) bool {
	if !e.analysisEnabled() {
		return false
	}
	retry := false
	for _, an := range e.analyzers {
		switch e.analysisRow(key, an.Name()).State {
		case "", catalog.AnalysisPending, catalog.AnalysisRunning, catalog.AnalysisSkipped:
			retry = true
		}
	}
	if !retry {
		return false
	}
	e.mu.Lock()
	if e.aqueued[key] {
		e.mu.Unlock()
		return false
	}
	e.aqueued[key] = true
	e.aq = append(e.aq, key)
	e.mu.Unlock()
	now := e.now().Unix()
	for _, an := range e.analyzers {
		row := e.analysisRow(key, an.Name())
		switch row.State {
		case "", catalog.AnalysisSkipped:
			row.State, row.RequestedAt, row.Error = catalog.AnalysisPending, now, ""
			_ = e.astore.UpsertLocalAnalysis(row)
		case catalog.AnalysisPending, catalog.AnalysisRunning:
			// keep RequestedAt: max_hold counts from the first request
		}
	}
	select {
	case e.awake <- struct{}{}:
	default:
	}
	return true
}

// AnalyzeRelease queues every complete, cache-linked file of a release (Web UI
// "Analyze"), clearing earlier results so the request is re-sent.
func (e *Engine) AnalyzeRelease(hash string) (int, error) {
	if !e.analysisEnabled() {
		return 0, errors.New("playback.analysis is disabled or no media server supports it")
	}
	n := 0
	for _, key := range e.completeKeys(hash) {
		for _, an := range e.analyzers {
			row := e.analysisRow(key, an.Name())
			if !row.State.Active() {
				row.State, row.RequestedAt, row.Error = catalog.AnalysisPending, e.now().Unix(), ""
				_ = e.astore.UpsertLocalAnalysis(row)
			}
		}
		if e.enqueueAnalysis(key) {
			n++
		}
	}
	return n, nil
}

// AnalyzeBackfill queues every complete local file that has no finished analysis yet
// (the one-shot backfill for files cached before the hook was enabled).
func (e *Engine) AnalyzeBackfill() (int, error) {
	if !e.analysisEnabled() {
		return 0, errors.New("playback.analysis is disabled or no media server supports it")
	}
	n := 0
	for _, key := range e.completeKeys("") {
		if e.analysisFinished(key) {
			continue
		}
		if e.enqueueAnalysis(key) {
			n++
		}
	}
	return n, nil
}

func (e *Engine) completeKeys(hash string) []fileKey {
	e.mu.Lock()
	defer e.mu.Unlock()
	var keys []fileKey
	for k, fs := range e.files {
		if fs.State == catalog.LocalComplete && (hash == "" || k.hash == hash) {
			keys = append(keys, k)
		}
	}
	return keys
}

// analysisFinished: every analyzer has a terminal, non-retryable row.
func (e *Engine) analysisFinished(key fileKey) bool {
	for _, an := range e.analyzers {
		switch e.analysisRow(key, an.Name()).State {
		case catalog.AnalysisDone, catalog.AnalysisUnavailable:
		default:
			return false
		}
	}
	return true
}

func (e *Engine) analysisRow(key fileKey, server string) *catalog.LocalAnalysis {
	rows, _ := e.astore.ListLocalAnalysis(key.hash, key.fileID)
	for _, r := range rows {
		if r.Server == server {
			return r
		}
	}
	return &catalog.LocalAnalysis{Hash: key.hash, FileID: key.fileID, Server: server}
}

// runAnalyzer is the single analysis worker.
func (e *Engine) runAnalyzer(ctx context.Context) {
	for {
		e.mu.Lock()
		var key fileKey
		ok := len(e.aq) > 0
		if ok {
			key = e.aq[0]
			e.aq = e.aq[1:]
		}
		e.mu.Unlock()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-e.awake:
			}
			continue
		}
		e.analyzeFile(ctx, key)
		e.mu.Lock()
		delete(e.aqueued, key)
		e.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
	}
}

// analyzeFile runs the analysis of one file on every analyzer, in turn.
func (e *Engine) analyzeFile(ctx context.Context, key fileKey) {
	for _, an := range e.analyzers {
		row := e.analysisRow(key, an.Name())
		if !row.State.Active() {
			continue // done elsewhere, or cleared
		}
		start := e.now()
		e.analyzeOn(ctx, an, key, row)
		if ctx.Err() != nil {
			return // shutdown: the row stays pending/running and resumes on next start
		}
		metrics.IncAnalysis(an.Name(), string(row.State))
		e.log.Info("localcache: analysis finished", "server", an.Name(), "hash", short(key.hash), "file_id", key.fileID,
			"state", row.State, "items", row.ItemIDs, "intro", row.Intro, "credits", row.Credits,
			"took", e.now().Sub(start).Round(time.Second), "err", row.Error)
	}
}

// analyzeOn drives one (file, server) analysis to a terminal state, persisting
// every transition in row.
func (e *Engine) analyzeOn(ctx context.Context, an playback.Analyzer, key fileKey, row *catalog.LocalAnalysis) {
	cfg := e.pb.Analysis
	save := func(st catalog.AnalysisState, msg string) {
		row.State, row.Error = st, msg
		if !st.Active() {
			row.FinishedAt = e.now().Unix()
		}
		_ = e.astore.UpsertLocalAnalysis(row)
	}
	if row.RequestedAt == 0 {
		row.RequestedAt = e.now().Unix()
	}

	// 1. Find the server's item(s) for the library path. Never trigger a scan: the
	// arr's own connect does that; wait up to scan_wait for it.
	var ids []string
	for {
		paths, local := e.analysisPaths(key)
		if !local {
			save(catalog.AnalysisSkipped, "file is not local (link not on the cache tree)")
			return
		}
		for _, p := range paths {
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			found, err := an.FindItems(fctx, p)
			cancel()
			if err != nil {
				e.log.Debug("localcache: analysis find items", "server", an.Name(), "err", err)
			}
			ids = appendUnique(ids, found...)
		}
		if len(ids) > 0 {
			break
		}
		if e.now().Sub(time.Unix(row.RequestedAt, 0)) >= cfg.ScanWait.D() {
			save(catalog.AnalysisUnavailable, "server has not indexed the file within scan_wait")
			return
		}
		if !sleepCtx(ctx, cfg.PollInterval.D()) {
			return
		}
	}

	// 2. Choose actions. Intro detection reads the WHOLE season (verified live), so it
	// is only requested once every episode of the season is local; a movie never gets
	// it. The last episode of a filled season to land retriggers it.
	actions, note := e.analysisActions(ctx, an, ids[0])
	if len(actions) == 0 {
		save(catalog.AnalysisDone, note)
		return
	}

	// 3. Gates. With open_gates, flip the server's detector settings on for this
	// request only — never inside avoid_hours (the server's maintenance window).
	var restore func(context.Context) error
	if cfg.OpenGates {
		for e.inAvoidHours(e.now()) {
			if !sleepCtx(ctx, cfg.PollInterval.D()) {
				return
			}
		}
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r, err := an.OpenGates(gctx, actions)
		cancel()
		if err != nil {
			save(catalog.AnalysisFailed, "open gates: "+err.Error())
			return
		}
		restore = r
		defer e.closeGates(&restore, an)
	}

	// 4. Request. Re-check locality immediately before: the request makes the server
	// read the whole file through whatever the link points at.
	if _, local := e.analysisPaths(key); !local {
		save(catalog.AnalysisSkipped, "file stopped being local before the request")
		return
	}
	row.ItemIDs = strings.Join(ids, ",")
	row.StartedAt = e.now().Unix()
	row.Attempts++
	save(catalog.AnalysisRunning, "")
	for _, id := range ids {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := an.StartAnalysis(rctx, id, actions)
		cancel()
		if err != nil {
			save(catalog.AnalysisFailed, err.Error())
			return
		}
	}

	// 5. Wait until the server has been idle for two consecutive polls (it queues the
	// work as background activities), then count markers. Gates close early if the
	// avoid window starts meanwhile (a running job keeps reading local files only).
	idle := 0
	for idle < 2 {
		if !sleepCtx(ctx, cfg.PollInterval.D()) {
			return
		}
		if restore != nil && e.inAvoidHours(e.now()) {
			e.closeGates(&restore, an)
		}
		if e.now().Sub(time.Unix(row.StartedAt, 0)) >= cfg.Timeout.D() {
			save(catalog.AnalysisTimeout, fmt.Sprintf("still busy after %s", cfg.Timeout.D()))
			return
		}
		busy, intro, credits, err := e.analysisStatus(ctx, an, ids)
		if err != nil {
			e.log.Debug("localcache: analysis status", "server", an.Name(), "err", err)
			idle = 0
			continue
		}
		row.Intro, row.Credits = intro, credits
		if busy {
			idle = 0
		} else {
			idle++
		}
	}
	save(catalog.AnalysisDone, note)
}

// analysisActions drops season-wide actions (intro) unless the whole season is local.
func (e *Engine) analysisActions(ctx context.Context, an playback.Analyzer, itemID string) ([]string, string) {
	var out []string
	note := ""
	for _, a := range e.pb.Analysis.Actions {
		if a != "intro" {
			out = append(out, a)
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		paths, err := an.SeasonPaths(sctx, itemID)
		cancel()
		switch {
		case err != nil:
			note = "intro skipped: " + err.Error()
		case len(paths) == 0:
			// a movie (or a season the server lists empty): no intro detection
		case !e.allLocal(paths):
			note = "intro waits until the whole season is local"
		default:
			out = append(out, a)
		}
	}
	return out, note
}

// allLocal reports whether every library path is a Lazarr link pointing at a cache
// file that is present. Anything else (FUSE, real file, unknown) is not local.
func (e *Engine) allLocal(paths []string) bool {
	for _, p := range paths {
		ref, err := e.resolver.Resolve(p)
		if err != nil || ref.Kind != playback.TargetCache || !cacheFilePresent(ref.Target) {
			return false
		}
	}
	return true
}

// inAvoidHours reports whether t (local time) falls in playback.analysis.avoid_hours.
func (e *Engine) inAvoidHours(t time.Time) bool {
	start, end, err := config.ParseHourWindow(e.pb.Analysis.AvoidHours)
	if err != nil || start == end {
		return false
	}
	lt := t.In(time.Local)
	m := lt.Hour()*60 + lt.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end // wraps midnight
}

// closeGates restores the server's gate settings once (idempotent via *restore).
func (e *Engine) closeGates(restore *func(context.Context) error, an playback.Analyzer) {
	if restore == nil || *restore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := (*restore)(ctx); err != nil {
		e.log.Error("localcache: could not close analysis gates — check the server's detector settings", "server", an.Name(), "err", err)
		return
	}
	*restore = nil
}

func (e *Engine) analysisStatus(ctx context.Context, an playback.Analyzer, ids []string) (busy bool, intro, credits int, err error) {
	for _, id := range ids {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		st, serr := an.AnalysisStatus(sctx, id)
		cancel()
		if serr != nil {
			return false, 0, 0, serr
		}
		busy = busy || st.Busy
		intro += st.Intro
		credits += st.Credits
	}
	return busy, intro, credits, nil
}

// analysisPaths returns the library paths a media server indexes for the file
// (download-dir links excluded) and whether the file is local: complete on disk and
// every known link points at the cache tree.
func (e *Engine) analysisPaths(key fileKey) (paths []string, local bool) {
	e.mu.Lock()
	fs, ok := e.files[key]
	complete := ok && fs.State == catalog.LocalComplete
	rel := ""
	if ok {
		rel = fs.rel
	}
	e.mu.Unlock()
	if !complete || e.links == nil {
		return nil, false
	}
	if _, onDisk := e.disk.stat(key.hash, rel); !onDisk {
		return nil, false
	}
	links, err := e.store.ListLibraryLinks(key.hash, key.fileID)
	if err != nil || len(links) == 0 {
		return nil, false
	}
	dl := filepath.Clean(e.downloadDir) + string(filepath.Separator)
	for _, l := range links {
		if l.TargetKind != playback.TargetCache {
			return nil, false
		}
		if e.downloadDir != "" && strings.HasPrefix(l.LinkPath, dl) {
			continue
		}
		paths = append(paths, l.LinkPath)
	}
	return paths, len(paths) > 0
}

// analysisHolds returns the files whose analysis is still pending within max_hold;
// rows past it are given up (state timeout) and logged.
func (e *Engine) analysisHolds(now time.Time) map[fileKey]bool {
	holds := map[fileKey]bool{}
	if e.astore == nil {
		return holds
	}
	rows, err := e.astore.ListActiveAnalysis()
	if err != nil {
		return holds
	}
	maxHold := e.pb.Analysis.MaxHold.D()
	for _, r := range rows {
		if now.Sub(time.Unix(r.RequestedAt, 0)) >= maxHold {
			r.State, r.Error, r.FinishedAt = catalog.AnalysisTimeout, "gave up: max_hold reached", now.Unix()
			_ = e.astore.UpsertLocalAnalysis(r)
			metrics.IncAnalysis(r.Server, string(catalog.AnalysisTimeout))
			e.log.Warn("localcache: gave up waiting for media-server analysis; file may now be evicted",
				"server", r.Server, "hash", short(r.Hash), "file_id", r.FileID, "max_hold", maxHold)
			continue
		}
		holds[fileKey{r.Hash, r.FileID}] = true
	}
	metrics.SetAnalysisActive(len(holds))
	return holds
}

// resumeAnalysis re-queues pending/running rows after a restart.
func (e *Engine) resumeAnalysis() {
	rows, err := e.astore.ListActiveAnalysis()
	if err != nil {
		return
	}
	e.mu.Lock()
	for _, r := range rows {
		k := fileKey{r.Hash, r.FileID}
		if _, ok := e.files[k]; ok && !e.aqueued[k] {
			e.aqueued[k] = true
			e.aq = append(e.aq, k)
		}
	}
	e.mu.Unlock()
}

// preflightAnalysis resets gates (open_gates) and logs, once, why the servers would
// ignore the configured actions.
func (e *Engine) preflightAnalysis(ctx context.Context) {
	for _, an := range e.analyzers {
		if e.pb.Analysis.OpenGates {
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := an.ResetGates(rctx, e.pb.Analysis.Actions); err != nil {
				e.log.Error("localcache: analysis: could not reset detector gates", "server", an.Name(), "err", err)
			}
			cancel()
			continue // the gates are Lazarr's to open; "never" warnings do not apply
		}
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		warns, err := an.Preflight(pctx, e.pb.Analysis.Actions)
		cancel()
		if err != nil {
			e.log.Warn("localcache: analysis preflight", "server", an.Name(), "err", err)
		}
		for _, w := range warns {
			e.log.Warn("localcache: analysis preflight", "server", an.Name(), "warning", w)
		}
	}
}

func appendUnique(dst []string, src ...string) []string {
	for _, s := range src {
		dup := false
		for _, d := range dst {
			if d == s {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, s)
		}
	}
	return dst
}

// sleepCtx waits d or until ctx is done; false means ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// closeAnalysis marks any still-active analysis of an evicted/dropped file skipped,
// so it neither holds anything nor counts as active.
func (e *Engine) closeAnalysis(key fileKey, why string) {
	if e.astore == nil {
		return
	}
	rows, err := e.astore.ListLocalAnalysis(key.hash, key.fileID)
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.State.Active() {
			r.State, r.Error, r.FinishedAt = catalog.AnalysisSkipped, why, e.now().Unix()
			_ = e.astore.UpsertLocalAnalysis(r)
		}
	}
}
