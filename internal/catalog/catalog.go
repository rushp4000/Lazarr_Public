// Package catalog is the SQLite-backed store of releases, files, and link cache.
// Schema mirrors docs/05-spec.md §Data model. The Store interface is the foundation
// contract; the SQLite implementation is built by Agent C (see docs/09).
package catalog

// State is a release's materialization state.
type State string

const (
	StateVirtual      State = "virtual"      // symlinked, NOT on TorBox
	StateMaterialized State = "materialized" // added to TorBox, streamable
	StateError        State = "error"        // checkcached/torrentinfo failed, or dead-cache
	// StateDownloading: on_cache_miss=wait — TorBox is fetching the uncached torrent;
	// the qbit wait-poller watches progress/ETA and flips it to virtual (released,
	// now cached) or error (over budget / stalled). The item IS on the account.
	StateDownloading State = "downloading"
)

// CacheStatus reports whether the content is still available on TorBox's CDN.
// Set by the repair scanner (engine.RepairScan). "evicted" means checkcached returned
// false — the content is gone and playback would fail with ErrPurged.
type CacheStatus string

const (
	CacheStatusUnknown CacheStatus = ""        // not yet checked
	CacheStatusCached  CacheStatus = "cached"  // confirmed available
	CacheStatusEvicted CacheStatus = "evicted" // no longer on TorBox CDN
)

// Source distinguishes which TorBox namespace (and which arr-facing emulation)
// added a release. Torrent ids and usenet ids are separate id spaces on TorBox,
// so every code path that calls back into the TorBox client (materialize,
// audit/reconcile, repair) must dispatch on this. vfs/symlink/proxy/reapers are
// source-agnostic and never need to look at it.
//
// Declared identically to the feat/nzb-usenet-foundation branch (b6bddcf) so the
// two workstreams merge without conflict. The local cache (Workstream G) hard-
// excludes anything that is not SourceTorrent.
type Source string

const (
	SourceTorrent Source = "torrent" // via qbit emulation; Hash = 40-hex BT infohash
	SourceUsenet  Source = "usenet"  // via sabnzbd emulation; Hash = 32-hex md5 of the NZB
)

// Release is one grabbed item (one torrent), keyed by infohash.
//
// JSON tags are the Web UI wire format (/api/releases, /api/repair) — snake_case,
// matched by the dashboard JS. Magnet is excluded: it is internal plumbing and can
// be very large (full trackers list) for zero UI value.
type Release struct {
	Hash       string `json:"hash"` // infohash
	Name       string `json:"name"`
	Category   string `json:"category"` // = arr name
	Magnet     string `json:"-"`        // original magnet/URL (to add at materialize time)
	TotalSize  int64  `json:"total_size"`
	State      State  `json:"state"`
	Source     Source `json:"source"`      // "torrent" (default) or "usenet"
	Cached     bool   `json:"cached"`      // checkcached hit at grab time?
	TorBoxID   int64  `json:"torbox_id"`   // set only while materialized
	AddedOn    int64  `json:"added_on"`    // unix; when added to the catalog (grab time)
	LastAccess int64  `json:"last_access"` // unix; drives the idle reaper
	// MaterializedAt is the unix time the release last entered StateMaterialized
	// (0 when not materialized). The max-hold reaper measures the hold window from
	// THIS, not AddedOn — a release grabbed long before its first playback must not
	// be an instant max-hold candidate the moment it materializes (add/delete churn).
	MaterializedAt int64       `json:"materialized_at"`
	CreatedAt      int64       `json:"created_at"`
	CacheStatus    CacheStatus `json:"cache_status"`     // set by repair scanner; "" = not yet checked
	LastCacheCheck int64       `json:"last_cache_check"` // unix; when CacheStatus was last set
	// DetachedAt is the unix time the arr deleted this release from the client while
	// imported library links still pointed at it (policy.detach_on_delete), or 0. A
	// detached release is invisible to the qBit API but stays readable through FUSE; a
	// re-grab of the same hash clears it (UpsertRelease writes 0).
	DetachedAt int64 `json:"detached_at"`
	// TorBoxName is the name TorBox's checkcached reported for the hash, kept for
	// diagnostics when Name is the arr's own release title (policy.use_arr_release_name).
	// TorBox can cache a season pack under one episode's file name (GoT S01, 2026-10).
	TorBoxName string `json:"torbox_name,omitempty"`

	// LocalFiles / LocalPulling are derived counts from the local_file table (Workstream
	// G): how many of this release's files are complete on local disk and how many are
	// mid-pull. Filled by ListReleases only (the Web UI table); zero elsewhere.
	LocalFiles   int `json:"local_files"`
	LocalPulling int `json:"local_pulling"`
}

