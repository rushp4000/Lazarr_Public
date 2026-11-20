// Package metrics is Lazarr's observability surface: a set of Prometheus
// collectors wired at the materialize/qbit call sites, plus the HTTP handlers
// for the opt-in admin server (/metrics and /health).
//
// The collectors live on a private registry so importing this package has no
// effect on any global state and tests get a clean slate via a fresh registry.
// Helper funcs (IncGrabs, SetSlotsInUse, …) are cheap and goroutine-safe; the
// engine and qbit layers call them directly without holding a metrics handle.
package metrics

import (
	"encoding/json"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// reg is the dedicated registry. We do not use the default registry so Lazarr's
// metrics are self-contained and a test can build an isolated handler.
var reg = prometheus.NewRegistry()

var (
	grabs = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_grabs_total",
		Help: "Total releases grabbed (symlinked at add, no TorBox add).",
	})
	materializes = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_materializes_total",
		Help: "Total lazy materializations (TorBox adds triggered by a read).",
	})
	releases = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_releases_total",
		Help: "Total releases (TorBox deletes by idle/max-hold reaper, LRU, or shutdown).",
	})
	linkRefresh = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_link_refresh_total",
		Help: "Total presigned-link refreshes after a stale-link 4xx.",
	})
	createRateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_createtorrent_ratelimited_total",
		Help: "Total createtorrent attempts rejected by the TorBox rate limit.",
	})
	probeHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_probe_cache_hits_total",
		Help: "Header-region reads served from the probe cache (no CDN fetch).",
	})
	probeMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_probe_cache_misses_total",
		Help: "Header-region reads that fell through the probe cache to the live proxy.",
	})
	materializedCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_materialized_count",
		Help: "Releases currently materialized (held on the TorBox account).",
	})
	slotsInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_slots_in_use",
		Help: "Active materialize slots in use.",
	})
	tosAuditLeaks = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_tos_audit_leaks",
		Help: "Leaked torrents found by the last ToS audit (account holds something believed released).",
	})
	reaperSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_reaper_skipped_total",
		Help: "Reaper sweeps skipped because the FUSE mount was unhealthy (reaping paused; alert if rising).",
	})
	cdnThrottled = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_cdn_throttled_total",
		Help: "HTTP 429 responses from the CDN. Each pauses readahead and triggers a patient foreground retry; sustained growth means the CDN is pacing this host.",
	})
	waitFailBlocklist = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_wait_fail_blocklist_total",
		Help: "policy.wait_fail_blocklist outcomes for failed downloads (wait, content gate, dead-cache), by result (removed|not_in_queue|gone|no_arr|error).",
	}, []string{"result"})

	torboxRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_torbox_requests_total",
		Help: "TorBox API requests by endpoint and HTTP status (or transport_error, or backoff = suppressed by the rate-limit governor with no request sent).",
	}, []string{"endpoint", "status"})
	torboxRemaining = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_torbox_ratelimit_remaining",
		Help: "x-ratelimit-remaining from the last TorBox API response (TorBox allows 300 per 60 s per key).",
	})
	torboxBackoffTrips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_torbox_backoff_trips_total",
		Help: "Times TorBox's own rate-limit signal (429, rate-limit detail, remaining 0) armed the global API backoff.",
	})
	requestDLNegative = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_requestdl_negative_hits_total",
		Help: "Reads answered from the requestdl negative cache (a recent failure for the same file) without calling TorBox.",
	})
	addRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_add_rejected_total",
		Help: "torrents/add refused, by reason: answered \"Fails.\" (the arr falls through to its next release) or, for content with policy.wait_fail_blocklist, accepted as a failed download the arr blocklists.",
	}, []string{"reason"})
	waitOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_wait_downloads_total",
		Help: "on_cache_miss=wait outcomes: started, completed, rejected_overflow, start_failed, failed{reason} (reason = max_wait|budget|eta|no_peers|vanished|not_cached|content).",
	}, []string{"outcome", "reason"})
	packJanitorRemoved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_pack_janitor_removed_total",
		Help: "Partially imported packs removed from an arr queue by policy.pack_janitor (no blocklist, no re-search).",
	})
	tosAuditOrphans = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_tos_audit_orphans",
		Help: "TorBox account torrents no Lazarr release references, at the last audit (after any opt-in deletes).",
	})
	danglingLinks = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_dangling_library_links",
		Help: "Library symlinks into Lazarr's FUSE/cache tree whose release is gone from the catalog (last sweep, after any unlinks).",
	})
	danglingHashes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_dangling_library_hashes",
		Help: "Distinct release hashes behind lazarr_dangling_library_links.",
	})
	emptyDirs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lazarr_empty_dirs",
		Help: "Empty folders found by the last cleanup sweep and not removed (report only, or removal failed), by area (download|library).",
	}, []string{"area"})
	emptyDirsRemoved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_empty_dirs_removed_total",
		Help: "Empty folders removed by cleanup.remove_empty_dirs, by area (download|library|repair).",
	}, []string{"area"})
	qbitDeletes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_qbit_deletes_total",
		Help: "torrents/delete outcomes per hash (purged|detached|wait_cancelled|wait_cancel_retry|swept|lookup_error).",
	}, []string{"outcome"})

	slowLaneItems = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lazarr_slowlane_items",
		Help: "Slow-lane items by state (queued|downloading|cached|handed_off|imported|failed|cancelled), at the last worker tick.",
	}, []string{"state"})
	slowLaneEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_slowlane_events_total",
		Help: "Slow-lane transitions: queued, adopted, started, start_deferred, cached, handed_off{how}, imported, failed{reason}, cancelled, auto_import{imported|partial|nothing|empty|ambiguous|mismatch|gone|error}.",
	}, []string{"event", "detail"})

	missingItems = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lazarr_missing_items",
		Help: "Missing tab: monitored, aired/available items without a file per arr (episodes for Sonarr, movies for Radarr), at its last good refresh.",
	}, []string{"category"})
	missingRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_missing_refreshes_total",
		Help: "Missing tab: per-arr refresh outcomes (ok|error).",
	}, []string{"category", "result"})
	missingSearches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_missing_searches_total",
		Help: "Missing tab: search commands sent to the arrs (SeasonSearch|EpisodeSearch|MoviesSearch) by result (ok|error).",
	}, []string{"command", "result"})
	replacementSearches = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_replacement_search_total",
		Help: "repair.search_replacement: episodes/movies by outcome (queued|searched|cooldown|rate_limited|dropped|unmonitored|has_file|gone|expired|error).",
	}, []string{"result"})

	// ---- Workstream G: local cache + playback signal ----
	localBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_localcache_bytes",
		Help: "Bytes held on local disk by the cache (complete files + in-progress frontiers).",
	})
	localFiles = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lazarr_localcache_files",
		Help: "Local-cache files by state.",
	}, []string{"state"})
	localDiskFree = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_localcache_disk_free_bytes",
		Help: "Free bytes on the cache volume at the last reaper sweep.",
	})
	localPulls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_localcache_pulls_total",
		Help: "Completed pulls into the local cache, by reason (watch|prefetch|grab|manual).",
	}, []string{"reason"})
	localPullFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_localcache_pull_failures_total",
		Help: "Pulls that ended in an error (retried by a later pass).",
	})
	localEvictions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_localcache_evictions_total",
		Help: "Local-cache evictions by policy class.",
	}, []string{"class"})
	localSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_localcache_skipped_total",
		Help: "Pull requests refused before any I/O, by reason (source|uncached|evicted|size|budget|disk|disabled).",
	}, []string{"reason"})
	localRetargets = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_localcache_retargets_total",
		Help: "Library symlink retargets, by direction (cache|fuse).",
	}, []string{"to"})
	localDanglingRepaired = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "lazarr_localcache_dangling_repaired_total",
		Help: "Library symlinks that pointed at a missing cache file and were put back on FUSE.",
	})
	analysisTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_analysis_total",
		Help: "Post-pull media-server analyses by server and result (done|unavailable|timeout|skipped|failed).",
	}, []string{"server", "result"})
	analysisActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_analysis_active",
		Help: "Cached files whose media-server analysis is pending (held from TTL eviction).",
	})
	playbackSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "lazarr_playback_sessions",
		Help: "Live playback sessions seen at the last poll (all servers).",
	})
	playbackPollErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lazarr_playback_poll_errors_total",
		Help: "Failed session polls, by server.",
	}, []string{"server"})
)

