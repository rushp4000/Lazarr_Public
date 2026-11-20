package config

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LocalCache configures Workstream G: the watch-aware local disk cache in front of
// the TorBox pipe. Off by default; with Enabled false every code path is byte-for-byte
// today's behaviour. See docs/28.
type LocalCache struct {
	Enabled bool `yaml:"enabled"`
	// Dir is the container path of the cache volume (e.g. /cache, bind-mounted from the
	// big host volume). Files live at <Dir>/<infohash>/<rel_path>.
	Dir string `yaml:"dir"`
	// Publish bind-mounts Dir onto PublishAt inside Lazarr's mount namespace so every
	// peer container that shares the rshared symlink root sees the cache tree at the
	// same path — the same propagation trick the FUSE mount already relies on.
	Publish bool `yaml:"publish"`
	// PublishAt is the twin of paths.fuse_mount: <PublishAt>/<hash>/<rel_path> holds
	// the real bytes; library symlinks are retargeted between the two roots.
	PublishAt string `yaml:"publish_at"`
	// LibraryRoots are the directories the arrs move imported symlinks into. Empty =
	// derive from paths.download_dir's parent (in a typical deploy that is the
	// symlink root holding Movies/ and TV Shows/).
	LibraryRoots      []string `yaml:"library_roots,omitempty"`
	LinkIndexInterval Duration `yaml:"link_index_interval"`
	// UnreferencedGrace: a cached file that no library link references any more (the
	// arr upgraded or deleted it) is evicted after this long.
	UnreferencedGrace Duration `yaml:"unreferenced_grace"`
	// TorrentOnly hard-excludes releases whose Source is not torrent (usenet bandwidth
	// is metered on TorBox). Default and strongly recommended: true.
	TorrentOnly bool `yaml:"torrent_only"`
	// Profile is a named preset for the TTL block and water marks:
	// conservative | balanced | generous. Explicit values in the YAML always win over
	// the preset (the preset is applied before the file is overlaid).
	Profile string `yaml:"profile"`
	// FreeHighPct / FreeLowPct: TTLs apply in full above FreeHighPct free space on Dir,
	// shrink linearly to zero at FreeLowPct, and below it the reaper evicts until back
	// above. ReservePct: never start a pull that would leave less than this free.
	FreeHighPct int  `yaml:"free_high_pct"`
	FreeLowPct  int  `yaml:"free_low_pct"`
	ReservePct  int  `yaml:"reserve_pct"`
	TTL         TTLs `yaml:"ttl"`
	// ActiveWindow: a series/movie counts as "being watched" this long after its last
	// playback activity by any user.
	ActiveWindow Duration `yaml:"active_window"`
	// NewReleaseWindow: a movie counts as "new" this long after its digital/physical
	// (or, failing that, theatrical) release date.
	NewReleaseWindow Duration `yaml:"new_release_window"`
	// PullOnGrab lists categories whose grabs start pulling immediately (movie arrs).
	PullOnGrab []string `yaml:"pull_on_grab,omitempty"`
	// PrefetchAhead is how many episodes to keep local ahead of each viewer's position.
	PrefetchAhead int `yaml:"prefetch_ahead"`
	// SeasonFill pulls the rest of a season once a viewer has shown commitment to it
	// (finished AfterCompleted of its episodes). Low priority, counted against
	// SpeculativeGBPerDay. Off by default; see docs/28 §17.
	SeasonFill SeasonFill `yaml:"season_fill"`
	// SpeculativeGBPerDay caps bytes pulled for speculative reasons (season fill) in
	// any rolling 24 h. Pulls for a live session (watch/prefetch) are never capped.
	// 0 = no speculative pulls at all.
	SpeculativeGBPerDay int `yaml:"speculative_gb_per_day"`
	// PullCurrent also pulls the file currently being streamed (from byte 0).
	PullCurrent        bool `yaml:"pull_current"`
	MaxConcurrentPulls int  `yaml:"max_concurrent_pulls"`
	MaxPullsPerHour    int  `yaml:"max_pulls_per_hour"`
	MinFileMiB         int  `yaml:"min_file_mib"`
	PullWindowMiB      int  `yaml:"pull_window_mib"`
	// StableMtime makes the FUSE tree report each release's grab time as mtime and
	// stamps cached files with the same value, so a retargeted link looks unchanged to
	// Plex/Jellyfin. Off by default: turning it on changes the mtime of EVERY existing
	// FUSE file once, which can make the media server re-analyse the whole library
	// (a TorBox add per never-played item). Enable on a fresh install or accept the
	// one-time scan. When off, cached files get the epoch mtime FUSE reports today.
	StableMtime bool `yaml:"stable_mtime"`
}