// LocalState is the on-disk state of one file in the local cache (Workstream G).
type LocalState string

const (
	LocalPulling  LocalState = "pulling"  // .part file growing; Frontier bytes are durable
	LocalComplete LocalState = "complete" // full file on disk, renamed into place
	LocalFailed   LocalState = "failed"   // last pull/read errored; Error says why
)

// LocalFile is one file of a release held (or being pulled) on local disk. Keyed per
// FILE, not per release: a season pack is one infohash with many episodes and the
// cache decides per episode.
type LocalFile struct {
	Hash     string     `json:"hash"`
	FileID   int        `json:"file_id"`
	State    LocalState `json:"state"`
	Linked   bool       `json:"linked"`   // library symlinks currently point at the cache copy
	Size     int64      `json:"size"`     // catalog file size
	Frontier int64      `json:"frontier"` // bytes durably written so far (== Size when complete)
	Reason   string     `json:"reason"`   // watch | prefetch | grab | manual
	Class    string     `json:"class"`    // last policy classification (for the UI)
	PulledAt int64      `json:"pulled_at"`
	LastRead int64      `json:"last_read"` // unix; last local-serve read (FUSE prefix or index)
	Error    string     `json:"error"`
}

// Watch is one (server, user, file) playback observation — the per-user signal the
// retention policy keys off. Retention uses MAX(LastSeen) across users; "finished"
// requires every user who touched a file to have Completed it.
type Watch struct {
	Server      string  `json:"server"` // "plex" | "jellyfin"
	UserID      string  `json:"user_id"`
	UserName    string  `json:"user_name"`
	Hash        string  `json:"hash"`
	FileID      int     `json:"file_id"`
	SeriesKey   string  `json:"series_key"` // server-native series id ("" for movies)
	Season      int     `json:"season"`
	Episode     int     `json:"episode"`
	ProgressPct float64 `json:"progress_pct"`
	Completed   bool    `json:"completed"`
	FirstSeen   int64   `json:"first_seen"`
	LastSeen    int64   `json:"last_seen"`
}

// LibraryLink is a symlink the media servers index (the arr moved it there at
// import) that points into Lazarr's FUSE tree or the local-cache twin tree.
type LibraryLink struct {
	LinkPath     string `json:"link_path"`
	Hash         string `json:"hash"`
	FileID       int    `json:"file_id"`
	TargetKind   string `json:"target_kind"` // "fuse" | "cache"
	LastVerified int64  `json:"last_verified"`
}

// File is one file within a release.
type File struct {
	Hash    string // FK -> Release.Hash
	FileID  int    // TorBox file id within the torrent
	RelPath string // path within the virtual folder
	Size    int64
}

// DLLink caches a presigned CDN URL, refreshed on 4xx.
type DLLink struct {
	Hash      string
	FileID    int
	URL       string
	FetchedAt int64
	ExpiresAt int64
}

// ReleaseFilter is the query for ListReleases (Web UI table).
type ReleaseFilter struct {
	Q        string // substring match on name or hash (case-insensitive); empty = all
	State    State  // empty = all states
	Category string // empty = all categories
	// LocalOnly restricts to releases with at least one complete or pulling local file.
	LocalOnly bool
	Limit     int // 0 → default (50)
	Offset    int
}