func init() {
	reg.MustRegister(
		grabs, materializes, releases, linkRefresh, createRateLimited,
		probeHits, probeMisses, materializedCount, slotsInUse, tosAuditLeaks,
		reaperSkipped, cdnThrottled, waitFailBlocklist, qbitDeletes,
		torboxRequests, torboxRemaining, torboxBackoffTrips, requestDLNegative, addRejected,
		waitOutcomes, packJanitorRemoved, tosAuditOrphans, danglingLinks, danglingHashes, emptyDirs, emptyDirsRemoved,
		slowLaneItems, slowLaneEvents, missingItems, missingRefreshes, missingSearches, replacementSearches,
		localBytes, localFiles, localDiskFree, localPulls, localPullFailures,
		localEvictions, localSkipped, localRetargets, localDanglingRepaired, analysisTotal, analysisActive, playbackSessions, playbackPollErrors,
	)
}

// Counter/gauge mutators — called at the wiring sites. All are goroutine-safe
// (prometheus collectors are). Kept as plain funcs so call sites stay terse.

func IncGrabs()             { grabs.Inc() }
func IncMaterializes()      { materializes.Inc() }
func IncReleases()          { releases.Inc() }
func IncLinkRefresh()       { linkRefresh.Inc() }
func IncCreateRateLimited() { createRateLimited.Inc() }
func IncProbeHit()          { probeHits.Inc() }
func IncProbeMiss()         { probeMisses.Inc() }
func IncReaperSkipped()     { reaperSkipped.Inc() }
func IncCDNThrottled()      { cdnThrottled.Inc() }

