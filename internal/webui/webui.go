// Package webui is Lazarr's opt-in human dashboard. It serves a browser UI on a
// separate listener (webui.listen in config.yaml), kept off the arr-facing qbit port
// and the Prometheus admin port. The UI is unauthenticated by default (trusted-LAN
// model) but supports HTTP Basic Auth when webui.username + webui.password are set.
//
// The UI communicates with the browser via a JSON API at /api/* and renders HTML
// using Go html/template. No external build step is required; all assets are embedded
// in the binary via go:embed.
//
// Security: torbox.api_key is never rendered or logged by this package — the settings
// endpoint accepts a new key (write-only) but only ever reports whether one is set.
// All mutating endpoints (force-release, audit, settings, restart) accept POST only
// and respect the auth middleware.
package webui

import "github.com/your-username/lazarr/internal/catalog"

// MaterializedItem is the web UI view of one live in-memory materialized release
// ("active stream" in UI language), joined with its catalog identity so humans see
// titles, not infohashes.
type MaterializedItem struct {
	Hash       string `json:"hash"`
	Name       string `json:"name"`     // catalog release name ("" if unknown)
	Category   string `json:"category"` // arr name
	TotalSize  int64  `json:"total_size"`
	TorBoxID   int64  `json:"torbox_id"`
	Refs       int    `json:"refs"`
	LastUsedNs int64  `json:"last_used_ns"`
}

// AccountInfo is a cached snapshot of the TorBox /user/me response.
// May be nil if the account is not yet fetched or the fetch failed.
type AccountInfo struct {
	Plan          int    `json:"plan"`
	ActiveSlots   int    `json:"active_slots"`
	CooldownUntil string `json:"cooldown_until"`
	LongTermStore bool   `json:"long_term_store"`
}