// Store is the catalog contract. All timestamps are unix seconds.
type Store interface {
	UpsertRelease(r *Release, files []File) error
	GetRelease(hash string) (*Release, []File, error)
	ListByCategory(category string) ([]*Release, error)
	// ListReleases returns releases matching f with a total count (for pagination).
	ListReleases(f ReleaseFilter) ([]*Release, int, error)
	SetState(hash string, st State, torboxID int64) error
	TouchAccess(hash string, ts int64) error
	// IdleCandidates returns materialized releases whose LastAccess is before ts.
	IdleCandidates(before int64) ([]*Release, error)
	// OverMaxHold returns materialized releases whose MaterializedAt is before ts
	// (hard ceiling, measured from materialize time — not grab time).
	OverMaxHold(before int64) ([]*Release, error)
	// MaterializedIDs returns the TorBox ids Lazarr believes are added (ToS audit).
	MaterializedIDs() ([]int64, error)
	// MaterializedReleases returns all releases currently in StateMaterialized. Drives the
	// boot-time reconciliation sweep that releases crash/restart leftovers (B2).
	MaterializedReleases() ([]*Release, error)
	// DownloadingReleases returns releases in StateDownloading (on_cache_miss=wait).
	// The qbit wait-poller resumes these after a restart; the ToS audit counts them
	// as legitimately held.
	DownloadingReleases() ([]*Release, error)
	// ListAllHashes returns every hash in the catalog. Used by the repair scanner to
	// batch-check availability with TorBox's checkcached endpoint (no TorBox adds).
	ListAllHashes() ([]string, error)
	// SetCacheStatus updates the cache_status and last_cache_check for a release.
	// Called by engine.RepairScan after each checkcached batch.
	SetCacheStatus(hash string, status CacheStatus, checkedAt int64) error
	// ListEvicted returns releases whose CacheStatus is CacheStatusEvicted, newest first.
	// These are items that are no longer available on TorBox's CDN.
	ListEvicted() ([]*Release, error)
	GetLink(hash string, fileID int) (*DLLink, error)
	SetLink(l *DLLink) error
	DeleteRelease(hash string) error
	// SetDetached stamps (at > 0) or clears (at == 0) Release.DetachedAt.
	SetDetached(hash string, at int64) error
	// DetachedReleases returns every release with DetachedAt != 0.
	DetachedReleases() ([]*Release, error)

	// ---- Workstream G: local cache + playback signal ----------------------------

	// UpsertLocalFile inserts or replaces the local_file row for (Hash, FileID).
	UpsertLocalFile(lf *LocalFile) error
	// GetLocalFile returns the row or ErrNotFound.
	GetLocalFile(hash string, fileID int) (*LocalFile, error)
	// ListLocalFiles returns rows in the given state ("" = every state).
	ListLocalFiles(state LocalState) ([]*LocalFile, error)
	// ListLocalFilesByHash returns every local_file row of one release.
	ListLocalFilesByHash(hash string) ([]*LocalFile, error)
	// DeleteLocalFile removes the row (no-op when absent).
	DeleteLocalFile(hash string, fileID int) error
	// LocalBytes sums bytes on disk: Size for complete rows, Frontier for pulling ones.
	LocalBytes() (int64, error)

	// UpsertWatch inserts or replaces a watch row keyed by (Server, UserID, Hash, FileID).
	UpsertWatch(w *Watch) error
	// ListWatchByFile returns every user's watch row for one file.
	ListWatchByFile(hash string, fileID int) ([]*Watch, error)
	// ListWatchSince returns watch rows with LastSeen >= since (the active set).
	ListWatchSince(since int64) ([]*Watch, error)

	// UpsertLibraryLink inserts or replaces a library_link row keyed by LinkPath.
	UpsertLibraryLink(l *LibraryLink) error
	// ListLibraryLinks returns the links that reference one file.
	ListLibraryLinks(hash string, fileID int) ([]*LibraryLink, error)
	// ListAllLibraryLinks returns every known link.
	ListAllLibraryLinks() ([]*LibraryLink, error)
	// DeleteLibraryLink removes one link row (no-op when absent).
	DeleteLibraryLink(linkPath string) error
	// PruneLibraryLinks deletes rows whose LastVerified < before (links the index walk
	// no longer finds). Returns the number removed.
	PruneLibraryLinks(before int64) (int, error)

	Close() error
}

// AnalysisState is the lifecycle of one post-pull media-server analysis.
type AnalysisState string

const (
	AnalysisPending     AnalysisState = "pending"     // queued, not yet sent to the server
	AnalysisRunning     AnalysisState = "running"     // requested; the server is working
	AnalysisDone        AnalysisState = "done"        // server went idle; markers counted
	AnalysisUnavailable AnalysisState = "unavailable" // server never indexed the file within scan_wait
	AnalysisTimeout     AnalysisState = "timeout"     // gave up (timeout or max_hold)
	AnalysisSkipped     AnalysisState = "skipped"     // not local any more / not eligible
	AnalysisFailed      AnalysisState = "failed"      // request error
)