// IncTorBoxRequest counts one TorBox API request outcome.
func IncTorBoxRequest(endpoint, status string) {
	torboxRequests.WithLabelValues(endpoint, status).Inc()
}

// SetTorBoxRateLimitRemaining records x-ratelimit-remaining from the last response.
func SetTorBoxRateLimitRemaining(n int) { torboxRemaining.Set(float64(n)) }

// IncTorBoxBackoffTrip counts one global API backoff armed by TorBox's signal.
func IncTorBoxBackoffTrip() { torboxBackoffTrips.Inc() }

// IncRequestDLNegativeHit counts one read short-circuited by the requestdl negative cache.
func IncRequestDLNegativeHit() { requestDLNegative.Inc() }

// IncAddRejected counts one refused torrents/add by reason.
func IncAddRejected(reason string) { addRejected.WithLabelValues(reason).Inc() }

// IncWait counts one wait-download outcome (reason "" except for failures).
func IncWait(outcome, reason string) { waitOutcomes.WithLabelValues(outcome, reason).Inc() }

// IncPackJanitorRemoved counts one pack janitor removal.
func IncPackJanitorRemoved() { packJanitorRemoved.Inc() }

// SetTosAuditOrphans records the orphan count of the last audit.
func SetTosAuditOrphans(n int) { tosAuditOrphans.Set(float64(n)) }

// SetDanglingLinks records the last dangling-link sweep.
func SetDanglingLinks(links, hashes int) {
	danglingLinks.Set(float64(links))
	danglingHashes.Set(float64(hashes))
}

// SetEmptyDirs records the empty folders one cleanup sweep left in an area.
func SetEmptyDirs(area string, n int) { emptyDirs.WithLabelValues(area).Set(float64(n)) }

// AddEmptyDirsRemoved counts folders removed by the empty-folder cleanup.
func AddEmptyDirsRemoved(area string, n int) { emptyDirsRemoved.WithLabelValues(area).Add(float64(n)) }

