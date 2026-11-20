package qbit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/torbox"
)

// The slow lane (slow_lane.*, v1.2.9): uncached torrents TorBox needs hours or days to
// fetch — old shows, multi-season packs, thin swarms. on_cache_miss=wait is the wrong
// tool for those: it holds the arr's grab open, bails after cache_wait_budget, and its
// two slots fill with stalled downloads (46 refusals in one afternoon, 2026-10-12).
//
// The slow lane is a persistent queue (catalog.SlowLaneStore) worked by one goroutine:
//
//	queued      -> nothing on the account; started when a slot is free (max_concurrent)
//	downloading -> TorBox is fetching it; polled every poll_interval, no time budget
//	               (only slow_lane.max_age)
//	cached      -> TorBox has it cached; the account copy is released (like a finished
//	               wait download) and the content gate checks the real file list
//	handed_off  -> given back to the arr (see handOff)
//	imported    -> a library symlink points at it (the arr imported it)
//
// Items come from the Web UI / API (origin "manual") or are adopted from wait downloads
// that failed only for being slow (origin "wait", slow_lane.adopt_wait_failures).
//
// Every TorBox call goes through the shared client, i.e. through the rate-limit governor:
// a backoff, a 429 or the hourly createtorrent cap simply defers the work to the next
// tick. The ToS audit counts downloading items as legitimately held (SlowLaneHeldIDs).

// SlowLaneArr is the slice of an arr client the slow lane needs to give a download the
// arr already failed back to it. Satisfied by *arr.Client.
type SlowLaneArr interface {
	Regrab(ctx context.Context, hash string) (arr.RegrabResult, error)
}

// SlowLaneImporter is the optional slice of an arr client the automatic manual import
// needs (slow_lane.auto_import, see slowAutoImports). Satisfied by *arr.Client; an arr in
// SlowLaneArrs without it is simply never auto-imported into.
type SlowLaneImporter interface {
	ImportMissing(ctx context.Context, folder, downloadID string, t arr.ImportTarget) (arr.ImportResult, error)
}

const (
	// slowMaxAttempts: consecutive start / hand-off errors before an item fails.
	slowMaxAttempts = 5
	// slowCacheLag: a finished download may not show in checkcached right away.
	slowCacheLag = 30 * time.Minute
	// slowRetention: finished items stay listed this long, then are pruned.
	slowRetention = 30 * 24 * time.Hour
	// slowImportWindow / slowImportChecksPerTick bound the "did the arr import it" check
	// (a library walk per item when the link index is stale).
	slowImportWindow        = 48 * time.Hour
	slowImportChecksPerTick = 5
	// slowArrTimeout bounds one arr hand-off (history + blocklist + search).
	slowArrTimeout = 45 * time.Second
	// slowLinkTimeout bounds one library-link lookup.
	slowLinkTimeout = 20 * time.Second
	// slowAutoImportMaxAttempts caps the automatic manual imports of one item;
	// slowAutoImportsPerTick bounds one tick (each attempt makes the arr probe every file
	// of the folder); slowAutoImportTimeout bounds one attempt (list + command + verify).
	slowAutoImportMaxAttempts = 3
	slowAutoImportsPerTick    = 2
	slowAutoImportTimeout     = 10 * time.Minute
)

// ErrSlowLaneDisabled is returned by the API entry points when slow_lane.enabled is off.
var ErrSlowLaneDisabled = errors.New("the slow lane is disabled (slow_lane.enabled)")

// ErrSlowLaneActive is returned by SlowLaneEnqueue for a hash that is already queued,
// downloading or awaiting hand-off.
var ErrSlowLaneActive = errors.New("already in the slow lane")

// errSlowDefer stops starting more queued items this tick (rate limit, backoff, TorBox
// parked the add): retrying immediately would only burn the budget.
var errSlowDefer = errors.New("slow lane: deferred to the next tick")

// slowWaitReasons are the wait-download failures that mean "too slow", not "broken".
var slowWaitReasons = []string{"budget elapsed", "eta over budget", "max wait elapsed", "no progress, no peers"}

func (s *server) slowOn() bool {
	return s.deps.SlowLane != nil && s.deps.Config.SlowLane.Enabled
}

// ── API entry points ─────────────────────────────────────────────────────────

// slowAdd is one manual slow-lane add, already parsed from a magnet / infohash or a
// .torrent file. Every manual entry point funnels into slowEnqueue with one of these.
type slowAdd struct {
	Hash     string                  // 40-hex v1 infohash
	Name     string                  // display name ("" until TorBox reports one)
	Magnet   string                  // what TorBox gets on start (with trackers when known)
	Category string                  // validated qBit category: the arr that should import it
	Note     string                  // detail shown on the Slow lane tab while the item is active
	Target   *catalog.SlowLaneTarget // optional: what it is for (automatic manual import)
}

// SlowLaneEnqueue queues a magnet link or bare infohash (hex or base32) for category.
// target (optional) names the series/season or movie it is for; the automatic manual
// import then imports only files that map to it. A finished item for the same hash is
// replaced; an active one returns ErrSlowLaneActive together with the existing item.
func (s *server) SlowLaneEnqueue(input, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error) {
	if !s.slowOn() {
		return nil, ErrSlowLaneDisabled
	}
	category, err := s.slowCategory(category)
	if err != nil {
		return nil, err
	}
	hash, name, magnet, err := parseSlowInput(input)
	if err != nil {
		return nil, err
	}
	target, err = s.slowTarget(target, category)
	if err != nil {
		return nil, err
	}
	return s.slowEnqueue(slowAdd{Hash: hash, Name: name, Magnet: magnet, Category: category, Target: target})
}

