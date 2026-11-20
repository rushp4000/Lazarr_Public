// Package materialize is the lazy engine: add->requestdl->proxy->idle-release, with a
// configurable slot semaphore (default 3), link-refresh-on-4xx, idle + max-hold reapers,
// the probe-header cache, and the ToS-audit loop. Phase 2; built by Agent M (docs/09).
// Implements vfs.Materializer. See docs/05 §4 + docs/11 + docs/12. Use torbox-api skill.
package materialize

import (
	"context"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/torbox"
)

// Deps are the engine's dependencies (wired in main).
type Deps struct {
	Store  catalog.Store
	TorBox torbox.Client
	// Policy carries the materialization knobs: AllowUncached, IdleTTL, MaxHold,
	// ActiveSlots (0 = auto-detect the slot count from TorBox.UserMe()), and ProbeCache.
	Policy config.Policy
	// ProbeCacheDir is a bounded on-disk dir for cached file-header regions, so Plex
	// header scans of freshly imported items don't trigger a fresh TorBox add. Required
	// when Policy.ProbeCache is true.
	//
	// Reaper interval, link-refresh statuses, and the createtorrent budget are read from
	// internal/constants (not tunable here). Each read fetches exactly its window — there
	// is no readahead widening (it only wasted CDN bandwidth; see proxy.getRange).
	ProbeCacheDir string
	// ExtraCDNHosts are additional host suffixes for the stream proxy's CDN host-pin
	// (config torbox.extra_cdn_hosts) — the operator escape hatch for a TorBox CDN
	// domain Lazarr does not know yet. Built-ins always apply; see proxy.go.
	ExtraCDNHosts []string
	// DeleteOrphansOlderThan is config audit.delete_orphans_older_than: 0 = the orphan
	// audit only reports; > 0 deletes account orphans older than this.
	DeleteOrphansOlderThan time.Duration
	// HeldElsewhere, when set, returns TorBox ids another subsystem legitimately keeps on
	// the account (the slow lane's running downloads). The ToS audit counts them as held:
	// never a leak, never an orphan to delete. An error skips the audit run (unknown is
	// not "free to delete").
	HeldElsewhere func() ([]int64, error)
}

// AccountOrphan is a TorBox account torrent no catalog row references.
type AccountOrphan struct {
	TorBoxID  int64     `json:"torbox_id"`
	Hash      string    `json:"hash"`
	Name      string    `json:"name"`
	State     string    `json:"download_state"`
	CreatedAt time.Time `json:"created_at"`
	// InCatalog: the hash is a Lazarr release (released or errored) whose account copy
	// was left behind; false = something else added it (another tool, a human, or a
	// pre-1.2.8 wait-download leak whose row is gone).
	InCatalog bool `json:"in_catalog"`
	Deleted   bool `json:"deleted"`
}

// OrphanReport is the last orphan audit.
type OrphanReport struct {
	At      int64           `json:"at"` // unix; 0 = no audit yet
	Items   []AccountOrphan `json:"items"`
	Deletes bool            `json:"deletes_enabled"`
}

// RepairEntry describes a single release whose content is no longer available on TorBox's
// CDN. JSON tags are the Web UI wire format (/api/repair/scan), snake_case like the rest.
type RepairEntry struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	Category string `json:"category"`
	// State is the release's catalog state at scan time (materialized/downloading
	// content is on the account and still readable even when checkcached misses).
	State catalog.State `json:"state"`
	// PrevEvictedAt is when an EARLIER scan last marked this hash evicted (unix), or
	// 0 when this scan is the first to see it gone. Two sightings some hours apart
	// confirm the eviction before anything acts on it.
	PrevEvictedAt int64 `json:"prev_evicted_at,omitempty"`
}

// Engine is the concrete materializer (built by Agent M). It must satisfy
// vfs.Materializer: ReadAt(hash, fileID, p, off) and Release(hash).
type Engine interface {
	ReadAt(hash string, fileID int, p []byte, off int64) (int, error)
	Release(hash string) error
	// AuditTOS diffs TorBox mylist against our materialized set; logs/alarms leaks.
	// Scoped to Lazarr-added ids while the account is shared with another tool (docs/12).
	AuditTOS() error
	// RepairScan batch-checks every catalogued hash against TorBox's checkcached endpoint
	// (no TorBox adds). Marks each release's CacheStatus in the catalog, then returns
	// the evicted set. Runs daily via the repair ticker in main.
	RepairScan(ctx context.Context) ([]RepairEntry, error)
	Close() error
}