// IncQbitDelete counts one torrents/delete outcome (see the Help text).
func IncQbitDelete(outcome string) { qbitDeletes.WithLabelValues(outcome).Inc() }

// IncWaitFailBlocklist counts one wait_fail_blocklist outcome (see the Help text).
func IncWaitFailBlocklist(result string) { waitFailBlocklist.WithLabelValues(result).Inc() }

// SetSlowLaneItems sets the per-state slow-lane gauge (states absent from counts go to 0).
func SetSlowLaneItems(counts map[string]int) {
	for _, st := range []string{"queued", "downloading", "cached", "handed_off", "imported", "failed", "cancelled"} {
		slowLaneItems.WithLabelValues(st).Set(float64(counts[st]))
	}
}

// IncSlowLane counts one slow-lane transition. detail is a bounded label ("" if none).
func IncSlowLane(event, detail string) { slowLaneEvents.WithLabelValues(event, detail).Inc() }

// SetMissingItems sets one arr's missing-item count (Missing tab).
func SetMissingItems(category string, n int) { missingItems.WithLabelValues(category).Set(float64(n)) }

// IncMissingRefresh counts one per-arr Missing-tab refresh (result ok|error).
func IncMissingRefresh(category, result string) {
	missingRefreshes.WithLabelValues(category, result).Inc()
}

// IncMissingSearch counts one Missing-tab search command (result ok|error).
func IncMissingSearch(command, result string) { missingSearches.WithLabelValues(command, result).Inc() }

// AddReplacementSearch counts n episodes/movies with one replacement-search outcome.
func AddReplacementSearch(result string, n int) {
	replacementSearches.WithLabelValues(result).Add(float64(n))
}

func SetMaterializedCount(n int) { materializedCount.Set(float64(n)) }
func SetSlotsInUse(n int)        { slotsInUse.Set(float64(n)) }
func SetTosAuditLeaks(n int)     { tosAuditLeaks.Set(float64(n)) }

// Workstream G mutators.
func SetLocalBytes(n int64)              { localBytes.Set(float64(n)) }
func SetLocalFiles(state string, n int)  { localFiles.WithLabelValues(state).Set(float64(n)) }
func SetLocalDiskFree(n uint64)          { localDiskFree.Set(float64(n)) }
func IncLocalPull(reason string)         { localPulls.WithLabelValues(reason).Inc() }
func IncLocalPullFailure()               { localPullFailures.Inc() }
func IncLocalEviction(class string)      { localEvictions.WithLabelValues(class).Inc() }
func IncLocalSkipped(reason string)      { localSkipped.WithLabelValues(reason).Inc() }
func IncLocalRetarget(to string)         { localRetargets.WithLabelValues(to).Inc() }
func IncLocalDanglingRepaired()          { localDanglingRepaired.Inc() }
func IncAnalysis(server, result string)  { analysisTotal.WithLabelValues(server, result).Inc() }
func SetAnalysisActive(n int)            { analysisActive.Set(float64(n)) }
func SetPlaybackSessions(n int)          { playbackSessions.Set(float64(n)) }
func IncPlaybackPollError(server string) { playbackPollErrors.WithLabelValues(server).Inc() }

// Summary is a snapshot of the current counter/gauge values for the Web UI.
// All counter fields are cumulative totals since process start.
type Summary struct {
	GrabsTotal             float64 `json:"grabs_total"`
	MaterializesTotal      float64 `json:"materializes_total"`
	ReleasesTotal          float64 `json:"releases_total"`
	LinkRefreshTotal       float64 `json:"link_refresh_total"`
	CreateRateLimitedTotal float64 `json:"create_ratelimited_total"`
	ProbeHitsTotal         float64 `json:"probe_hits_total"`
	ProbeMissesTotal       float64 `json:"probe_misses_total"`
	MaterializedCount      float64 `json:"materialized_count"`
	SlotsInUse             float64 `json:"slots_in_use"`
	TosAuditLeaks          float64 `json:"tos_audit_leaks"`
	ReaperSkippedTotal     float64 `json:"reaper_skipped_total"`
	LocalBytes             float64 `json:"local_bytes"`
	LocalPullsTotal        float64 `json:"local_pulls_total"`
	LocalEvictionsTotal    float64 `json:"local_evictions_total"`
	PlaybackSessions       float64 `json:"playback_sessions"`
}

