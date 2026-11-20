// Package config loads Lazarr's config.yaml. See docs/05-spec.md §7 and docs/11.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/constants"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "15m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	pd, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(pd)
	return nil
}

// ParseDuration is time.ParseDuration plus a "Nd" days form ("270d", "1.5d"), which
// the retention windows use. Anything time.ParseDuration accepts still works.
func ParseDuration(s string) (time.Duration, error) {
	t := strings.TrimSpace(s)
	if strings.HasSuffix(t, "d") && !strings.ContainsAny(strings.TrimSuffix(t, "d"), "hms") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(t, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q: %w", s, err)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(t)
}

// MarshalYAML renders the duration back as a human string ("168h0m0s"), so a
// Config round-trips through Save/Load (the Web UI settings editor rewrites the
// whole file).
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

type TorBox struct {
	APIKey  string `yaml:"api_key"`
	APIBase string `yaml:"api_base"`
	// ExtraCDNHosts are additional CDN host SUFFIXES the stream proxy's host-pin
	// accepts, e.g. ".tb-cdn.net". The built-ins (.tb-cdn.io, .tb-cdn.earth) are
	// always allowed. TorBox occasionally serves a CDN option under a brand-new
	// domain (ERTH shipped under .tb-cdn.earth and broke pinned installs until
	// v1.1.1) — this is the operator escape hatch so streaming can be unblocked by
	// a config edit instead of waiting for a Lazarr release.
	ExtraCDNHosts []string `yaml:"extra_cdn_hosts,omitempty"`
	// MaxRequestsPerMinute caps Lazarr's own TorBox API calls (token bucket, burst =
	// 1/12 of it). TorBox allows 300 per 60 s per API key; the default 240 leaves room
	// for anything else that uses the key. 0 = default. Range 1..300.
	MaxRequestsPerMinute int `yaml:"max_requests_per_minute"`
}

type QBit struct {
	Listen   string `yaml:"listen"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Paths struct {
	DownloadDir   string `yaml:"download_dir"`    // arr's qBit save path (per-category subdirs)
	FuseMount     string `yaml:"fuse_mount"`      // virtual tree root
	DBPath        string `yaml:"db_path"`         // SQLite catalog file (default /data/lazarr.sqlite)
	ProbeCacheDir string `yaml:"probe_cache_dir"` // bounded header cache (default /data/probe)
}

type Policy struct {
	AllowUncached bool     `yaml:"allow_uncached"`
	IdleTTL       Duration `yaml:"idle_ttl"`
	MaxHold       Duration `yaml:"max_hold"`
	ActiveSlots   int      `yaml:"active_slots"` // 0 = auto-detect from /user/me; else your plan's slots
	ProbeCache    bool     `yaml:"probe_cache"`

	// OnCacheMiss decides what happens when an arr grabs a release TorBox has NOT
	// cached (only consulted when AllowUncached is false — with AllowUncached true
	// every grab is accepted):
	//   "error"  (default) — accept the grab and surface it as a qBit error state;
	//             the arr shows a warning until something (Cleanuparr, a human, or
	//             the repair tab) removes it.
	//   "reject" — refuse the add outright; the arr immediately falls back to the
	//             next release in its decision list. Cleanest queues, but repeated
	//             rejections can trip the arr's download-client backoff.
	//   "wait"   — start the TorBox download and watch it: if TorBox's reported ETA
	//             stays within CacheWaitBudget the grab is held in "downloading"
	//             state (real progress reported to the arr) until cached, then the
	//             torrent is RELEASED from the account (the content is now in
	//             TorBox's cache) and the grab completes as a normal lazy import.
	//             Past the budget a download whose progress is still rising keeps
	//             waiting, up to CacheWaitMax; one that stalled, or whose ETA
	//             lands beyond CacheWaitMax, is deleted and the grab goes to
	//             error state.
	OnCacheMiss string `yaml:"on_cache_miss"`
	// OnCacheMissByCategory overrides OnCacheMiss per qBit category, e.g.
	// {sonarr_main_seerr: reject, radarr_main_seerr: reject} so Seerr-tagged grabs (sent
	// through a second "Lazarr Seerr" download client with its own category) are
	// cached-only while direct adds keep waiting. Unlisted categories use OnCacheMiss.
	OnCacheMissByCategory map[string]string `yaml:"on_cache_miss_by_category,omitempty"`
	// CacheWaitBudget is how long a "wait" download gets unconditionally. After it,
	// only a download whose progress is still rising keeps waiting. Only used when
	// OnCacheMiss == "wait".
	CacheWaitBudget Duration `yaml:"cache_wait_budget"`
	// CacheWaitMax is the hard ceiling for a "wait" download that is still
	// progressing past CacheWaitBudget. 0 = 3x CacheWaitBudget. Set it equal to
	// CacheWaitBudget to bail strictly at the budget (pre-1.2.5 behavior).
	CacheWaitMax Duration `yaml:"cache_wait_max"`
	// RepairUnlinkDead lets the daily repair scan remove library symlinks whose
	// release TorBox has evicted (confirmed by two scans at least 12h apart, at most
	// 50 releases per run), then asks the owning arr in playback.arrs to rescan the
	// series/movie, so it marks those files missing and its missing search re-grabs
	// them. Off by default: it deletes library symlinks.
	RepairUnlinkDead bool `yaml:"repair_unlink_dead"`
	// WaitFailBlocklist makes a failed "wait" download (bailed over budget/stalled,
	// vanished, or not cached after download) count as a FAILED download in the arr,
	// not just a warning. qBit has no state the arrs map to "failed" — they render
	// "error" as a warning on purpose — so Lazarr removes the item from the owning
	// arr's queue itself with blocklist=true: the arr blocklists that release and
	// searches for another. Needs the category's arr in playback.arrs (URL +
	// LAZARR_ARR_API_KEY_<CATEGORY>); categories without one keep the error state.
	// The same path fails a content-gate refusal (accepted as an empty error row instead
	// of "Fails.", which the arrs never blocklist) and a release the materialize engine
	// marks errored on dead-cache.
	// Off by default: it writes to the arr (blocklist + search).
	WaitFailBlocklist bool `yaml:"wait_fail_blocklist"`
	// DetachOnDelete: when an arr deletes a release from the client (torrents/delete)
	// while symlinks it already imported into its library still point at it (a
	// partially imported season pack, a queue cleaner, a manual removal), keep the
	// catalog row — hidden from the arr — so those files keep playing, and remove only
	// Lazarr's own download_dir symlinks. Rows whose library links are all gone are
	// removed by a daily sweep. Default true. false = pre-1.2.8 behavior: every delete
	// drops the row and any imported library links dangle.
	DetachOnDelete bool `yaml:"detach_on_delete"`
	// UseArrReleaseName names the download folder after the release title the arr
	// grabbed (the magnet dn / .torrent name) instead of TorBox's cached name, whenever
	// that title is usable as a folder name. TorBox can cache a season pack under one
	// episode's file name; Sonarr then parsed the folder as that single episode and
	// rejected the rest of the pack (GoT S01, 2026-10). TorBox's name is kept in the
	// catalog (torbox_name). Default true; affects new grabs only.
	UseArrReleaseName bool `yaml:"use_arr_release_name"`
	// PackJanitor (opt-in): every 5 min, remove from each playback.arrs queue the
	// Lazarr downloads that are importBlocked (or importPending on two passes) although at least one file was imported
	// (arr history) and every remaining rejection only says the arr already has the
	// episode ("Not an upgrade", "Not a Custom Format upgrade", "already imported",
	// "Existing file"). Removal uses blocklist=false&skipRedownload=true, so nothing is
	// blocklisted or re-searched. Requires detach_on_delete (the removal calls
	// torrents/delete, which must keep the imported links). Default false.
	PackJanitor bool `yaml:"pack_janitor"`
	// PackJanitorClients are the arr-side download-client names that are Lazarr.
	// Default ["Lazarr", "Lazarr Seerr"].
	PackJanitorClients []string `yaml:"pack_janitor_clients,omitempty"`
	// RefuseAddErrors: when an add cannot be completed cleanly — checkcached failed
	// (transport error, API backoff), the wait cap is full, the uncached createtorrent
	// failed, or the allow_uncached torrentinfo lookup failed — answer "Fails." so the
	// arr moves on to its next release, instead of storing an "error" row the arr shows
	// as a warning forever.
	// Default true. on_cache_miss: error still creates error rows by design.
	RefuseAddErrors bool `yaml:"refuse_add_errors"`
	// WaitNoProgress: a wait download whose progress has not risen for this long while
	// TorBox sees 0 seeds and 0 peers is a dead swarm: bail early (TorBox delete +
	// wait_fail_blocklist) instead of burning the whole wait budget. 0 = off. Default 15m.
	WaitNoProgress Duration `yaml:"wait_no_progress"`
	// MaxWaitDownloads caps concurrent "wait" downloads (each holds a TorBox slot
	// while downloading). Overflow misses fall back to error state.
	MaxWaitDownloads int `yaml:"max_wait_downloads"`

	// ContentGate refuses ("Fails.") at add time any release whose checkcached file
	// list cannot be a real movie/episode: an executable or script file, no video file
	// of at least MinVideoMiB, or archives per RejectArchives. The arr then falls
	// through to its next release. Default true.
	ContentGate bool `yaml:"content_gate"`
	// MinVideoMiB: the content gate requires at least one video file this large.
	// 0 = no size requirement. Default 100.
	MinVideoMiB int `yaml:"min_video_mib"`
	// RejectArchives: "only" (default) refuses archives with no qualifying video;
	// "any" refuses a release containing ANY archive (.rar/.rNN/.zip/.7z/.tar/.gz/.tgz,
	// even a small subtitle zip); "none" never refuses for archives.
	RejectArchives string `yaml:"reject_archives"`

	// RequestDLNegativeTTL: after a requestdl failure for one file (a 5xx, an unusable
	// answer, a link TorBox reports expired), further reads of that file fail fast for
	// this long WITHOUT calling TorBox. Every FUSE read used to retry requestdl, and an
	// arr probing a stuck import every minute turned that into a 429 storm. 0 = off.
	// Default 90s. Rate-limit backoffs are handled by the TorBox governor instead.
	RequestDLNegativeTTL Duration `yaml:"requestdl_negative_ttl"`
	// BackoffReadWait: while the TorBox API is backing off (HTTP 429 / remaining 0), a
	// FUSE read that needs an API call fails at once with EIO (default 0). Set e.g. 10s
	// to let reads wait out a backoff that ends within that long instead.
	BackoffReadWait Duration `yaml:"backoff_read_wait"`

	// ReadaheadWindows is the number of chunk-sized windows prefetched in parallel
	// ahead of a sequential read (per open stream). 0 disables prefetch (each read
	// is one serial CDN round-trip). 4-8 is the 4K-streaming range; memory cost is
	// ReadaheadWindows × ReadaheadChunkMiB per active stream and discarded
	// prefetches count against TorBox bandwidth.
	ReadaheadWindows int `yaml:"readahead_windows"`

	// ReadaheadChunkMiB is the size of each readahead window in MiB. Larger chunks
	// mean fewer CDN round-trips (higher throughput) but more RAM and more wasted
	// transfer on seek/stop. Range 1-64; default 2.
	ReadaheadChunkMiB int `yaml:"readahead_chunk_mib"`
}

// EffectiveOnCacheMiss is the miss policy for a category: its on_cache_miss_by_category
// entry when present, else on_cache_miss; "" normalizes to "error".
func (p Policy) EffectiveOnCacheMiss(category string) string {
	mode := p.OnCacheMiss
	if m, ok := p.OnCacheMissByCategory[category]; ok && m != "" {
		mode = m
	}
	if mode == "" {
		return "error"
	}
	return mode
}

// AnyWait reports whether any category (or the default) uses on_cache_miss=wait.
func (p Policy) AnyWait() bool {
	if p.OnCacheMiss == "wait" {
		return true
	}
	for _, m := range p.OnCacheMissByCategory {
		if m == "wait" {
			return true
		}
	}
	return false
}

// EffectiveCacheWaitMax is the hard ceiling for a progressing "wait" download:
// CacheWaitMax when set, otherwise 3x CacheWaitBudget, never below the budget.
func (p Policy) EffectiveCacheWaitMax() time.Duration {
	budget := p.CacheWaitBudget.D()
	if m := p.CacheWaitMax.D(); m > 0 {
		return max(m, budget)
	}
	return 3 * budget
}

// Ownership controls the privilege model for the symlink tree (docs/05 §5).
//
// Lazarr's daemon must run as root inside the container to mount FUSE: in Docker
// the CAP_SYS_ADMIN capability needed for mount(2) is only effective for uid 0.
// But the *arr suite (Sonarr/Radarr) runs as its own uid (e.g. 1001) and must be
// able to move/delete the symlinks Lazarr creates during import — which requires
// those symlinks and their parent dirs to be owned by the arr's uid:gid.
//
// The chosen model is "run as root + chown to PUID/PGID": after creating each
// directory and each symlink, the symlink manager chowns it to PUID:PGID. PUID/
// PGID == 0 disables chown (leave ownership as-is — i.e. root, the daemon's uid).
type Ownership struct {
	PUID int `yaml:"puid"` // 0 = disabled (leave as-is); else chown created symlinks/dirs to this uid
	PGID int `yaml:"pgid"` // 0 = disabled (leave as-is); else chown created symlinks/dirs to this gid
}

// Metrics configures the OPT-IN observability admin server. It is disabled by
// default (empty Listen). When set it serves /metrics (Prometheus) and /health
// (JSON) on a SEPARATE listener, kept off the arr-facing qbit port; like the
// qbit listener it is unauthenticated, so bind it to the trusted LAN.
type Metrics struct {
	Listen string `yaml:"listen"` // e.g. ":9090"; empty = admin server disabled
}

// WebUI configures the OPT-IN human dashboard. It is disabled by default (empty
// Listen). When set it serves the Lazarr Web UI on a SEPARATE listener, kept off
// the arr-facing qbit port. It is unauthenticated by default (trusted-LAN model,
// like the qbit and metrics ports). Set Username + Password to enable HTTP Basic
// Auth, which is recommended because the UI exposes more context and includes
// mutating actions (force-release, run-audit).
type WebUI struct {
	Listen   string `yaml:"listen"`   // e.g. ":8081"; empty = web UI disabled
	Username string `yaml:"username"` // optional basic-auth username
	Password string `yaml:"password"` // optional basic-auth password
}

// Audit configures the account orphan audit (v1.2.8). Every ToS audit (5 min) lists
// TorBox account torrents that no catalog row references (not materialized, not a wait
// download, not in flight); the list is shown in the Web UI and logged.
type Audit struct {
	// DeleteOrphansOlderThan, when > 0, deletes (controltorrent delete) orphans older
	// than this, at most 10 per audit. 0 (default) = report only. CAUTION: on an account
	// shared with another tool, its torrents are orphans to Lazarr too.
	DeleteOrphansOlderThan Duration `yaml:"delete_orphans_older_than"`
}

// Repair configures the dangling library-link sweeper (v1.2.8). Every hour Lazarr walks
// the library roots for symlinks into its FUSE/cache tree whose release is gone from the
// catalog and reports them (Web UI Health tab, lazarr_dangling_library_links).
type Repair struct {
	// UnlinkOrphans removes those links (symlinks only) once a hash has been seen
	// dangling for an hour, at most 20 hashes per run, then asks the owning arr
	// (playback.arrs) to rescan the series/movie so the files become missing and get
	// re-searched. Default false (report only).
	UnlinkOrphans bool `yaml:"unlink_orphans"`
	// SearchReplacement asks the arr to search for exactly the episodes/movies whose
	// library links were removed (by policy.repair_unlink_dead or UnlinkOrphans) once its
	// rescan shows them missing. Opt-in and bounded; see SearchReplacement.
	SearchReplacement SearchReplacement `yaml:"search_replacement"`
}

// SearchReplacement bounds the replacement search (repair.search_replacement). It never
// adds anything to TorBox: it only sends the arr an EpisodeSearch / MoviesSearch, and the
// arr's grab goes through Lazarr's normal add path (cached-only unless
// policy.allow_uncached / on_cache_miss say otherwise). TorBox flags misconfigured
// automated tools, so every knob below is a hard cap.
type SearchReplacement struct {
	// Enabled turns it on. Default false. Needs policy.repair_unlink_dead or
	// repair.unlink_orphans (the only sources) and the category in playback.arrs.
	Enabled bool `yaml:"enabled"`
	// MaxPerHour caps searched episodes/movies per rolling hour across all arrs; the
	// rest wait (at most 200 pending, then new ones are dropped). 1..30, default 10.
	MaxPerHour int `yaml:"max_per_hour"`
	// Cooldown is the minimum gap between two searches for the same episode/movie, so a
	// replacement that dies again cannot loop. Persisted. 24h..720h, default 72h.
	Cooldown Duration `yaml:"cooldown"`
}

// validate checks the replacement-search knobs (also while disabled).
func (r Repair) validate(p Policy) error {
	sr := r.SearchReplacement
	if sr.MaxPerHour < 1 || sr.MaxPerHour > 30 {
		return fmt.Errorf("repair.search_replacement.max_per_hour (%d) must be 1..30", sr.MaxPerHour)
	}
	if d := sr.Cooldown.D(); d < 24*time.Hour || d > 720*time.Hour {
		return fmt.Errorf("repair.search_replacement.cooldown (%s) must be 24h..720h", d)
	}
	if sr.Enabled && !p.RepairUnlinkDead && !r.UnlinkOrphans {
		return fmt.Errorf("repair.search_replacement.enabled requires policy.repair_unlink_dead or repair.unlink_orphans")
	}
	return nil
}

// SlowLane configures the slow lane (v1.2.9): a persistent queue of uncached torrents
// TorBox may take hours or days to fetch (old shows, multi-season packs, thin swarms).
// Unlike on_cache_miss=wait there is no time budget and nothing blocks an arr: the
// worker downloads at most MaxConcurrent items at a time, polls every PollInterval, and
// hands each one off once TorBox has it cached (see internal/qbit/slowlane.go). Items
// are added from the Web UI / API, or adopted from wait downloads that failed only for
// being slow (AdoptWaitFailures).
type SlowLane struct {
	// Enabled turns the worker and the Web UI/API entry points on. Default false.
	Enabled bool `yaml:"enabled"`
	// MaxConcurrent caps simultaneous slow-lane downloads on the TorBox account. Each
	// one holds an account download slot next to wait downloads and playback, so keep
	// it small (TorBox Essential has 3 active slots). 1..5, default 1.
	MaxConcurrent int `yaml:"max_concurrent"`
	// PollInterval is how often the worker checks progress and starts queued items.
	// 1m..24h, default 10m. Each tick costs one mylist call per active download.
	PollInterval Duration `yaml:"poll_interval"`
	// MaxAge gives up on an item (deleting any TorBox transfer) this long after it was
	// queued. 0 = never; otherwise at least 1h. Default 168h (7 days).
	MaxAge Duration `yaml:"max_age"`
	// AdoptWaitFailures hands an on_cache_miss=wait download that failed ONLY for being
	// slow (budget elapsed, ETA over budget, max wait, dead swarm) to the slow lane
	// instead of deleting it. The arr still sees the failure (and, with
	// wait_fail_blocklist, blocklists the release and searches on); once the slow lane
	// has the content cached it clears that blocklist entry and asks the arr to search
	// the episodes/movie again. Default false.
	AdoptWaitFailures bool `yaml:"adopt_wait_failures"`
	// AutoImport finishes "added" hand-offs (the arr never grabbed the hash, so its
	// completed-download handling only has the folder name to go on) with the arr's
	// manual import, file by file: multi-season packs, foreign-language folder names,
	// 5x01-style folders. Only files without rejections that map to one series/movie
	// (the item's target when set) and to monitored episodes/movies WITHOUT a file are
	// imported; at most 3 attempts per item. Needs the category in playback.arrs.
	// Default true.
	AutoImport bool `yaml:"auto_import"`
	// AutoImportAfter is how long after the hand-off the arr's own import gets before
	// Lazarr steps in (also the gap between attempts). 1m..24h, default 15m.
	AutoImportAfter Duration `yaml:"auto_import_after"`
}

// Missing configures the Web UI "Missing" tab: a periodic, read-only snapshot of what
// the arrs in playback.arrs still want (monitored, aired, no file), so the owner can
// find releases for it and hand them to the slow lane. See internal/missing.
type Missing struct {
	// Enabled turns the collector on. Default true; it only runs when playback.arrs
	// has at least one arr with a URL and an API key.
	Enabled bool `yaml:"enabled"`
	// RefreshInterval is how often every arr is asked again (about three requests per
	// arr). 5m..24h, default 30m. The tab's Refresh button may refresh sooner, at most
	// once a minute.
	RefreshInterval Duration `yaml:"refresh_interval"`
}

// validate checks the missing-list knobs (also while disabled, like the slow lane).
func (m Missing) validate() error {
	if d := m.RefreshInterval.D(); d < 5*time.Minute || d > 24*time.Hour {
		return fmt.Errorf("missing.refresh_interval (%s) must be 5m..24h", d)
	}
	return nil
}

type Config struct {
	// LogLevel is one of debug|info|warn|error (default info). The Web UI settings
	// page applies a change live via slog.LevelVar — no restart needed.
	LogLevel   string    `yaml:"log_level"`
	TorBox     TorBox    `yaml:"torbox"`
	QBit       QBit      `yaml:"qbit"`
	Paths      Paths     `yaml:"paths"`
	Categories []string  `yaml:"categories"`
	Policy     Policy    `yaml:"policy"`
	Audit      Audit     `yaml:"audit"`
	Repair     Repair    `yaml:"repair"`
	Cleanup    Cleanup   `yaml:"cleanup"`
	SlowLane   SlowLane  `yaml:"slow_lane"`
	Missing    Missing   `yaml:"missing"`
	Ownership  Ownership `yaml:"ownership"`
	Metrics    Metrics   `yaml:"metrics"`
	WebUI      WebUI     `yaml:"webui"`
	// LocalCache + Playback: Workstream G (docs/28). Both default to disabled/empty.
	LocalCache LocalCache `yaml:"local_cache"`
	Playback   Playback   `yaml:"playback"`
	// ConfigDir is the directory the config file was loaded from (set by Load; not
	// serialized). The local cache must never publish it into the shared tree.
	ConfigDir string `yaml:"-"`
}

// Default returns a Config pre-populated with sane defaults; YAML overlays it.
func Default() *Config {
	return &Config{
		TorBox: TorBox{APIBase: constants.TorBoxAPIBase, MaxRequestsPerMinute: 240},
		QBit:   QBit{Listen: ":8080", Username: "lazarr", Password: "lazarr"},
		Policy: Policy{
			AllowUncached:        false,
			IdleTTL:              Duration(constants.DefaultIdleTTL),
			MaxHold:              Duration(constants.DefaultMaxHold),
			ActiveSlots:          constants.DefaultActiveSlots,
			ProbeCache:           true,
			OnCacheMiss:          "error",
			CacheWaitBudget:      Duration(15 * time.Minute),
			MaxWaitDownloads:     1,
			DetachOnDelete:       true,
			RefuseAddErrors:      true,
			UseArrReleaseName:    true,
			WaitNoProgress:       Duration(15 * time.Minute),
			ContentGate:          true,
			MinVideoMiB:          100,
			RejectArchives:       "only",
			RequestDLNegativeTTL: Duration(90 * time.Second),
			ReadaheadWindows:     4,
			ReadaheadChunkMiB:    2,
		},
		SlowLane: SlowLane{
			MaxConcurrent:   1,
			PollInterval:    Duration(10 * time.Minute),
			MaxAge:          Duration(7 * 24 * time.Hour),
			AutoImport:      true,
			AutoImportAfter: Duration(15 * time.Minute),
		},
		Repair: Repair{SearchReplacement: SearchReplacement{
			MaxPerHour: 10,
			Cooldown:   Duration(72 * time.Hour),
		}},
		Missing:    Missing{Enabled: true, RefreshInterval: Duration(30 * time.Minute)},
		Cleanup:    defaultCleanup(),
		LocalCache: defaultLocalCache(),
		Playback:   defaultPlayback(),
	}
}

// Load reads config.yaml over the defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path, by design
	if err != nil {
		return nil, err
	}
	c := Default()
	// The local_cache.profile preset must be applied BEFORE the file is overlaid so
	// explicit ttl/watermark values in the YAML win over the preset. Peek at it first.
	var peek struct {
		LocalCache struct {
			Profile string `yaml:"profile"`
		} `yaml:"local_cache"`
	}
	if err := yaml.Unmarshal(b, &peek); err != nil {
		return nil, err
	}
	if err := c.LocalCache.applyProfile(peek.LocalCache.Profile); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if abs, aerr := filepath.Abs(path); aerr == nil {
		c.ConfigDir = filepath.Dir(abs)
	}
	ApplyEnv(c, os.LookupEnv)
	if c.TorBox.APIBase == "" {
		c.TorBox.APIBase = constants.TorBoxAPIBase
	}
	if c.Paths.DBPath == "" {
		c.Paths.DBPath = "/data/lazarr.sqlite"
	}
	if c.Paths.ProbeCacheDir == "" {
		c.Paths.ProbeCacheDir = "/data/probe"
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// validate rejects nonsensical configurations after defaults are applied, so the
// daemon fails fast at startup instead of misbehaving at runtime. It is called by
// Load; tests call it directly via table-driven cases.
func (c *Config) validate() error {
	// (a) If we are serving any categories we need both the download dir (where the
	// import symlinks live) and the FUSE mount (their targets). Empty either way is a
	// misconfiguration that would silently break import.
	if len(c.Categories) > 0 {
		if c.Paths.DownloadDir == "" {
			return fmt.Errorf("download_dir is required when categories are set")
		}
		if c.Paths.FuseMount == "" {
			return fmt.Errorf("fuse_mount is required when categories are set")
		}
	}

	// (b) idle_ttl must be strictly less than max_hold: max_hold is the hard ceiling
	// the idle reaper sits under, so idle >= max_hold means the idle sweep never wins.
	if c.Policy.IdleTTL.D() >= c.Policy.MaxHold.D() {
		return fmt.Errorf("idle_ttl (%s) must be strictly less than max_hold (%s)",
			c.Policy.IdleTTL.D(), c.Policy.MaxHold.D())
	}

	// (c) active_slots may be 0 (auto-detect from /user/me) but never negative.
	if c.Policy.ActiveSlots < 0 {
		return fmt.Errorf("active_slots (%d) must be >= 0 (0 = auto-detect)", c.Policy.ActiveSlots)
	}

	// (d) puid/pgid are uids/gids; 0 disables chown, negative is meaningless. A malformed
	// PUID/PGID environment override fails fast rather than falling back to the YAML.
	if err := c.Playback.EnvSet.ownershipErr; err != nil {
		return err
	}
	if c.Ownership.PUID < 0 {
		return fmt.Errorf("puid (%d) must be >= 0 (0 = disabled)", c.Ownership.PUID)
	}
	if c.Ownership.PGID < 0 {
		return fmt.Errorf("pgid (%d) must be >= 0 (0 = disabled)", c.Ownership.PGID)
	}
	// (d2/S5) chown needs BOTH puid and pgid: chownEnabled() requires both > 0, so setting
	// exactly one silently disables chown and produces the "arr can't move/import" failure
	// docs/20 §9 troubleshoots. Reject the half-set config with a clear, named error.
	if (c.Ownership.PUID > 0) != (c.Ownership.PGID > 0) {
		return fmt.Errorf("ownership.puid (%d) and ownership.pgid (%d) must both be set (>0) or both be 0 (chown disabled)",
			c.Ownership.PUID, c.Ownership.PGID)
	}

	// (e) webui basic auth: both username AND password must be set, or neither.
	if (c.WebUI.Username == "") != (c.WebUI.Password == "") {
		return fmt.Errorf("webui.username and webui.password must both be set or both be empty")
	}

	// (f2) on_cache_miss must be a recognized mode (empty = error).
	switch c.Policy.OnCacheMiss {
	case "", "error", "reject", "wait":
	default:
		return fmt.Errorf("on_cache_miss %q must be one of error|reject|wait", c.Policy.OnCacheMiss)
	}
	for cat, m := range c.Policy.OnCacheMissByCategory {
		switch m {
		case "error", "reject", "wait":
		default:
			return fmt.Errorf("on_cache_miss_by_category[%s] %q must be one of error|reject|wait", cat, m)
		}
	}
	if c.Policy.AnyWait() && c.Policy.CacheWaitBudget.D() <= 0 {
		return fmt.Errorf("cache_wait_budget must be > 0 when on_cache_miss is \"wait\"")
	}
	if c.Policy.CacheWaitMax.D() < 0 ||
		(c.Policy.CacheWaitMax.D() > 0 && c.Policy.CacheWaitMax.D() < c.Policy.CacheWaitBudget.D()) {
		return fmt.Errorf("cache_wait_max (%s) must be 0 (auto) or >= cache_wait_budget (%s)",
			c.Policy.CacheWaitMax.D(), c.Policy.CacheWaitBudget.D())
	}
	if d := c.Audit.DeleteOrphansOlderThan.D(); d < 0 || (d > 0 && d < time.Hour) {
		return fmt.Errorf("audit.delete_orphans_older_than (%s) must be 0 (report only) or at least 1h", d)
	}
	if c.Policy.PackJanitor && !c.Policy.DetachOnDelete {
		return fmt.Errorf("pack_janitor requires detach_on_delete (the arr's removal calls torrents/delete, which must keep the imported episodes)")
	}
	if c.Policy.WaitNoProgress.D() < 0 {
		return fmt.Errorf("wait_no_progress (%s) must be >= 0 (0 = off)", c.Policy.WaitNoProgress.D())
	}
	if c.Policy.MaxWaitDownloads < 0 {
		return fmt.Errorf("max_wait_downloads (%d) must be >= 0", c.Policy.MaxWaitDownloads)
	}
	switch c.Policy.RejectArchives {
	case "", "none", "only", "any":
	default:
		return fmt.Errorf("reject_archives %q must be one of none|only|any", c.Policy.RejectArchives)
	}
	if c.Policy.MinVideoMiB < 0 {
		return fmt.Errorf("min_video_mib (%d) must be >= 0", c.Policy.MinVideoMiB)
	}
	if c.Policy.RequestDLNegativeTTL.D() < 0 || c.Policy.RequestDLNegativeTTL.D() > 10*time.Minute {
		return fmt.Errorf("requestdl_negative_ttl (%s) must be 0..10m", c.Policy.RequestDLNegativeTTL.D())
	}
	if c.Policy.BackoffReadWait.D() < 0 || c.Policy.BackoffReadWait.D() > time.Minute {
		return fmt.Errorf("backoff_read_wait (%s) must be 0..1m", c.Policy.BackoffReadWait.D())
	}
	if c.Policy.ReadaheadWindows < 0 || c.Policy.ReadaheadWindows > 32 {
		return fmt.Errorf("readahead_windows (%d) must be 0..32", c.Policy.ReadaheadWindows)
	}
	if c.Policy.ReadaheadChunkMiB < 1 || c.Policy.ReadaheadChunkMiB > 64 {
		return fmt.Errorf("readahead_chunk_mib (%d) must be 1..64", c.Policy.ReadaheadChunkMiB)
	}

	// (g) extra_cdn_hosts entries must look like real multi-label host suffixes
	// ("x.y" minimum; leading dot optional). A one-label suffix like ".io" would
	// pin half the internet onto the SSRF allowlist — reject it loudly.
	for _, s := range c.TorBox.ExtraCDNHosts {
		t := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), ".")
		if t == "" || !strings.Contains(t, ".") || strings.ContainsAny(t, "/:@?#%* \t") {
			return fmt.Errorf("torbox.extra_cdn_hosts entry %q must be a host suffix like \".tb-cdn.net\"", s)
		}
	}

	if err := c.SlowLane.validate(); err != nil {
		return err
	}
	if err := c.Missing.validate(); err != nil {
		return err
	}
	if err := c.Repair.validate(c.Policy); err != nil {
		return err
	}
	if err := c.validateCleanup(); err != nil {
		return err
	}

	if n := c.TorBox.MaxRequestsPerMinute; n < 0 || n > 300 {
		return fmt.Errorf("torbox.max_requests_per_minute (%d) must be 0 (default 240) or 1..300 (TorBox allows 300 per minute per key)", n)
	}

	// (f) log_level must be a recognized name (empty = info).
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level %q must be one of debug|info|warn|error", c.LogLevel)
	}

	// (h) Workstream G: local cache + playback signal.
	if err := c.validateLocalCache(); err != nil {
		return err
	}

	return nil
}

// validate checks the slow-lane knobs. They are validated even while the lane is
// disabled so a bad value is caught when it is written, not when it is switched on.
func (sl SlowLane) validate() error {
	if sl.MaxConcurrent < 1 || sl.MaxConcurrent > 5 {
		return fmt.Errorf("slow_lane.max_concurrent (%d) must be 1..5 (each download holds a TorBox slot)", sl.MaxConcurrent)
	}
	if d := sl.PollInterval.D(); d < time.Minute || d > 24*time.Hour {
		return fmt.Errorf("slow_lane.poll_interval (%s) must be 1m..24h", d)
	}
	if d := sl.MaxAge.D(); d < 0 || (d > 0 && d < time.Hour) {
		return fmt.Errorf("slow_lane.max_age (%s) must be 0 (never give up) or at least 1h", d)
	}
	if d := sl.AutoImportAfter.D(); d < time.Minute || d > 24*time.Hour {
		return fmt.Errorf("slow_lane.auto_import_after (%s) must be 1m..24h", d)
	}
	if sl.AdoptWaitFailures && !sl.Enabled {
		return fmt.Errorf("slow_lane.adopt_wait_failures requires slow_lane.enabled")
	}
	return nil
}

// Validate exposes the post-load validation for callers that construct or mutate a
// Config in memory (the Web UI settings editor) before saving it.
func (c *Config) Validate() error { return c.validate() }

// Save atomically writes c as YAML to path (tmp file + rename, mode 0600 — the file
// holds the TorBox API key). Callers should Validate() first.
//
// Secrets that were supplied via environment variables are blanked in the written
// file (they live in the environment, not on disk).
func Save(path string, c *Config) error {
	b, err := yaml.Marshal(stripEnvSecrets(c))
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename: %w", err)
	}
	return nil
}