// SeasonFill configures "pull the rest of the season" (docs/28 §17).
type SeasonFill struct {
	Enabled bool `yaml:"enabled"`
	// AfterCompleted: how many episodes of the season this viewer must have finished
	// before the rest is pulled (measured: 2 → ~21-28% of pulled episodes go unwatched,
	// 0 = on first play → ~29-36%).
	AfterCompleted int `yaml:"after_completed"`
	// MaxEpisodes bounds one fill (a 30-episode anime season is still one request).
	MaxEpisodes int `yaml:"max_episodes"`
}

// TTLs are the base retention windows per policy class (docs/28 §7.1).
type TTLs struct {
	Watching Duration `yaml:"watching"` // 0 = pinned while active
	Finished Duration `yaml:"finished"` // every viewer completed it, nobody mid-series
	// FinishedMovie overrides Finished for movies (37% of watched movies are watched
	// again, median 12 days later). 0 = use Finished.
	FinishedMovie Duration `yaml:"finished_movie"`
	Untouched     Duration `yaml:"untouched"` // pulled (prefetch/grab) but never played
	NewMovie      Duration `yaml:"new_movie"` // movie inside NewReleaseWindow
	Default       Duration `yaml:"default"`   // everything else
}

// Profile is a named preset for LocalCache retention.
type Profile struct {
	Name          string `json:"name"`
	TTL           TTLs   `json:"-"`
	Finished      string `json:"finished"`
	FinishedMovie string `json:"finished_movie"`
	Untouched     string `json:"untouched"`
	NewMovie      string `json:"new_movie"`
	Default       string `json:"default"`
	FreeHighPct   int    `json:"free_high_pct"`
	FreeLowPct    int    `json:"free_low_pct"`
}

const (
	ProfileConservative = "conservative"
	ProfileBalanced     = "balanced"
	ProfileGenerous     = "generous"
)

const day = 24 * time.Hour

// Profiles returns the presets in display order.
func Profiles() []Profile {
	mk := func(name string, fin, finMovie, unt, nm, def time.Duration, high, low int) Profile {
		return Profile{
			Name: name,
			TTL: TTLs{
				Finished: Duration(fin), FinishedMovie: Duration(finMovie), Untouched: Duration(unt),
				NewMovie: Duration(nm), Default: Duration(def),
			},
			Finished: fin.String(), FinishedMovie: finMovie.String(), Untouched: unt.String(),
			NewMovie: nm.String(), Default: def.String(),
			FreeHighPct: high, FreeLowPct: low,
		}
	}
	return []Profile{
		mk(ProfileConservative, 24*time.Hour, 14*day, 7*day, 90*day, 14*day, 30, 10),
		mk(ProfileBalanced, 48*time.Hour, 30*day, 14*day, 180*day, 30*day, 20, 5),
		mk(ProfileGenerous, 7*day, 60*day, 30*day, 365*day, 60*day, 15, 5),
	}
}