// Active reports whether the analysis still wants its file kept local.
func (s AnalysisState) Active() bool { return s == AnalysisPending || s == AnalysisRunning }

// LocalAnalysis is the post-pull analysis record of one cached file on one media
// server (docs/28 §16). ItemIDs is the comma-joined server item ids the file maps to
// (several for a multi-episode file).
type LocalAnalysis struct {
	Hash        string        `json:"hash"`
	FileID      int           `json:"file_id"`
	Server      string        `json:"server"`
	State       AnalysisState `json:"state"`
	ItemIDs     string        `json:"item_ids"`
	RequestedAt int64         `json:"requested_at"`
	StartedAt   int64         `json:"started_at"`
	FinishedAt  int64         `json:"finished_at"`
	Attempts    int           `json:"attempts"`
	Intro       int           `json:"intro"`
	Credits     int           `json:"credits"`
	Error       string        `json:"error"`
}

// AnalysisStore persists LocalAnalysis rows. It is separate from Store so the
// in-memory test fakes of Store need not grow; *sqliteStore implements both.
type AnalysisStore interface {
	UpsertLocalAnalysis(a *LocalAnalysis) error
	// ListLocalAnalysis returns every row of one file (one per server).
	ListLocalAnalysis(hash string, fileID int) ([]*LocalAnalysis, error)
	// ListActiveAnalysis returns pending/running rows (for resume and the eviction guard).
	ListActiveAnalysis() ([]*LocalAnalysis, error)
}

// SlowLaneState is the lifecycle of one slow-lane item (policy slow_lane, v1.2.9).
type SlowLaneState string

const (
	// SlowQueued: waiting for a free slow-lane slot; nothing is on the TorBox account.
	SlowQueued SlowLaneState = "queued"
	// SlowDownloading: TorBox is fetching it (TorBoxID is the account torrent). The ToS
	// audit counts these ids as legitimately held.
	SlowDownloading SlowLaneState = "downloading"
	// SlowCached: TorBox has it cached and the account copy is released; the hand-off
	// to the arr has not succeeded yet (retried every tick).
	SlowCached SlowLaneState = "cached"
	// SlowHandedOff: given back to the arr (revived download, re-search, or a new
	// completed download in the category). Note says which.
	SlowHandedOff SlowLaneState = "handed_off"
	// SlowImported: a library symlink now points at the release (the arr imported it).
	SlowImported SlowLaneState = "imported"
	// SlowFailed: gave up (max age, content gate, repeated errors). LastError says why.
	SlowFailed SlowLaneState = "failed"
	// SlowCancelled: removed by a human from the Web UI / API.
	SlowCancelled SlowLaneState = "cancelled"
)

// Active reports whether the worker still has something to do for the item.
func (s SlowLaneState) Active() bool {
	return s == SlowQueued || s == SlowDownloading || s == SlowCached
}

// SlowLaneItem is one torrent in the slow lane, keyed by infohash. JSON tags are the
// Web UI wire format; Magnet is internal plumbing (can carry a long tracker list).
type SlowLaneItem struct {
	Hash     string        `json:"hash"`
	Magnet   string        `json:"-"`
	Name     string        `json:"name"`     // display / folder name (magnet dn, arr title, TorBox name)
	Category string        `json:"category"` // qBit category = owning arr
	Origin   string        `json:"origin"`   // "manual" (Web UI/API) | "wait" (adopted wait download)
	State    SlowLaneState `json:"state"`
	TorBoxID int64         `json:"torbox_id"` // account torrent id while downloading, else 0

	QueuedAt   int64 `json:"queued_at"`   // unix; max_age is measured from here
	StartedAt  int64 `json:"started_at"`  // unix; when the TorBox download started (0 = not yet)
	CachedAt   int64 `json:"cached_at"`   // unix; when TorBox reported it cached
	FinishedAt int64 `json:"finished_at"` // unix; handed off / failed / cancelled
	UpdatedAt  int64 `json:"updated_at"`

	Progress float64 `json:"progress"` // 0..1 (last poll)
	ETA      int64   `json:"eta"`      // seconds, 0 = unknown
	Speed    int64   `json:"speed"`    // bytes/s
	Seeds    int     `json:"seeds"`
	Peers    int     `json:"peers"`
	Size     int64   `json:"size"` // known once cached

	Attempts  int    `json:"attempts"`   // consecutive start/hand-off errors
	LastError string `json:"last_error"` // last problem (also set while still retrying)
	Note      string `json:"note"`       // how it was handed off

	// Target optionally names what the item is for (API / Missing tab); nil = unknown.
	// The automatic manual import only imports files that map to it.
	Target *SlowLaneTarget `json:"target,omitempty"`
	// HandOff is how the item was handed off: revived | existing | regrab | added
	// ("" before hand-off, or for items handed off before v1.2.10).
	HandOff string `json:"handoff,omitempty"`

	// Automatic manual import (slow_lane.auto_import) of an "added" hand-off.
	AutoImportAttempts int    `json:"auto_import_attempts"`
	AutoImportAt       int64  `json:"auto_import_at"`   // unix; last attempt (0 = none)
	AutoImportDone     bool   `json:"auto_import_done"` // nothing more to try
	AutoImportNote     string `json:"auto_import_note"` // outcome of the last attempt
}