// StatusSnapshot is the /api/status response body.
type StatusSnapshot struct {
	Version       string       `json:"version"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	Mounted       bool         `json:"mounted"`
	SlotsInUse    int          `json:"slots_in_use"`
	SlotsTotal    int          `json:"slots_total"`
	LastAuditUnix int64        `json:"last_audit_unix"`
	Account       *AccountInfo `json:"account,omitempty"`
}

// SafeConfig is the effective configuration with secrets redacted (api_key, passwords).
// Kept for the read-only view; the editable form uses Settings.
type SafeConfig struct {
	TorBoxAPIBase string   `json:"torbox_api_base"`
	QBitListen    string   `json:"qbit_listen"`
	AdminListen   string   `json:"admin_listen"`
	WebUIListen   string   `json:"webui_listen"`
	DownloadDir   string   `json:"download_dir"`
	FuseMount     string   `json:"fuse_mount"`
	DBPath        string   `json:"db_path"`
	Categories    []string `json:"categories"`
	AllowUncached bool     `json:"allow_uncached"`
	IdleTTL       string   `json:"idle_ttl"`
	MaxHold       string   `json:"max_hold"`
	ActiveSlots   int      `json:"active_slots"`
	ProbeCache    bool     `json:"probe_cache"`
	OwnershipPUID int      `json:"ownership_puid"`
	OwnershipPGID int      `json:"ownership_pgid"`
	AuthEnabled   bool     `json:"auth_enabled"`
}

// Settings is the editable configuration exchanged with the settings page.
//
// GET /api/settings returns it with TorBoxAPIKey and WebUIPassword ALWAYS empty;
// the *Set flags tell the form whether a value exists. POST /api/settings accepts
// the same shape: an empty TorBoxAPIKey / WebUIPassword means "keep the current
// value" (so the form can save without re-entering secrets).
type Settings struct {
	LogLevel string `json:"log_level"` // debug|info|warn|error; applied live on save

	TorBoxAPIKey    string `json:"torbox_api_key,omitempty"` // write-only
	TorBoxAPIKeySet bool   `json:"torbox_api_key_set"`
	TorBoxAPIBase   string `json:"torbox_api_base"`

	QBitListen   string `json:"qbit_listen"`
	QBitUsername string `json:"qbit_username"`
	QBitPassword string `json:"qbit_password"` // shown: the arr needs it to connect

	Categories []string `json:"categories"` // one per arr instance

	DownloadDir   string `json:"download_dir"`
	FuseMount     string `json:"fuse_mount"`
	DBPath        string `json:"db_path"`
	ProbeCacheDir string `json:"probe_cache_dir"`

	AllowUncached bool   `json:"allow_uncached"`
	IdleTTL       string `json:"idle_ttl"` // Go duration string, e.g. "168h"
	MaxHold       string `json:"max_hold"` // Go duration string, e.g. "720h"
	ActiveSlots   int    `json:"active_slots"`
	ProbeCache    bool   `json:"probe_cache"`

	OnCacheMiss       string `json:"on_cache_miss"`     // error | reject | wait
	CacheWaitBudget   string `json:"cache_wait_budget"` // Go duration, e.g. "15m"
	MaxWaitDownloads  int    `json:"max_wait_downloads"`
	CacheWaitMax      string `json:"cache_wait_max"`      // Go duration; "0s" = 3x the budget
	ReadaheadWindows  int    `json:"readahead_windows"`   // 0=off; 4-8 for 4K streaming
	ReadaheadChunkMiB int    `json:"readahead_chunk_mib"` // MiB per window; 1-64, default 2

	PUID         int  `json:"puid"`
	PGID         int  `json:"pgid"`
	OwnershipEnv bool `json:"ownership_env"` // PUID/PGID come from the environment (read-only)

	MetricsListen string `json:"metrics_listen"`

	WebUIListen      string `json:"webui_listen"`
	WebUIUsername    string `json:"webui_username"`
	WebUIPassword    string `json:"webui_password,omitempty"` // write-only
	WebUIPasswordSet bool   `json:"webui_password_set"`

	// ---- Workstream G: local cache + playback (docs/28) ----
	// Secrets (Plex token, Jellyfin key, arr keys) are NOT editable here: they come
	// from environment variables and the *Set/*Env flags only report presence.
	Cache    CacheSettings    `json:"cache"`
	Playback PlaybackSettings `json:"playback"`
	SlowLane SlowLaneSettings `json:"slow_lane"`
	// Cleanup is a pointer so a form that does not send it keeps the current values.
	Cleanup  *CleanupSettings `json:"cleanup,omitempty"`
	Profiles []ProfileInfo    `json:"profiles,omitempty"` // read-only presets for the form
}

// CleanupSettings mirrors config.Cleanup for the settings form (restart to apply).
type CleanupSettings struct {
	RemoveEmptyDirs bool   `json:"remove_empty_dirs"`
	EmptyDirGrace   string `json:"empty_dir_grace"` // Go duration, e.g. "1h"
	// LibraryRoots edits playback.arrs[].library_root (category -> host path of the
	// arr's root folder); nil keeps the current values.
	LibraryRoots map[string]string `json:"library_roots"`
	// NoLibrarySweep edits playback.arrs[].no_library_sweep (categories); nil keeps.
	NoLibrarySweep []string `json:"no_library_sweep"`
	SweepLibrary   bool     `json:"sweep_library"`
}

// CacheSettings mirrors config.LocalCache for the settings form.
type CacheSettings struct {
	Enabled             bool     `json:"enabled"`
	Dir                 string   `json:"dir"`
	Publish             bool     `json:"publish"`
	PublishAt           string   `json:"publish_at"`
	LibraryRoots        []string `json:"library_roots"`
	LinkIndexInterval   string   `json:"link_index_interval"`
	UnreferencedGrace   string   `json:"unreferenced_grace"`
	TorrentOnly         bool     `json:"torrent_only"`
	Profile             string   `json:"profile"`
	FreeHighPct         int      `json:"free_high_pct"`
	FreeLowPct          int      `json:"free_low_pct"`
	ReservePct          int      `json:"reserve_pct"`
	TTLWatching         string   `json:"ttl_watching"`
	TTLFinished         string   `json:"ttl_finished"`
	TTLFinishedMovie    string   `json:"ttl_finished_movie"` // "0s" = same as ttl_finished
	TTLUntouched        string   `json:"ttl_untouched"`
	TTLNewMovie         string   `json:"ttl_new_movie"`
	TTLDefault          string   `json:"ttl_default"`
	ActiveWindow        string   `json:"active_window"`
	NewReleaseWindow    string   `json:"new_release_window"`
	PullOnGrab          []string `json:"pull_on_grab"`
	PrefetchAhead       int      `json:"prefetch_ahead"`
	PullCurrent         bool     `json:"pull_current"`
	MaxConcurrentPulls  int      `json:"max_concurrent_pulls"`
	MaxPullsPerHour     int      `json:"max_pulls_per_hour"`
	SpeculativeGBPerDay int      `json:"speculative_gb_per_day"`
	MinFileMiB          int      `json:"min_file_mib"`
	PullWindowMiB       int      `json:"pull_window_mib"`
	StableMtime         bool     `json:"stable_mtime"`
}

// SlowLaneSettings mirrors config.SlowLane for the settings form (restart to apply).
type SlowLaneSettings struct {
	Enabled           bool   `json:"enabled"`
	MaxConcurrent     int    `json:"max_concurrent"`
	PollInterval      string `json:"poll_interval"` // Go duration, e.g. "10m"
	MaxAge            string `json:"max_age"`       // Go duration or "Nd"; "0s" = never give up
	AdoptWaitFailures bool   `json:"adopt_wait_failures"`
	// AutoImport is a pointer so a form that does not send it keeps the current value
	// (it defaults to on).
	AutoImport      *bool  `json:"auto_import,omitempty"`
	AutoImportAfter string `json:"auto_import_after"` // Go duration, e.g. "15m"
}

// SlowLaneView is the GET /api/slowlane body: whether the lane runs and its items.
type SlowLaneView struct {
	Enabled         bool                    `json:"enabled"` // running config (not a pending save)
	MaxConcurrent   int                     `json:"max_concurrent"`
	PollInterval    string                  `json:"poll_interval"`
	AutoImport      bool                    `json:"auto_import"`
	AutoImportAfter string                  `json:"auto_import_after"`
	Categories      []string                `json:"categories"`
	CategoryKinds   map[string]string       `json:"category_kinds,omitempty"` // category -> "sonarr" | "radarr" (absent = unknown): the UI's kind warning
	Items           []*catalog.SlowLaneItem `json:"items"`
}

// PlaybackSettings mirrors config.Playback (secrets as presence flags only).
type PlaybackSettings struct {
	PollInterval      string           `json:"poll_interval"`
	WatchedPct        int              `json:"watched_pct"`
	RefreshOnRetarget bool             `json:"refresh_on_retarget"`
	PathMap           []PathMapSetting `json:"path_map"`
	PlexURL           string           `json:"plex_url"`
	PlexTokenSet      bool             `json:"plex_token_set"`
	PlexTokenEnv      bool             `json:"plex_token_env"`
	JellyfinURL       string           `json:"jellyfin_url"`
	JellyfinKeySet    bool             `json:"jellyfin_key_set"`
	JellyfinKeyEnv    bool             `json:"jellyfin_key_env"`
	Arrs              []ArrSetting     `json:"arrs"`
}

type PathMapSetting struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ArrSetting is one Radarr/Sonarr instance; the key is env-only (LAZARR_ARR_API_KEY_<CATEGORY>).
type ArrSetting struct {
	Category string `json:"category"`
	URL      string `json:"url"`
	Kind     string `json:"kind"`
	KeySet   bool   `json:"key_set"`
	KeyEnv   bool   `json:"key_env"`
	EnvVar   string `json:"env_var"`
}

// ProfileInfo is a retention preset the form can apply client-side.
type ProfileInfo struct {
	Name          string `json:"name"`
	Finished      string `json:"finished"`
	FinishedMovie string `json:"finished_movie"`
	Untouched     string `json:"untouched"`
	NewMovie      string `json:"new_movie"`
	Default       string `json:"default"`
	FreeHighPct   int    `json:"free_high_pct"`
	FreeLowPct    int    `json:"free_low_pct"`
}

// SaveResult is the POST /api/settings response.
type SaveResult struct {
	Saved           bool `json:"saved"`
	RestartRequired bool `json:"restart_required"`
}