// profileByName returns the preset or ok=false.
func profileByName(name string) (Profile, bool) {
	for _, p := range Profiles() {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// applyProfile overwrites the TTL block and water marks with the preset. Called on
// the defaults BEFORE the YAML overlay so explicit values in the file still win.
func (lc *LocalCache) applyProfile(name string) error {
	if name == "" {
		return nil
	}
	p, ok := profileByName(name)
	if !ok {
		return fmt.Errorf("local_cache.profile %q must be one of conservative|balanced|generous", name)
	}
	lc.Profile = name
	lc.TTL.Finished = p.TTL.Finished
	lc.TTL.FinishedMovie = p.TTL.FinishedMovie
	lc.TTL.Untouched = p.TTL.Untouched
	lc.TTL.NewMovie = p.TTL.NewMovie
	lc.TTL.Default = p.TTL.Default
	lc.FreeHighPct = p.FreeHighPct
	lc.FreeLowPct = p.FreeLowPct
	return nil
}

// defaultLocalCache is the balanced preset with the plumbing defaults from docs/28 §6.
func defaultLocalCache() LocalCache {
	lc := LocalCache{
		Enabled:             false,
		Dir:                 "/cache",
		Publish:             true,
		PublishAt:           "",
		LinkIndexInterval:   Duration(10 * time.Minute),
		UnreferencedGrace:   Duration(time.Hour),
		TorrentOnly:         true,
		Profile:             ProfileBalanced,
		ReservePct:          2,
		TTL:                 TTLs{Watching: 0},
		ActiveWindow:        Duration(72 * time.Hour),
		NewReleaseWindow:    Duration(270 * day),
		PrefetchAhead:       2,
		SeasonFill:          SeasonFill{Enabled: false, AfterCompleted: 2, MaxEpisodes: 30},
		SpeculativeGBPerDay: 250,
		PullCurrent:         true,
		MaxConcurrentPulls:  1,
		MaxPullsPerHour:     20,
		MinFileMiB:          200,
		PullWindowMiB:       8,
		StableMtime:         false,
	}
	_ = lc.applyProfile(ProfileBalanced)
	return lc
}

// Playback configures the media-server and arr clients that feed the local cache's
// planner (Workstream G). Secrets are normally supplied via environment variables
// (LAZARR_PLEX_TOKEN, LAZARR_JELLYFIN_API_KEY, LAZARR_ARR_API_KEY_<CATEGORY>); an
// env value always overrides the YAML and is never written back to the file.
type Playback struct {
	// PollInterval is how often sessions are polled. 0 disables polling.
	PollInterval Duration `yaml:"poll_interval"`
	// WatchedPct: progress at/after which a viewing counts as completed (Plex scrobbles at 90).
	WatchedPct int `yaml:"watched_pct"`
	// PathMap rewrites a media server's reported path prefix to Lazarr's view when the
	// containers mount the shared root at different paths. Usually empty.
	PathMap []PathMap `yaml:"path_map,omitempty"`
	// RefreshOnRetarget asks the media server to refresh the item after a link retarget.
	// Not needed with automatic library scanning; see docs/28 §4.6.
	RefreshOnRetarget bool     `yaml:"refresh_on_retarget"`
	Plex              Plex     `yaml:"plex"`
	Jellyfin          Jellyfin `yaml:"jellyfin"`
	Arrs              []Arr    `yaml:"arrs,omitempty"`
	WebhookListen     string   `yaml:"webhook_listen"` // Phase 3; reserved
	// Analysis asks the media server to run its own file analysis (intro/credits
	// markers, loudness, thumbnails) while a file sits in the local cache, so those
	// whole-file reads cost no CDN bandwidth. Off by default; see docs/28 §16.
	Analysis Analysis  `yaml:"analysis"`
	EnvSet   EnvSecret `yaml:"-"` // which secrets came from the environment
}

// EnvSecret records which secret fields were supplied by environment variables so
// the settings editor can show "set via environment" and Save never persists them.
type EnvSecret struct {
	TorBoxAPIKey   bool
	PlexToken      bool
	JellyfinAPIKey bool
	ArrAPIKeys     map[string]bool // by category
	// Ownership is true when PUID/PGID (or LAZARR_PUID/LAZARR_PGID) supplied ownership;
	// FileOwnership keeps the YAML values so Save writes those back, not the environment's.
	Ownership     bool
	FileOwnership Ownership
	ownershipErr  error // malformed ownership env value, reported by validate
}

type PathMap struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type Plex struct {
	URL   string `yaml:"url"`
	Token string `yaml:"token"`
}

type Jellyfin struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
}

// Arr is one Radarr/Sonarr instance keyed by its Lazarr category. Kind is inferred
// from the category name when empty ("sonarr*" → sonarr, else radarr).
type Arr struct {
	Category string `yaml:"category"`
	URL      string `yaml:"url"`
	APIKey   string `yaml:"api_key"`
	Kind     string `yaml:"kind,omitempty"` // radarr | sonarr
	// AlsoCategories are further Lazarr categories this same arr instance grabs with,
	// e.g. "sonarr_main_seerr" for a second "Lazarr Seerr" download client on sonarr_main
	// (per-category on_cache_miss). Lookups by any of them (wait_fail_blocklist,
	// release dates, the pack janitor) reach this arr with this API key.
	AlsoCategories []string `yaml:"also_categories,omitempty"`
	// LibraryRoot is the arr's root folder AS LAZARR SEES IT, e.g.
	// "/data/TV Shows/sonarr_main" for an arr whose root folder is "/tv". The arrs report
	// paths in their own container's namespace; this maps them (rescans after removed
	// library links) and bounds the empty-folder cleanup (library folders are only
	// removed strictly below it). Empty = auto-detected for rescans (see
	// internal/arrroots) and no library cleanup for this arr.
	LibraryRoot string `yaml:"library_root,omitempty"`
	// NoLibrarySweep keeps cleanup.sweep_library away from this arr's root (pruning the
	// folders the repair sweeps empty still applies). An arr with "Create empty
	// series/movie folders" on is skipped automatically.
	NoLibrarySweep bool `yaml:"no_library_sweep,omitempty"`
}

// Categories returns the arr's primary category followed by AlsoCategories.
func (a Arr) Categories() []string {
	return append([]string{a.Category}, a.AlsoCategories...)
}

// EffectiveKind returns radarr|sonarr, inferring from the category when unset.
func (a Arr) EffectiveKind() string {
	if a.Kind != "" {
		return a.Kind
	}
	if strings.HasPrefix(strings.ToLower(a.Category), "sonarr") {
		return "sonarr"
	}
	return "radarr"
}

// CategoryKind returns what a qBit category imports: "sonarr" or "radarr" for an arr in
// playback.arrs (by any of its categories), else inferred from a "sonarr…" / "radarr…"
// name, else "" (unknown).
func (c *Config) CategoryKind(category string) string {
	for _, a := range c.Playback.Arrs {
		if slices.Contains(a.Categories(), category) {
			return strings.ToLower(a.EffectiveKind())
		}
	}
	switch lc := strings.ToLower(category); {
	case strings.HasPrefix(lc, "sonarr"):
		return "sonarr"
	case strings.HasPrefix(lc, "radarr"):
		return "radarr"
	}
	return ""
}

// Analysis configures post-pull media-server analysis (docs/28 §16).
type Analysis struct {
	// Enabled turns the hook on. Off by default.
	Enabled bool `yaml:"enabled"`
	// Actions are what to ask the server for, in order. Plex: analyze (media analysis
	// incl. loudness — a whole-file read), intro, credits, voice_activity,
	// chapter_thumbs, index (video preview thumbnails). Plex ignores intro/credits/
	// index/chapter_thumbs while the matching server-wide "Generate …" setting is "never".
	Actions []string `yaml:"actions,omitempty"`
	// Timeout bounds one item's analysis (wall clock, from the request).
	Timeout Duration `yaml:"timeout"`
	// MaxHold is the longest a pending analysis keeps its file from TTL eviction.
	MaxHold Duration `yaml:"max_hold"`
	// ScanWait is how long to wait for the server to index a freshly imported file
	// before giving up (Lazarr never triggers a library scan itself).
	ScanWait Duration `yaml:"scan_wait"`
	// PollInterval is how often a running analysis is checked.
	PollInterval Duration `yaml:"poll_interval"`
	// OpenGates lets Lazarr flip the server's own gate for intro/credits detection
	// (Plex: Generate{Intro,Credits}MarkerBehavior never → scheduled) for the duration
	// of one request and put it back afterwards, so those detectors run only for
	// local files instead of nightly over the whole (FUSE) library. Lazarr owns those
	// two settings while this is on: it resets them to "never" at every start.
	OpenGates bool `yaml:"open_gates"`
	// AvoidHours is a local-time window ("HH:MM-HH:MM") in which gates are never
	// opened and any open gate is closed — keep it around the server's maintenance
	// window (Plex butler: 02:00-05:00 by default).
	AvoidHours string `yaml:"avoid_hours"`
}

// ParseHourWindow parses "HH:MM-HH:MM" into minutes after midnight (may wrap).
func ParseHourWindow(s string) (start, end int, err error) {
	var h1, m1, h2, m2 int
	if _, err := fmt.Sscanf(s, "%d:%d-%d:%d", &h1, &m1, &h2, &m2); err != nil {
		return 0, 0, fmt.Errorf("window %q must look like HH:MM-HH:MM", s)
	}
	for _, v := range [][2]int{{h1, m1}, {h2, m2}} {
		if v[0] < 0 || v[0] > 23 || v[1] < 0 || v[1] > 59 {
			return 0, 0, fmt.Errorf("window %q has an invalid time", s)
		}
	}
	return h1*60 + m1, h2*60 + m2, nil
}

// AnalysisActions is the set of accepted playback.analysis.actions values.
var AnalysisActions = []string{"analyze", "intro", "credits", "voice_activity", "chapter_thumbs", "index"}

func defaultPlayback() Playback {
	return Playback{
		PollInterval: Duration(30 * time.Second),
		WatchedPct:   90,
		Analysis: Analysis{
			Actions:      []string{"intro", "credits"},
			Timeout:      Duration(2 * time.Hour),
			MaxHold:      Duration(6 * time.Hour),
			ScanWait:     Duration(30 * time.Minute),
			PollInterval: Duration(30 * time.Second),
			AvoidHours:   "01:30-05:30",
		},
	}
}

// EnvPrefix is the environment-variable namespace for secret overrides.
const EnvPrefix = "LAZARR_"

// ArrEnvKey returns the env var name carrying the API key for a category, e.g.
// LAZARR_ARR_API_KEY_RADARR_MAIN. Non-alphanumerics become '_'.
func ArrEnvKey(category string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(category) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return EnvPrefix + "ARR_API_KEY_" + b.String()
}

// ApplyEnv overlays environment-supplied secrets onto c and records their provenance.
// lookup is os.LookupEnv in production; tests pass a map. An arr key whose category is
// not declared in playback.arrs is ignored (nothing to attach it to) — the operator
// declares the instance in YAML/UI and only the secret comes from the environment.
func ApplyEnv(c *Config, lookup func(string) (string, bool)) {
	c.Playback.EnvSet = EnvSecret{ArrAPIKeys: map[string]bool{}}
	if v, ok := lookup(EnvPrefix + "TORBOX_API_KEY"); ok && v != "" {
		c.TorBox.APIKey = v
		c.Playback.EnvSet.TorBoxAPIKey = true
	}
	if v, ok := lookup(EnvPrefix + "PLEX_TOKEN"); ok && v != "" {
		c.Playback.Plex.Token = v
		c.Playback.EnvSet.PlexToken = true
	}
	if v, ok := lookup(EnvPrefix + "JELLYFIN_API_KEY"); ok && v != "" {
		c.Playback.Jellyfin.APIKey = v
		c.Playback.EnvSet.JellyfinAPIKey = true
	}
	for i := range c.Playback.Arrs {
		if v, ok := lookup(ArrEnvKey(c.Playback.Arrs[i].Category)); ok && v != "" {
			c.Playback.Arrs[i].APIKey = v
			c.Playback.EnvSet.ArrAPIKeys[c.Playback.Arrs[i].Category] = true
		}
	}
	applyOwnershipEnv(c, lookup)
}

// applyOwnershipEnv lets the container's PUID/PGID (the linuxserver.io convention the arrs
// already use) set ownership, with LAZARR_PUID/LAZARR_PGID taking precedence. The YAML
// values are kept in EnvSet.FileOwnership so Save writes them back. A malformed value is
// reported by validate instead of being silently ignored.
func applyOwnershipEnv(c *Config, lookup func(string) (string, bool)) {
	es := &c.Playback.EnvSet
	es.FileOwnership = c.Ownership
	for _, f := range []struct {
		dst  *int
		keys [2]string
	}{
		{&c.Ownership.PUID, [2]string{EnvPrefix + "PUID", "PUID"}},
		{&c.Ownership.PGID, [2]string{EnvPrefix + "PGID", "PGID"}},
	} {
		for _, k := range f.keys {
			v, ok := lookup(k)
			if !ok || v == "" {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				es.ownershipErr = fmt.Errorf("environment %s=%q must be a non-negative integer", k, v)
			} else {
				*f.dst = n
				es.Ownership = true
			}
			break
		}
	}
}

// stripEnvSecrets returns a copy of c with every environment-sourced secret blanked,
// so Save never persists a value that only ever lived in the environment.
func stripEnvSecrets(c *Config) *Config {
	cp := *c
	cp.Playback.Arrs = append([]Arr(nil), c.Playback.Arrs...)
	es := c.Playback.EnvSet
	if es.TorBoxAPIKey {
		cp.TorBox.APIKey = ""
	}
	if es.PlexToken {
		cp.Playback.Plex.Token = ""
	}
	if es.JellyfinAPIKey {
		cp.Playback.Jellyfin.APIKey = ""
	}
	if es.Ownership {
		cp.Ownership = es.FileOwnership
	}
	for i := range cp.Playback.Arrs {
		if es.ArrAPIKeys[cp.Playback.Arrs[i].Category] {
			cp.Playback.Arrs[i].APIKey = ""
		}
	}
	return &cp
}

// validateLocalCache is the Workstream G slice of Config.validate.
func (c *Config) validateLocalCache() error {
	lc := &c.LocalCache
	pb := &c.Playback

	if lc.Profile != "" {
		if _, ok := profileByName(lc.Profile); !ok {
			return fmt.Errorf("local_cache.profile %q must be one of conservative|balanced|generous", lc.Profile)
		}
	}
	if lc.FreeLowPct < 0 || lc.FreeHighPct > 100 || lc.FreeLowPct >= lc.FreeHighPct {
		return fmt.Errorf("local_cache.free_low_pct (%d) must be >= 0 and < free_high_pct (%d) <= 100", lc.FreeLowPct, lc.FreeHighPct)
	}
	if lc.ReservePct < 0 || lc.ReservePct > 50 {
		return fmt.Errorf("local_cache.reserve_pct (%d) must be 0..50", lc.ReservePct)
	}
	for name, d := range map[string]Duration{
		"watching": lc.TTL.Watching, "finished": lc.TTL.Finished, "untouched": lc.TTL.Untouched,
		"new_movie": lc.TTL.NewMovie, "default": lc.TTL.Default,
		"active_window": lc.ActiveWindow, "new_release_window": lc.NewReleaseWindow,
		"unreferenced_grace": lc.UnreferencedGrace, "link_index_interval": lc.LinkIndexInterval,
	} {
		if d.D() < 0 {
			return fmt.Errorf("local_cache.%s must be >= 0", name)
		}
	}
	if lc.SeasonFill.AfterCompleted < 0 || lc.SeasonFill.AfterCompleted > 20 {
		return fmt.Errorf("local_cache.season_fill.after_completed (%d) must be 0..20", lc.SeasonFill.AfterCompleted)
	}
	if lc.SeasonFill.Enabled && (lc.SeasonFill.MaxEpisodes < 1 || lc.SeasonFill.MaxEpisodes > 100) {
		return fmt.Errorf("local_cache.season_fill.max_episodes (%d) must be 1..100", lc.SeasonFill.MaxEpisodes)
	}
	if lc.SpeculativeGBPerDay < 0 {
		return fmt.Errorf("local_cache.speculative_gb_per_day must be >= 0")
	}
	if lc.TTL.FinishedMovie.D() < 0 {
		return fmt.Errorf("local_cache.ttl.finished_movie must be >= 0")
	}
	if lc.PrefetchAhead < 0 || lc.PrefetchAhead > 20 {
		return fmt.Errorf("local_cache.prefetch_ahead (%d) must be 0..20", lc.PrefetchAhead)
	}
	if lc.MaxConcurrentPulls < 1 || lc.MaxConcurrentPulls > 8 {
		return fmt.Errorf("local_cache.max_concurrent_pulls (%d) must be 1..8", lc.MaxConcurrentPulls)
	}
	if lc.MaxPullsPerHour < 1 {
		return fmt.Errorf("local_cache.max_pulls_per_hour (%d) must be >= 1", lc.MaxPullsPerHour)
	}
	if lc.MinFileMiB < 0 {
		return fmt.Errorf("local_cache.min_file_mib (%d) must be >= 0", lc.MinFileMiB)
	}
	if lc.PullWindowMiB < 1 || lc.PullWindowMiB > 64 {
		return fmt.Errorf("local_cache.pull_window_mib (%d) must be 1..64", lc.PullWindowMiB)
	}
	cats := make(map[string]bool, len(c.Categories))
	for _, cat := range c.Categories {
		cats[cat] = true
	}
	for _, cat := range lc.PullOnGrab {
		if !cats[cat] {
			return fmt.Errorf("local_cache.pull_on_grab entry %q is not a configured category", cat)
		}
	}
	seen := map[string]bool{}
	for _, a := range pb.Arrs {
		if !cats[a.Category] {
			return fmt.Errorf("playback.arrs entry %q is not a configured category", a.Category)
		}
		for _, cat := range a.Categories() {
			if !cats[cat] {
				return fmt.Errorf("playback.arrs[%s].also_categories entry %q is not a configured category", a.Category, cat)
			}
			if seen[cat] {
				return fmt.Errorf("playback.arrs lists category %q twice", cat)
			}
			seen[cat] = true
		}
		if err := validateHTTPURL("playback.arrs["+a.Category+"].url", a.URL); err != nil {
			return err
		}
		switch a.Kind {
		case "", "radarr", "sonarr":
		default:
			return fmt.Errorf("playback.arrs[%s].kind %q must be radarr|sonarr", a.Category, a.Kind)
		}
	}
	if pb.Plex.URL != "" {
		if err := validateHTTPURL("playback.plex.url", pb.Plex.URL); err != nil {
			return err
		}
	}
	if pb.Jellyfin.URL != "" {
		if err := validateHTTPURL("playback.jellyfin.url", pb.Jellyfin.URL); err != nil {
			return err
		}
	}
	if pb.WatchedPct < 50 || pb.WatchedPct > 100 {
		return fmt.Errorf("playback.watched_pct (%d) must be 50..100", pb.WatchedPct)
	}
	if pb.PollInterval.D() < 0 {
		return fmt.Errorf("playback.poll_interval must be >= 0")
	}
	if pb.PollInterval.D() > 0 && pb.PollInterval.D() < 5*time.Second {
		return fmt.Errorf("playback.poll_interval (%s) must be >= 5s (or 0 to disable)", pb.PollInterval.D())
	}
	for _, m := range pb.PathMap {
		if m.From == "" || m.To == "" {
			return fmt.Errorf("playback.path_map entries need both from and to")
		}
	}
	if err := validateAnalysis(pb.Analysis); err != nil {
		return err
	}

	if !lc.Enabled {
		return nil
	}
	// Everything below only matters when the cache is on.
	if lc.Dir == "" {
		return fmt.Errorf("local_cache.dir is required when local_cache.enabled is true")
	}
	if c.Paths.FuseMount == "" {
		return fmt.Errorf("fuse_mount is required when local_cache.enabled is true")
	}
	publishAt := c.EffectivePublishAt()
	if publishAt == "" {
		return fmt.Errorf("local_cache.publish_at is required when local_cache.enabled is true")
	}
	if !sameParent(publishAt, c.Paths.FuseMount) {
		return fmt.Errorf("local_cache.publish_at (%s) must be a sibling of fuse_mount (%s)", publishAt, c.Paths.FuseMount)
	}
	// The cache dir is published (bind-mounted, as root) into the shared tree that every
	// peer container sees. It must therefore never contain Lazarr's own private state —
	// the catalog, the probe cache — nor be an ancestor of them, or a settings change
	// could expose the database (and, via the config dir, the TorBox key) to every
	// container and the host.
	for name, priv := range map[string]string{"db_path": c.Paths.DBPath, "probe_cache_dir": c.Paths.ProbeCacheDir, "the config file": c.ConfigDir} {
		if priv != "" && underOrEqual(priv, lc.Dir) {
			return fmt.Errorf("local_cache.dir (%s) must not contain %s (%s)", lc.Dir, name, priv)
		}
	}
	if lc.Dir == "/" {
		return fmt.Errorf("local_cache.dir must not be /")
	}
	// With publish on, dir is bind-mounted ONTO publish_at, so the two must be
	// disjoint. With publish off the operator mounts the volume at publish_at in every
	// container themselves and dir is normally the same path.
	if lc.Publish && (underOrEqual(lc.Dir, publishAt) || underOrEqual(publishAt, lc.Dir)) {
		return fmt.Errorf("local_cache.publish_at (%s) and local_cache.dir (%s) must not nest while publish is on", publishAt, lc.Dir)
	}
	if c.Paths.DownloadDir != "" && (underOrEqual(publishAt, c.Paths.DownloadDir) || underOrEqual(c.Paths.DownloadDir, publishAt)) {
		return fmt.Errorf("local_cache.publish_at (%s) must not nest with download_dir (%s)", publishAt, c.Paths.DownloadDir)
	}
	hasSignal := pb.Plex.URL != "" || pb.Jellyfin.URL != ""
	if !hasSignal && len(lc.PullOnGrab) == 0 {
		return fmt.Errorf("local_cache.enabled needs playback.plex.url or playback.jellyfin.url, or a non-empty local_cache.pull_on_grab — otherwise nothing can ever fill the cache")
	}
	if pb.Plex.URL != "" && pb.Plex.Token == "" {
		return fmt.Errorf("playback.plex.url is set but no token: set LAZARR_PLEX_TOKEN (or playback.plex.token)")
	}
	if pb.Jellyfin.URL != "" && pb.Jellyfin.APIKey == "" {
		return fmt.Errorf("playback.jellyfin.url is set but no API key: set LAZARR_JELLYFIN_API_KEY (or playback.jellyfin.api_key)")
	}
	return nil
}

// EffectivePublishAt returns local_cache.publish_at, defaulting to a "lazarr_cache"
// sibling of paths.fuse_mount.
func (c *Config) EffectivePublishAt() string {
	if c.LocalCache.PublishAt != "" {
		return c.LocalCache.PublishAt
	}
	if c.Paths.FuseMount == "" {
		return ""
	}
	return parentOf(c.Paths.FuseMount) + "/lazarr_cache"
}

// EffectiveLibraryRoots returns local_cache.library_roots, defaulting to the parent
// of paths.download_dir (the symlink root the arrs import into on the reference
// deploy). The download dir itself is always indexed too, so it is included.
func (c *Config) EffectiveLibraryRoots() []string {
	roots := append([]string(nil), c.LocalCache.LibraryRoots...)
	if len(roots) == 0 && c.Paths.DownloadDir != "" {
		roots = []string{parentOf(c.Paths.DownloadDir)}
	}
	sort.Strings(roots)
	return roots
}

func parentOf(p string) string {
	p = strings.TrimRight(p, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func sameParent(a, b string) bool {
	return parentOf(a) == parentOf(b)
}

func underOrEqual(child, root string) bool {
	child = strings.TrimRight(child, "/")
	root = strings.TrimRight(root, "/")
	return child == root || strings.HasPrefix(child, root+"/")
}

func validateHTTPURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q must be an http(s) URL", field, raw)
	}
	return nil
}

func validateAnalysis(a Analysis) error {
	for _, act := range a.Actions {
		if !slices.Contains(AnalysisActions, act) {
			return fmt.Errorf("playback.analysis.actions: %q is not one of %v", act, AnalysisActions)
		}
	}
	if a.Enabled && len(a.Actions) == 0 {
		return fmt.Errorf("playback.analysis.enabled needs at least one action")
	}
	for name, d := range map[string]Duration{"timeout": a.Timeout, "max_hold": a.MaxHold, "scan_wait": a.ScanWait} {
		if d.D() < 0 {
			return fmt.Errorf("playback.analysis.%s must be >= 0", name)
		}
	}
	if a.Enabled && a.PollInterval.D() < 5*time.Second {
		return fmt.Errorf("playback.analysis.poll_interval (%s) must be >= 5s", a.PollInterval.D())
	}
	if a.AvoidHours != "" {
		if _, _, err := ParseHourWindow(a.AvoidHours); err != nil {
			return fmt.Errorf("playback.analysis.avoid_hours: %w", err)
		}
	}
	if a.Enabled && a.MaxHold.D() < a.Timeout.D() {
		return fmt.Errorf("playback.analysis.max_hold (%s) must be >= timeout (%s)", a.MaxHold.D(), a.Timeout.D())
	}
	return nil
}