// GatherSummary reads the current counter/gauge values from the Prometheus registry.
// It is cheap (gather + scan) and safe for concurrent use.
func GatherSummary() (*Summary, error) {
	families, err := reg.Gather()
	if err != nil {
		return nil, err
	}
	s := &Summary{}
	for _, f := range families {
		val := firstMetricValue(f)
		switch f.GetName() {
		case "lazarr_grabs_total":
			s.GrabsTotal = val
		case "lazarr_materializes_total":
			s.MaterializesTotal = val
		case "lazarr_releases_total":
			s.ReleasesTotal = val
		case "lazarr_link_refresh_total":
			s.LinkRefreshTotal = val
		case "lazarr_createtorrent_ratelimited_total":
			s.CreateRateLimitedTotal = val
		case "lazarr_probe_cache_hits_total":
			s.ProbeHitsTotal = val
		case "lazarr_probe_cache_misses_total":
			s.ProbeMissesTotal = val
		case "lazarr_materialized_count":
			s.MaterializedCount = val
		case "lazarr_slots_in_use":
			s.SlotsInUse = val
		case "lazarr_tos_audit_leaks":
			s.TosAuditLeaks = val
		case "lazarr_reaper_skipped_total":
			s.ReaperSkippedTotal = val
		case "lazarr_localcache_bytes":
			s.LocalBytes = val
		case "lazarr_localcache_pulls_total":
			s.LocalPullsTotal = sumMetricValues(f)
		case "lazarr_localcache_evictions_total":
			s.LocalEvictionsTotal = sumMetricValues(f)
		case "lazarr_playback_sessions":
			s.PlaybackSessions = val
		}
	}
	return s, nil
}

// sumMetricValues adds every label combination of a family (for *Vec counters).
func sumMetricValues(f *dto.MetricFamily) float64 {
	var total float64
	for _, m := range f.GetMetric() {
		if c := m.GetCounter(); c != nil {
			total += c.GetValue()
		} else if g := m.GetGauge(); g != nil {
			total += g.GetValue()
		}
	}
	return total
}

// firstMetricValue extracts the scalar value from the first metric in a family,
// handling Counter, Gauge, and Untyped kinds. Returns 0 for unknown kinds.
func firstMetricValue(f *dto.MetricFamily) float64 {
	if f == nil {
		return 0
	}
	ms := f.GetMetric()
	if len(ms) == 0 {
		return 0
	}
	m := ms[0]
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	if u := m.GetUntyped(); u != nil {
		return u.GetValue()
	}
	return 0
}

// MetricsHandler serves the Prometheus exposition of Lazarr's registry.
func MetricsHandler() http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// HealthProvider supplies the live values reported by /health. main wires the
// concrete materialize engine + vfs mount in; tests pass a fake.
type HealthProvider interface {
	Mounted() bool
	SlotsInUse() int
	SlotsTotal() int
	LastAuditUnix() int64
	Version() string
}

// health is the JSON body returned by /health.
type health struct {
	Mounted       bool   `json:"mounted"`
	SlotsInUse    int    `json:"slots_in_use"`
	SlotsTotal    int    `json:"slots_total"`
	LastAuditUnix int64  `json:"last_audit_unix"`
	Version       string `json:"version"`
}

// HealthHandler returns a handler that reports liveness + a snapshot of engine
// state as JSON. It always returns 200 (it is a liveness probe, not a readiness
// gate); the `mounted` field carries the FUSE health signal for scrapers.
func HealthHandler(p HealthProvider) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := health{
			Mounted:       p.Mounted(),
			SlotsInUse:    p.SlotsInUse(),
			SlotsTotal:    p.SlotsTotal(),
			LastAuditUnix: p.LastAuditUnix(),
			Version:       p.Version(),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}