// SlowLaneEnqueueTorrent queues the content of an uploaded .torrent file for category,
// optionally for a target (validated as in SlowLaneEnqueue). The stored magnet carries
// the torrent's trackers (thin swarms need them). Private and v2-only torrents are
// refused. Same duplicate semantics as SlowLaneEnqueue.
func (s *server) SlowLaneEnqueueTorrent(torrent []byte, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error) {
	if !s.slowOn() {
		return nil, ErrSlowLaneDisabled
	}
	category, err := s.slowCategory(category)
	if err != nil {
		return nil, err
	}
	target, err = s.slowTarget(target, category)
	if err != nil {
		return nil, err
	}
	add, err := parseSlowTorrent(torrent)
	if err != nil {
		return nil, err
	}
	add.Category, add.Target = category, target
	return s.slowEnqueue(add)
}

// slowCategory validates a manual add's category against the configured ones.
func (s *server) slowCategory(category string) (string, error) {
	category = strings.TrimSpace(category)
	if category == "" {
		return "", fmt.Errorf("a category is required (the arr that should import it)")
	}
	if cats := s.deps.Config.Categories; len(cats) > 0 && !slices.Contains(cats, category) {
		return "", fmt.Errorf("unknown category %q (configured: %s)", category, strings.Join(cats, ", "))
	}
	return category, nil
}

// slowEnqueue stores a parsed manual add as a queued item (origin "manual"), under the
// hash lock. An active item for the hash wins: it is returned with ErrSlowLaneActive.
func (s *server) slowEnqueue(a slowAdd) (*catalog.SlowLaneItem, error) {
	unlock := s.locks.lock(a.Hash)
	defer unlock()
	if old, gerr := s.deps.SlowLane.GetSlowLane(a.Hash); gerr == nil && old.State.Active() {
		return old, ErrSlowLaneActive
	}
	now := s.now().Unix()
	it := &catalog.SlowLaneItem{
		Hash: a.Hash, Magnet: a.Magnet, Name: a.Name, Category: a.Category, Origin: "manual",
		State: catalog.SlowQueued, QueuedAt: now, UpdatedAt: now, Note: a.Note, Target: a.Target,
	}
	if err := s.deps.SlowLane.UpsertSlowLane(it); err != nil {
		return nil, err
	}
	metrics.IncSlowLane("queued", "manual")
	slog.Info("slow lane: queued", "hash", a.Hash, "name", a.Name, "category", a.Category, "note", a.Note, "target", a.Target)
	return it, nil
}

// SlowLaneCancel removes an active item: a running TorBox download is deleted from the
// account first (an error leaves the item as it was, so nothing leaks). A download that
// is already gone from the account (deleted on the dashboard, or removed by TorBox's
// stalled-download sweep) counts as deleted.
func (s *server) SlowLaneCancel(hash string) error {
	if s.deps.SlowLane == nil {
		return ErrSlowLaneDisabled
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	unlock := s.locks.lock(hash)
	defer unlock()
	it, err := s.deps.SlowLane.GetSlowLane(hash)
	if err != nil {
		return err
	}
	if !it.State.Active() {
		return fmt.Errorf("item is already %s", it.State)
	}
	if it.State == catalog.SlowDownloading && it.TorBoxID != 0 {
		if err := s.slowDeleteDownload(it); err != nil {
			return fmt.Errorf("delete the TorBox download: %w", err)
		}
	}
	s.slowFinish(it, catalog.SlowCancelled, "", "cancelled from the Web UI")
	metrics.IncSlowLane("cancelled", "")
	slog.Info("slow lane: cancelled", "hash", hash, "name", it.Name, "torbox_id", it.TorBoxID)
	return nil
}

// SlowLaneList returns every item (newest first) for the Web UI.
func (s *server) SlowLaneList() ([]*catalog.SlowLaneItem, error) {
	if s.deps.SlowLane == nil {
		return nil, nil
	}
	items, err := s.deps.SlowLane.ListSlowLane()
	if err != nil {
		return nil, err
	}
	slices.Reverse(items)
	return items, nil
}

// SlowLaneHeldIDs returns the TorBox ids of downloading items, which the ToS audit must
// count as legitimately held (never a leak, never an orphan to delete).
func (s *server) SlowLaneHeldIDs() ([]int64, error) {
	if s.deps.SlowLane == nil {
		return nil, nil
	}
	items, err := s.deps.SlowLane.ListSlowLane(catalog.SlowDownloading)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if it.TorBoxID != 0 {
			ids = append(ids, it.TorBoxID)
		}
	}
	return ids, nil
}

