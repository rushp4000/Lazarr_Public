package webui

import (
	"context"
	"errors"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/localcache"
	"github.com/your-username/lazarr/internal/logging"
	"github.com/your-username/lazarr/internal/materialize"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/missing"
	"github.com/your-username/lazarr/internal/repair"
)

// Provider is the webui data contract. main wires concrete implementations via the
// adapter in cmd/lazarr/main.go. Tests pass a fake.
//
// Provider must never expose torbox.api_key. SafeConfig() must redact it.
type Provider interface {
	// Status returns a live snapshot for /api/status.
	Status() StatusSnapshot
	// ListReleases delegates to catalog.Store.ListReleases for the releases table.
	ListReleases(f catalog.ReleaseFilter) ([]*catalog.Release, int, error)
	// MaterializedSet returns a snapshot of the live in-memory materialized set.
	MaterializedSet() []MaterializedItem
	// MetricsSummary returns the current counter/gauge values for sparklines/charts.
	MetricsSummary() (*metrics.Summary, error)
	// ForceRelease calls engine.Release for a single hash. Used by the force-release
	// action in the UI; mutating, POST-only.
	ForceRelease(hash string) error
	// TriggerAudit runs the ToS audit immediately (engine.AuditTOS). Mutating, POST-only.
	TriggerAudit() error
	// TriggerRepairScan runs engine.RepairScan synchronously and returns the evicted set.
	TriggerRepairScan(ctx context.Context) ([]materialize.RepairEntry, error)
	// ListEvicted returns releases whose content is no longer available on TorBox's CDN.
	ListEvicted() ([]*catalog.Release, error)
	// ForgetRelease removes a release from the catalog and deletes its symlinks so the
	// arr's health-check will flag it missing and trigger a re-search.
	ForgetRelease(hash string) error
	// Orphans returns the last account orphan audit (TorBox torrents no release
	// references; v1.2.8).
	Orphans() materialize.OrphanReport
	// Dangling returns the last dangling library-link sweep (v1.2.8).
	Dangling() repair.DanglingReport
	// SafeConfig returns the effective config with api_key and passwords redacted.
	SafeConfig() SafeConfig

	// GetSettings returns the editable configuration for the settings form, with
	// TorBoxAPIKey and WebUIPassword blanked (the *Set flags report presence).
	GetSettings() Settings
	// SaveSettings validates and persists s to config.yaml. Empty secret fields keep
	// their current values. Log level is applied live; the return reports whether any
	// other change needs a process restart to take effect.
	SaveSettings(s Settings) (restartRequired bool, err error)
	// Logs returns up to limit recent records at or above level, oldest first.
	Logs(level string, limit int) []logging.Entry
	// Restart triggers a graceful shutdown shortly after returning, so the container
	// supervisor (Docker restart policy) brings Lazarr back up on the saved config.
	Restart() error

	// ---- Slow lane (v1.2.9) ----

	// SlowLane returns the slow-lane state and items (newest first) for the UI.
	SlowLane() (SlowLaneView, error)
	// SlowLaneAdd queues a magnet link / infohash for a category, optionally for a target
	// series/season or movie (nil = none). Mutating, POST-only.
	SlowLaneAdd(input, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error)
	// SlowLaneAddTorrent queues an uploaded .torrent file for a category, optionally for a
	// target (nil = none; private and v2-only torrents are refused). Mutating, POST-only.
	SlowLaneAddTorrent(torrent []byte, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error)
	// SlowLaneCancel cancels an active item (its TorBox download is deleted first).
	SlowLaneCancel(hash string) error

	// ---- Workstream G: local cache (docs/28) ----

	// CacheSnapshot returns the live cache view, or nil when the cache is disabled.
	CacheSnapshot() *localcache.Snapshot
	// CacheFiles lists cached files (newest pull first); nil when disabled.
	CacheFiles() []localcache.LocalFileView
	// CachePull enqueues every eligible file of a release; returns how many were queued.
	CachePull(hash string) (int, error)
	// CacheEvict evicts every local file of a release (links back to FUSE first).
	CacheEvict(hash string) error
	// CacheAnalyze queues post-pull analysis for every cached file of a release.
	CacheAnalyze(hash string) (int, error)
	// CacheAnalyzeBackfill queues analysis for every cached file not yet analyzed.
	CacheAnalyzeBackfill() (int, error)
	// CachePoll polls the media servers once, right now.
	CachePoll(ctx context.Context) error
}

// MissingProvider is the optional data contract of the Missing tab (what the linked
// arrs still want). It is separate from Provider so a provider without it still
// works: New detects it with a type assertion, and without it /api/missing reports
// {"enabled":false}.
type MissingProvider interface {
	// Missing returns the snapshot. refresh asks for a fresh one now (rate-limited:
	// a *missing.RateLimitedError comes back with the current snapshot). Returns
	// ErrMissingDisabled when the collector is off.
	Missing(ctx context.Context, refresh bool) (missing.Snapshot, error)
	// MissingSearch asks an arr to search for one row. Mutating, POST-only.
	MissingSearch(ctx context.Context, req missing.SearchRequest) (missing.SearchResult, error)
}

// ErrMissingDisabled is MissingProvider's answer while the collector is off
// (missing.enabled false, or no arr in playback.arrs has a URL and an API key).
var ErrMissingDisabled = errors.New("the missing list is off: it needs missing.enabled and at least one arr in playback.arrs with an API key")