// SlowLaneTarget is the optional "what this download is for" of a slow-lane item. The
// JSON shape is the POST /api/slowlane "target" object. Season 0 = any season.
type SlowLaneTarget struct {
	SeriesID int    `json:"series_id,omitempty"` // Sonarr series id
	Season   int    `json:"season,omitempty"`    // Sonarr season number; 0 = any
	MovieID  int    `json:"movie_id,omitempty"`  // Radarr movie id
	Title    string `json:"title,omitempty"`     // display only
}

// IsZero reports whether the target names nothing.
func (t *SlowLaneTarget) IsZero() bool {
	return t == nil || (t.SeriesID == 0 && t.Season == 0 && t.MovieID == 0 && t.Title == "")
}

// SlowLaneStore persists slow-lane items. Separate from Store (like AnalysisStore) so
// the in-memory test fakes of Store need not grow; *sqliteStore implements it.
type SlowLaneStore interface {
	UpsertSlowLane(it *SlowLaneItem) error
	// GetSlowLane returns the item or ErrNotFound.
	GetSlowLane(hash string) (*SlowLaneItem, error)
	// ListSlowLane returns items in the given states (none = every state), oldest
	// queued first.
	ListSlowLane(states ...SlowLaneState) ([]*SlowLaneItem, error)
	// PruneSlowLane deletes finished (inactive) items whose FinishedAt is before ts.
	PruneSlowLane(before int64) (int, error)
}

// ReplacementState is the lifecycle of one replacement-search row
// (repair.search_replacement).
type ReplacementState string

const (
	// ReplacementPending waits for the arr's rescan to land, then for a search.
	ReplacementPending ReplacementState = "pending"
	// ReplacementDone is finished (searched, or dropped with Result saying why). The row
	// stays so SearchedAt keeps enforcing the per-item cooldown across restarts.
	ReplacementDone ReplacementState = "done"
)

// ReplacementItem is one episode (Sonarr) or movie (Radarr) whose library link died
// because TorBox dropped the content, keyed by (Category, ItemID).
type ReplacementItem struct {
	Category   string           `json:"category"`
	ItemID     int              `json:"item_id"` // episode id (Sonarr) or movie id (Radarr)
	Kind       string           `json:"kind"`    // "episode" | "movie"
	State      ReplacementState `json:"state"`
	QueuedAt   int64            `json:"queued_at"`
	SearchedAt int64            `json:"searched_at"` // last search sent; 0 = never
	Attempts   int              `json:"attempts"`    // arr errors / not-yet-missing checks
	Result     string           `json:"result"`      // last outcome (searched, cooldown, ...)
}

// ReplacementStore persists replacement-search rows. Separate from Store (like
// SlowLaneStore); *sqliteStore implements it.
type ReplacementStore interface {
	// GetReplacement returns the row or ErrNotFound.
	GetReplacement(category string, itemID int) (*ReplacementItem, error)
	UpsertReplacement(it *ReplacementItem) error
	// ListPendingReplacements returns pending rows queued at or before ts, oldest first.
	ListPendingReplacements(queuedBefore int64, limit int) ([]*ReplacementItem, error)
	CountPendingReplacements() (int, error)
	// CountReplacementSearchesSince counts rows whose last search was at or after ts.
	CountReplacementSearchesSince(ts int64) (int, error)
	// PruneReplacements deletes done rows last touched (queued or searched) before ts.
	PruneReplacements(before int64) (int, error)
}