// slowTarget validates an add's optional target and returns a cleaned copy (nil when it
// names nothing). The item carries it from enqueue on; only the automatic manual import
// reads it. A series target for a category that imports movies (or a movie target for a
// Sonarr category) is refused: the import could never use it, and it usually means the
// wrong arr was picked (a TV episode queued under radarr_main, 2026-10-14).
func (s *server) slowTarget(t *catalog.SlowLaneTarget, category string) (*catalog.SlowLaneTarget, error) {
	if t.IsZero() {
		return nil, nil
	}
	if t.SeriesID < 0 || t.Season < 0 || t.MovieID < 0 {
		return nil, fmt.Errorf("target ids must not be negative")
	}
	if t.SeriesID != 0 && t.MovieID != 0 {
		return nil, fmt.Errorf("a target is either a series (series_id) or a movie (movie_id), not both")
	}
	kind := s.deps.Config.CategoryKind(category)
	switch {
	case kind == "radarr" && (t.SeriesID != 0 || t.Season != 0):
		return nil, fmt.Errorf("the target is a TV series but category %s imports movies (Radarr): pick the Sonarr category", category)
	case kind == "sonarr" && t.MovieID != 0:
		return nil, fmt.Errorf("the target is a movie but category %s imports TV series (Sonarr): pick the Radarr category", category)
	}
	c := *t
	c.Title = strings.TrimSpace(c.Title)
	return &c, nil
}

// parseSlowInput accepts a magnet link or a bare 40-hex / 32-base32 infohash. A magnet is
// normalized first (normalizeMagnet: HTML-escaped "&amp;" from copied links); the
// cleaned link, with all its trackers, is what gets stored and sent to TorBox.
func parseSlowInput(in string) (hash, name, magnet string, err error) {
	in = normalizeMagnet(in)
	if strings.HasPrefix(in, "magnet:") {
		hash, name = parseMagnet(in)
		magnet = in
	} else {
		hash = normalizeInfohash(in)
		magnet = "magnet:?xt=urn:btih:" + hash
	}
	if !isInfohash(hash) {
		return "", "", "", fmt.Errorf("not a magnet link or infohash")
	}
	if strings.EqualFold(name, hash) {
		name = ""
	}
	return hash, name, magnet, nil
}

// parseSlowTorrent reads an uploaded .torrent into a slow-lane add: v1 infohash, name,
// and a magnet carrying its trackers.
func parseSlowTorrent(data []byte) (slowAdd, error) {
	m, err := parseTorrentBytes(data)
	if err != nil {
		return slowAdd{}, fmt.Errorf("not a valid .torrent file: %w", err)
	}
	if m.V2Only {
		return slowAdd{}, fmt.Errorf("BitTorrent v2-only torrent: it has no v1 (btih) infohash, which TorBox needs")
	}
	if m.Private {
		return slowAdd{}, fmt.Errorf("private torrent refused: TorBox would announce with your private-tracker " +
			"passkey, and private swarms do not work from TorBox anyway")
	}
	if !isInfohash(m.Hash) {
		return slowAdd{}, fmt.Errorf("not a valid .torrent file: bad infohash")
	}
	note := "uploaded .torrent"
	switch n := len(m.Trackers); n {
	case 0:
		note += " (no trackers: DHT only)"
	case 1:
		note += " (1 tracker)"
	default:
		note += fmt.Sprintf(" (%d trackers)", n)
	}
	return slowAdd{Hash: m.Hash, Name: m.Name, Magnet: buildMagnet(m.Hash, m.Name, m.Trackers), Note: note}, nil
}

// buildMagnet builds magnet:?xt=urn:btih:<hash>&dn=<name>&tr=<tracker>…, every value
// percent-encoded (spaces as %20, which parseMagnet and TorBox both decode).
func buildMagnet(hash, name string, trackers []string) string {
	enc := func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(hash)
	if name != "" {
		b.WriteString("&dn=")
		b.WriteString(enc(name))
	}
	for _, tr := range trackers {
		b.WriteString("&tr=")
		b.WriteString(enc(tr))
	}
	return b.String()
}

// ── adoption of slow wait downloads ──────────────────────────────────────────

// adoptWaitDownload is called by bailWaitDownload (under the hash lock) before it
// deletes a wait download. When the failure only means "too slow" and adoption is on,
// the download moves to the slow lane: kept running on TorBox when a slow-lane slot is
// free (returns true: the caller must NOT delete it), otherwise queued for a later
// restart (returns false: the caller deletes the transfer as usual). Either way the
// caller still fails the grab in the arr.
func (s *server) adoptWaitDownload(rel *catalog.Release, reason string) bool {
	if !s.slowOn() || !s.deps.Config.SlowLane.AdoptWaitFailures || !slices.Contains(slowWaitReasons, reason) {
		return false
	}
	if old, err := s.deps.SlowLane.GetSlowLane(rel.Hash); err == nil && old.State.Active() {
		return false // already there (re-grabbed while queued): let the wait bail normally
	}
	running, err := s.deps.SlowLane.ListSlowLane(catalog.SlowDownloading)
	if err != nil {
		slog.Warn("slow lane: adopt: list", "hash", rel.Hash, "err", err)
		return false
	}
	now := s.now().Unix()
	it := &catalog.SlowLaneItem{
		Hash: rel.Hash, Magnet: rel.Magnet, Name: rel.Name, Category: rel.Category, Origin: "wait",
		State: catalog.SlowQueued, QueuedAt: now, UpdatedAt: now,
		Note: "adopted from a wait download (" + reason + ")",
	}
	if p, ok := s.waits.get(rel.Hash); ok {
		it.Progress, it.ETA = p.Progress, p.ETA
	}
	if it.Magnet == "" {
		it.Magnet = "magnet:?xt=urn:btih:" + rel.Hash
	}
	keep := len(running) < s.deps.Config.SlowLane.MaxConcurrent && rel.TorBoxID != 0
	if keep {
		it.State, it.TorBoxID, it.StartedAt = catalog.SlowDownloading, rel.TorBoxID, rel.MaterializedAt
	}
	if err := s.deps.SlowLane.UpsertSlowLane(it); err != nil {
		slog.Warn("slow lane: adopt: store", "hash", rel.Hash, "err", err)
		return false
	}
	metrics.IncSlowLane("adopted", map[bool]string{true: "kept", false: "queued"}[keep])
	slog.Info("slow lane: adopted slow wait download", "hash", rel.Hash, "name", rel.Name,
		"reason", reason, "kept_downloading", keep, "torbox_id", it.TorBoxID)
	return keep
}

// ── worker ───────────────────────────────────────────────────────────────────

// StartSlowLane launches the slow-lane worker (no-op when disabled): one tick shortly
// after boot — which also resumes every downloading item from the DB — then every
// slow_lane.poll_interval. Stops when ctx is canceled.
func (s *server) StartSlowLane(ctx context.Context) {
	if !s.slowOn() {
		return
	}
	s.slowCtx = ctx
	every := s.deps.Config.SlowLane.PollInterval.D()
	slog.Info("slow lane enabled", "max_concurrent", s.deps.Config.SlowLane.MaxConcurrent,
		"poll_interval", every, "max_age", s.deps.Config.SlowLane.MaxAge.D(),
		"adopt_wait_failures", s.deps.Config.SlowLane.AdoptWaitFailures, "arrs", len(s.deps.SlowLaneArrs))
	go func() {
		first := time.NewTimer(time.Minute)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			s.slowTick()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// slowTick runs one pass: advance downloads, retry hand-offs, start queued items while
// slots are free, check recent hand-offs for an import, prune old history.
func (s *server) slowTick() {
	all, err := s.deps.SlowLane.ListSlowLane()
	if err != nil {
		slog.Warn("slow lane: list", "err", err)
		return
	}
	now := s.now()
	maxAge := s.deps.Config.SlowLane.MaxAge.D()
	for _, it := range all {
		if it.State != catalog.SlowDownloading && it.State != catalog.SlowCached && it.State != catalog.SlowQueued {
			continue
		}
		s.withItem(it.Hash, func(cur *catalog.SlowLaneItem) {
			if maxAge > 0 && cur.State != catalog.SlowCached && now.Sub(time.Unix(cur.QueuedAt, 0)) >= maxAge {
				s.slowGiveUp(cur, "max_age", fmt.Sprintf("not cached within slow_lane.max_age (%s)", maxAge))
				return
			}
			switch cur.State {
			case catalog.SlowDownloading:
				s.slowPoll(cur)
			case catalog.SlowCached:
				s.slowHandOff(cur, nil)
			}
		})
	}

	// Start queued items, oldest first, while slots are free.
	queued, err := s.deps.SlowLane.ListSlowLane(catalog.SlowQueued)
	if err == nil {
		for _, it := range queued {
			running, lerr := s.deps.SlowLane.ListSlowLane(catalog.SlowDownloading)
			if lerr != nil || len(running) >= s.deps.Config.SlowLane.MaxConcurrent {
				break
			}
			var serr error
			s.withItem(it.Hash, func(cur *catalog.SlowLaneItem) {
				if cur.State == catalog.SlowQueued {
					serr = s.slowStart(cur)
				}
			})
			if errors.Is(serr, errSlowDefer) {
				break
			}
		}
	}

	s.slowCheckImports(now)
	s.slowAutoImports(now)
	if n, perr := s.deps.SlowLane.PruneSlowLane(now.Add(-slowRetention).Unix()); perr == nil && n > 0 {
		slog.Info("slow lane: pruned finished items", "count", n)
	}
	s.slowGauge()
}

// withItem re-reads the item under its hash lock and runs fn on the fresh copy (a
// cancel, an adoption or a re-grab may have changed it since the list query).
func (s *server) withItem(hash string, fn func(*catalog.SlowLaneItem)) {
	unlock := s.locks.lock(hash)
	defer unlock()
	cur, err := s.deps.SlowLane.GetSlowLane(hash)
	if err != nil || !cur.State.Active() {
		return
	}
	fn(cur)
}

// slowStart begins one queued item: straight to hand-off when TorBox already has it
// cached, else an uncached createtorrent. Returns errSlowDefer when TorBox says "not
// now" (rate limit, backoff, add parked upstream).
func (s *server) slowStart(it *catalog.SlowLaneItem) error {
	cached, err := s.deps.TorBox.CheckCached([]string{it.Hash})
	if err != nil {
		s.slowNote(it, "checkcached: "+err.Error())
		if errors.Is(err, torbox.ErrAPIBackoff) {
			return errSlowDefer
		}
		return s.slowError(it, "start_error", err)
	}
	if item, ok := cached[it.Hash]; ok && len(item.Files) > 0 {
		it.State, it.CachedAt, it.Progress = catalog.SlowCached, s.now().Unix(), 1
		metrics.IncSlowLane("cached", "already")
		s.slowHandOff(it, &item)
		return nil
	}
	id, _, err := s.deps.TorBox.CreateTorrent(it.Magnet, false /* uncached add */)
	switch {
	case err == nil:
	case errors.Is(err, torbox.ErrRateLimited), errors.Is(err, torbox.ErrAlreadyQueued), errors.Is(err, torbox.ErrAPIBackoff):
		s.slowNote(it, "TorBox deferred the add: "+err.Error())
		metrics.IncSlowLane("start_deferred", "")
		return errSlowDefer
	default:
		return s.slowError(it, "start_error", err)
	}
	now := s.now().Unix()
	it.State, it.TorBoxID, it.StartedAt, it.Attempts, it.LastError = catalog.SlowDownloading, id, now, 0, ""
	s.slowSave(it)
	metrics.IncSlowLane("started", "")
	slog.Info("slow lane: TorBox download started", "hash", it.Hash, "name", it.Name, "torbox_id", id)
	return nil
}

// slowPoll advances one downloading item by one mylist observation.
func (s *server) slowPoll(it *catalog.SlowLaneItem) {
	detail, err := s.deps.TorBox.MyListByID(it.TorBoxID)
	if err != nil {
		s.slowNote(it, "mylist: "+err.Error()) // transient: retried next tick
		return
	}
	if detail == nil {
		// Gone from the account. If it finished first the content is cached and goes
		// to the arr. Otherwise someone deleted it on the dashboard or TorBox removed it
		// (no progress for 2 days): fail it like a vanished wait download instead of
		// re-adding a transfer the owner or TorBox just dropped. Re-enqueueing a failed
		// item starts it again.
		cached, cerr := s.deps.TorBox.CheckCached([]string{it.Hash})
		if cerr != nil {
			s.slowNote(it, "checkcached: "+cerr.Error()) // transient: retried next tick
			return
		}
		it.TorBoxID = 0
		if item, ok := cached[it.Hash]; ok && len(item.Files) > 0 {
			it.State, it.CachedAt, it.Progress = catalog.SlowCached, s.now().Unix(), 1
			metrics.IncSlowLane("cached", "vanished")
			s.slowHandOff(it, &item)
			return
		}
		s.slowGiveUp(it, "removed_on_torbox", "download was removed from the TorBox account before it finished")
		return
	}
	if detail.DownloadFinished || detail.DownloadState == "cached" ||
		detail.DownloadState == "completed" || detail.DownloadState == "uploading" {
		// Done: release the account copy exactly like a finished wait download (the
		// content now lives in TorBox's cache). A failed delete keeps the item
		// downloading so the next tick retries — never leave the copy behind silently.
		if err := s.deps.TorBox.ControlDelete(it.TorBoxID); err != nil {
			s.slowNote(it, "release account copy: "+err.Error())
			return
		}
		it.State, it.TorBoxID, it.CachedAt, it.Progress, it.ETA, it.Speed =
			catalog.SlowCached, 0, s.now().Unix(), 1, 0, 0
		metrics.IncSlowLane("cached", "downloaded")
		slog.Info("slow lane: download finished, account copy released", "hash", it.Hash, "name", it.Name)
		s.slowHandOff(it, nil)
		return
	}
	it.Progress, it.ETA, it.Speed, it.Seeds, it.Peers = detail.Progress, detail.ETA, detail.DownloadSpeed, detail.Seeds, detail.Peers
	if it.Name == "" {
		it.Name = detail.Name
	}
	s.slowSave(it)
}

// slowHandOff gives a cached item back to the arr. item is the checkcached result when
// the caller already has it. Order of preference:
//
//  1. the arr's own grab is still in the catalog in error state (wait_fail_blocklist off,
//     or the arr has not removed it yet): revive it in place — the arr sees its download
//     complete and imports it normally;
//  2. the hash is already a live release (someone grabbed it meanwhile): nothing to do;
//  3. the category's arr is configured and its history has a grab of this hash (always
//     the case for an adopted wait download; possible for a manual add the arr once gave
//     up on): Sonarr/Radarr never import a download id whose latest history event is
//     "failed", so clear the blocklist entry and ask the arr to search those episodes /
//     that movie again (arr.Regrab); its new grab of the now-cached release completes
//     instantly;
//  4. otherwise (the arr never grabbed this hash, or no arr is configured): add it to the
//     category as a new completed download. The arr imports what it can parse from the
//     folder name; with slow_lane.auto_import Lazarr later runs the arr's manual import
//     for what it could not (multi-season packs, foreign folder names; slowAutoImports).
func (s *server) slowHandOff(it *catalog.SlowLaneItem, item *torbox.CachedItem) {
	if item == nil {
		cached, err := s.deps.TorBox.CheckCached([]string{it.Hash})
		if err != nil {
			s.slowNote(it, "checkcached: "+err.Error())
			return
		}
		if c, ok := cached[it.Hash]; ok && len(c.Files) > 0 {
			item = &c
		} else {
			if s.now().Sub(time.Unix(it.CachedAt, 0)) < slowCacheLag {
				s.slowNote(it, "finished, waiting for TorBox's cache index")
				return
			}
			s.slowGiveUp(it, "not_cached", "TorBox finished the download but never reported it cached")
			return
		}
	}
	it.Size = item.Size
	if reason, file := gateRefusal(s.deps.Config.Policy, item.Files); reason != "" {
		metrics.IncAddRejected("content_slow_lane")
		slog.Warn("slow lane: cached content fails the content gate", "hash", it.Hash, "name", it.Name,
			"reason", reason, "file", file)
		s.slowGiveUp(it, "content", "content gate: "+reason)
		return
	}

	rel, _, err := s.deps.Store.GetRelease(it.Hash)
	if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		s.slowNote(it, "catalog: "+err.Error())
		return
	}
	switch {
	case rel != nil && rel.DetachedAt == 0 && rel.State == catalog.StateError:
		if err := s.slowMaterializeRelease(rel, it, item); err != nil {
			_ = s.slowError(it, "handoff_error", err)
			return
		}
		s.slowHanded(it, "revived", "the arr's own download completed in place")
		return
	case rel != nil && rel.DetachedAt == 0:
		s.slowHanded(it, "existing", "already a live release (grabbed again meanwhile)")
		return
	}

	if a := s.deps.SlowLaneArrs[it.Category]; a != nil {
		ctx, cancel := context.WithTimeout(s.slowContext(), slowArrTimeout)
		res, err := a.Regrab(ctx, it.Hash)
		cancel()
		switch {
		case err == nil:
			s.slowHanded(it, "regrab", fmt.Sprintf("asked the arr to search again (%d item(s), %d blocklist entr%s cleared)",
				len(res.Searched), res.BlocklistCleared, map[bool]string{true: "y", false: "ies"}[res.BlocklistCleared == 1]))
			return
		case errors.Is(err, arr.ErrNoGrab):
			// No grab to give back to: fall through to a plain completed download.
		default:
			if it.Attempts+1 < slowMaxAttempts {
				_ = s.slowError(it, "handoff_error", fmt.Errorf("arr re-search: %w", err))
				return
			}
			slog.Warn("slow lane: arr re-search keeps failing, adding as a completed download instead",
				"hash", it.Hash, "category", it.Category, "err", err)
		}
	}

	now := s.now().Unix()
	nrel := &catalog.Release{Hash: it.Hash, Name: it.Name, Category: it.Category, Magnet: it.Magnet,
		State: catalog.StateError, AddedOn: now, CreatedAt: now}
	if err := s.slowMaterializeRelease(nrel, it, item); err != nil {
		_ = s.slowError(it, "handoff_error", err)
		return
	}
	s.slowHanded(it, "added", "added to "+it.Category+" as a completed download")
}

// slowMaterializeRelease turns rel into a normal cached, lazy (virtual) release with
// symlinks under download_dir/<category> — the exact shape a cached grab has, so the
// arr sees a completed download and Lazarr streams it on first read.
func (s *server) slowMaterializeRelease(rel *catalog.Release, it *catalog.SlowLaneItem, item *torbox.CachedItem) error {
	rel.Name, rel.TorBoxName = s.releaseName(it.Name, item.Name, it.Hash), item.Name
	rel.TotalSize = item.Size
	rel.Cached = true
	rel.State = catalog.StateVirtual
	rel.TorBoxID = 0
	rel.MaterializedAt = 0
	rel.DetachedAt = 0
	if rel.Magnet == "" {
		rel.Magnet = it.Magnet
	}
	files := toCatalogFiles(it.Hash, item.Files)
	if err := s.deps.Store.UpsertRelease(rel, files); err != nil {
		return fmt.Errorf("store release: %w", err)
	}
	if err := s.deps.Symlink.Create(rel, files); err != nil {
		slog.Warn("slow lane: symlink", "hash", it.Hash, "err", err) // the arr still sees it complete
	}
	if s.deps.GrabHook != nil {
		s.deps.GrabHook(rel, files)
	}
	if it.Name == "" {
		it.Name = rel.Name
	}
	return nil
}

// slowCheckImports marks recent hand-offs imported once a library symlink points at
// them (needs the library-link finder; bounded per tick).
func (s *server) slowCheckImports(now time.Time) {
	if s.deps.Links == nil {
		return
	}
	items, err := s.deps.SlowLane.ListSlowLane(catalog.SlowHandedOff)
	if err != nil {
		return
	}
	checked := 0
	for i := len(items) - 1; i >= 0 && checked < slowImportChecksPerTick; i-- {
		it := items[i]
		if now.Sub(time.Unix(it.FinishedAt, 0)) > slowImportWindow {
			continue
		}
		checked++
		ctx, cancel := context.WithTimeout(s.slowContext(), slowLinkTimeout)
		links, lerr := s.deps.Links.LinksTo(ctx, it.Hash)
		cancel()
		if lerr != nil || len(links) == 0 {
			continue
		}
		unlock := s.locks.lock(it.Hash)
		if cur, gerr := s.deps.SlowLane.GetSlowLane(it.Hash); gerr == nil && cur.State == catalog.SlowHandedOff {
			cur.State, cur.UpdatedAt = catalog.SlowImported, now.Unix()
			cur.Note = fmt.Sprintf("%s; imported (%d library link(s))", cur.Note, len(links))
			s.slowSave(cur)
			metrics.IncSlowLane("imported", "")
			slog.Info("slow lane: imported by the arr", "hash", it.Hash, "name", cur.Name, "links", len(links))
		}
		unlock()
	}
}

// ── automatic manual import ─────────────────────────────────────────────────

// slowAutoImports finishes "added" hand-offs with the arr's manual import
// (slow_lane.auto_import). Such a download is one the arr never grabbed, so its
// Completed Download Handling only has the folder name to go on and leaves behind
// whatever that name does not describe: the other seasons of a multi-season pack, every
// file under a foreign-language folder name, 5x01-style folders. The arr's manual import
// parses each file on its own; arr.Client.ImportMissing imports only unambiguous,
// unrejected files into monitored episodes/movies that have no file yet (never a
// replacement), so a re-run is idempotent.
//
// Only "added" hand-offs qualify, whatever their origin: "revived" and "regrab" go
// through the arr's own grab, which knows its episodes/movie, and "existing" belongs to
// someone else's grab. The arr gets auto_import_after to import first (also the gap
// between attempts); at most slowAutoImportMaxAttempts attempts per item, at most
// slowAutoImportsPerTick items per tick, newest first.
func (s *server) slowAutoImports(now time.Time) {
	if !s.deps.Config.SlowLane.AutoImport || len(s.deps.SlowLaneArrs) == 0 {
		return
	}
	items, err := s.deps.SlowLane.ListSlowLane(catalog.SlowHandedOff, catalog.SlowImported)
	if err != nil {
		return
	}
	n := 0
	for i := len(items) - 1; i >= 0 && n < slowAutoImportsPerTick; i-- {
		it := items[i]
		if !s.autoImportDue(it, now) {
			continue
		}
		imp, ok := s.deps.SlowLaneArrs[it.Category].(SlowLaneImporter)
		if !ok {
			continue
		}
		n++
		s.slowAutoImport(it.Hash, imp, now)
	}
}

// autoImportDue reports whether the item should get an automatic manual import attempt now.
func (s *server) autoImportDue(it *catalog.SlowLaneItem, now time.Time) bool {
	if it.HandOff != "added" || it.AutoImportDone || it.AutoImportAttempts >= slowAutoImportMaxAttempts || it.FinishedAt == 0 {
		return false
	}
	if it.State != catalog.SlowHandedOff && it.State != catalog.SlowImported {
		return false
	}
	after := s.deps.Config.SlowLane.AutoImportAfter.D()
	since := now.Sub(time.Unix(it.FinishedAt, 0))
	if since < after || since > slowImportWindow {
		return false
	}
	return it.AutoImportAt == 0 || now.Sub(time.Unix(it.AutoImportAt, 0)) >= after
}

// slowAutoImport runs one attempt for one item. The arr calls run outside the hash lock
// (they can take minutes); the outcome is recorded only if the item is still the same.
func (s *server) slowAutoImport(hash string, imp SlowLaneImporter, now time.Time) {
	unlock := s.locks.lock(hash)
	cur, err := s.deps.SlowLane.GetSlowLane(hash)
	if err != nil || !s.autoImportDue(cur, now) {
		unlock()
		return
	}
	rel, files, rerr := s.deps.Store.GetRelease(hash)
	folder := ""
	if rerr == nil && visible(rel) && len(files) > 0 {
		folder = s.contentPath(rel, files) // exactly what torrents/info reports to the arr
	}
	unlock()
	if folder == "" {
		if rerr != nil && !errors.Is(rerr, catalog.ErrNotFound) {
			slog.Warn("slow lane: auto import: catalog", "hash", hash, "err", rerr)
			return // transient: next tick
		}
		s.autoImportRecord(cur, now, "gone", true,
			"auto manual import: the download is no longer in "+cur.Category+" (the arr removed it); nothing to import")
		return
	}

	var t arr.ImportTarget
	if cur.Target != nil {
		t = arr.ImportTarget{SeriesID: cur.Target.SeriesID, Season: cur.Target.Season, MovieID: cur.Target.MovieID}
	}
	ctx, cancel := context.WithTimeout(s.slowContext(), slowAutoImportTimeout)
	res, err := imp.ImportMissing(ctx, folder, hash, t)
	cancel()
	outcome, final, note := autoImportOutcome(res, err, cur.Category)
	s.autoImportRecord(cur, now, outcome, final, note)
	attrs := []any{"hash", hash, "name", cur.Name, "category", cur.Category, "outcome", outcome,
		"candidates", res.Candidates, "selected", res.Selected, "imported", res.Imported,
		"skipped", res.SkipSummary(), "command", res.Command, "folder", folder}
	if err != nil || outcome == "partial" {
		slog.Warn("slow lane: auto manual import", append(attrs, "err", err)...)
	} else {
		slog.Info("slow lane: auto manual import", attrs...)
	}
}

// retriableSkips are skip reasons that may clear on a later attempt (the arr refreshed the
// series, a file finished probing); the others are final (the episode already has a file,
// the file is for another series/season, ...). A file the arr rejected only as "not an
// upgrade" / "already imported" is reported as "has file" by ImportMissing, so it is final.
var retriableSkips = []string{"rejected", "unmatched", "unknown episode", "no quality"}

// autoImportOutcome turns one ImportMissing result into a metric label, whether the item
// is done, and the note shown in the Web UI.
func autoImportOutcome(res arr.ImportResult, err error, category string) (outcome string, final bool, note string) {
	skipped := ""
	if sk := res.SkipSummary(); sk != "" {
		skipped = " (skipped: " + sk + ")"
	}
	retry := false
	for _, k := range retriableSkips {
		retry = retry || res.Skipped[k] > 0
	}
	switch {
	case err != nil:
		return "error", false, "auto manual import failed: " + err.Error()
	case res.Mismatch:
		return "mismatch", true, "auto manual import skipped: the target does not fit " + category + "'s arr (series vs movie)"
	case res.Ambiguous:
		return "ambiguous", true, "auto manual import skipped: the files map to more than one series/movie and the item has no target"
	case res.Candidates == 0:
		return "empty", false, "auto manual import: the arr listed no files in the download folder"
	case res.Selected == 0 && !retry:
		return "nothing", true, "auto manual import: nothing left to import" + skipped
	case res.Selected == 0:
		return "nothing", false, "auto manual import: nothing to import" + skipped
	case res.Imported < res.Selected:
		cmd := res.Command
		if res.Message != "" {
			cmd += ": " + res.Message
		}
		return "partial", false, fmt.Sprintf("auto manual import: %d of %d file(s) imported, command %s%s",
			res.Imported, res.Selected, cmd, skipped)
	default:
		return "imported", !retry, "auto manual import: " + res.Summary() + skipped
	}
}

// autoImportRecord stores one attempt on the item, unless the item changed meanwhile (a
// cancel, or a new add of the same hash replaced it).
func (s *server) autoImportRecord(snap *catalog.SlowLaneItem, now time.Time, outcome string, final bool, note string) {
	unlock := s.locks.lock(snap.Hash)
	defer unlock()
	cur, err := s.deps.SlowLane.GetSlowLane(snap.Hash)
	if err != nil || cur.QueuedAt != snap.QueuedAt || (cur.State != catalog.SlowHandedOff && cur.State != catalog.SlowImported) {
		return
	}
	cur.AutoImportAttempts++
	cur.AutoImportAt = now.Unix()
	cur.AutoImportDone = final || cur.AutoImportAttempts >= slowAutoImportMaxAttempts
	if !final && cur.AutoImportDone {
		note += fmt.Sprintf(" (gave up after %d attempts)", cur.AutoImportAttempts)
	}
	cur.AutoImportNote = note
	s.slowSave(cur)
	metrics.IncSlowLane("auto_import", outcome)
}

// ── small state helpers ──────────────────────────────────────────────────────

func (s *server) slowContext() context.Context {
	if s.slowCtx != nil {
		return s.slowCtx
	}
	return context.Background()
}

func (s *server) slowSave(it *catalog.SlowLaneItem) {
	it.UpdatedAt = s.now().Unix()
	if err := s.deps.SlowLane.UpsertSlowLane(it); err != nil {
		slog.Warn("slow lane: store", "hash", it.Hash, "err", err)
	}
}

// slowNote records a transient problem without counting it as an attempt.
func (s *server) slowNote(it *catalog.SlowLaneItem, msg string) {
	it.LastError = msg
	s.slowSave(it)
}

// slowError counts one failed attempt; after slowMaxAttempts the item fails.
func (s *server) slowError(it *catalog.SlowLaneItem, label string, err error) error {
	it.Attempts++
	if it.Attempts >= slowMaxAttempts {
		s.slowGiveUp(it, label, fmt.Sprintf("%v (after %d attempts)", err, it.Attempts))
		return err
	}
	it.LastError = err.Error()
	s.slowSave(it)
	slog.Warn("slow lane: attempt failed, will retry", "hash", it.Hash, "name", it.Name,
		"attempt", it.Attempts, "err", err)
	return err
}

// slowGiveUp fails the item, deleting a still-running TorBox download first. A failed
// delete keeps the item active so the next tick retries (nothing may leak).
func (s *server) slowGiveUp(it *catalog.SlowLaneItem, label, msg string) {
	if it.State == catalog.SlowDownloading && it.TorBoxID != 0 {
		if err := s.slowDeleteDownload(it); err != nil {
			s.slowNote(it, "give up: delete TorBox download: "+err.Error())
			return
		}
	}
	s.slowFinish(it, catalog.SlowFailed, msg, it.Note)
	metrics.IncSlowLane("failed", label)
	slog.Warn("slow lane: gave up", "hash", it.Hash, "name", it.Name, "reason", msg)
}

// slowDeleteDownload deletes the item's running TorBox download. TorBox answers a delete
// of an id that is no longer on the account with an error (HTTP 500), so a failed delete
// counts as done only when mylist confirms the id is gone. If the download is still
// there, or the check itself fails, the delete error is returned (nothing may leak).
func (s *server) slowDeleteDownload(it *catalog.SlowLaneItem) error {
	err := s.deps.TorBox.ControlDelete(it.TorBoxID)
	if err == nil {
		return nil
	}
	detail, lerr := s.deps.TorBox.MyListByID(it.TorBoxID)
	if lerr != nil || detail != nil {
		return err
	}
	slog.Info("slow lane: TorBox download already gone from the account", "hash", it.Hash,
		"name", it.Name, "torbox_id", it.TorBoxID, "delete_err", err)
	return nil
}

func (s *server) slowHanded(it *catalog.SlowLaneItem, how, note string) {
	if it.Note != "" && it.Origin == "wait" {
		note = it.Note + "; " + note
	}
	it.HandOff = how
	s.slowFinish(it, catalog.SlowHandedOff, "", note)
	metrics.IncSlowLane("handed_off", how)
	slog.Info("slow lane: handed off", "hash", it.Hash, "name", it.Name, "category", it.Category, "how", how)
}

func (s *server) slowFinish(it *catalog.SlowLaneItem, st catalog.SlowLaneState, lastErr, note string) {
	now := s.now().Unix()
	it.State, it.TorBoxID, it.FinishedAt, it.LastError, it.Note = st, 0, now, lastErr, note
	s.slowSave(it)
}

func (s *server) slowGauge() {
	items, err := s.deps.SlowLane.ListSlowLane()
	if err != nil {
		return
	}
	counts := map[string]int{}
	for _, it := range items {
		counts[string(it.State)]++
	}
	metrics.SetSlowLaneItems(counts)
}
