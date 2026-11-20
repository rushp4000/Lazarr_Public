package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDurationDays(t *testing.T) {
	cases := map[string]time.Duration{
		"270d": 270 * 24 * time.Hour,
		"1.5d": 36 * time.Hour,
		"48h":  48 * time.Hour,
		"90m":  90 * time.Minute,
		"0":    0,
	}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %s want %s", in, got, want)
		}
	}
	if _, err := ParseDuration("xd"); err == nil {
		t.Fatal("expected error for xd")
	}
}

func TestDefaultLocalCacheIsBalanced(t *testing.T) {
	c := Default()
	if c.LocalCache.Enabled {
		t.Fatal("cache must default to disabled")
	}
	if c.LocalCache.Profile != ProfileBalanced {
		t.Fatalf("profile %q", c.LocalCache.Profile)
	}
	if c.LocalCache.TTL.Finished.D() != 48*time.Hour || c.LocalCache.TTL.NewMovie.D() != 180*24*time.Hour {
		t.Fatalf("balanced preset not applied: %+v", c.LocalCache.TTL)
	}
	if c.LocalCache.FreeHighPct != 20 || c.LocalCache.FreeLowPct != 5 {
		t.Fatalf("balanced water marks not applied")
	}
	if !c.LocalCache.TorrentOnly {
		t.Fatal("torrent_only must default to true")
	}
	if err := c.validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

// The profile preset is applied before the YAML overlay, so an explicit ttl wins.
func TestLoadProfileThenExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
torbox: {api_key: "k"}
categories: [radarr_main]
paths: {download_dir: /d/lazarr, fuse_mount: /d/lazarr_mount}
local_cache:
  profile: generous
  ttl:
    finished: "12h"
  new_release_window: "300d"
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalCache.TTL.Finished.D() != 12*time.Hour {
		t.Fatalf("explicit finished lost: %s", c.LocalCache.TTL.Finished.D())
	}
	if c.LocalCache.TTL.Untouched.D() != 30*24*time.Hour {
		t.Fatalf("generous untouched not applied: %s", c.LocalCache.TTL.Untouched.D())
	}
	if c.LocalCache.FreeHighPct != 15 {
		t.Fatalf("generous free_high_pct not applied: %d", c.LocalCache.FreeHighPct)
	}
	if c.LocalCache.NewReleaseWindow.D() != 300*24*time.Hour {
		t.Fatalf("days suffix not parsed: %s", c.LocalCache.NewReleaseWindow.D())
	}
	if c.ConfigDir != dir {
		t.Fatalf("ConfigDir = %q, want %q", c.ConfigDir, dir)
	}
	if c.EffectivePublishAt() != "/d/lazarr_cache" {
		t.Fatalf("publish_at default: %s", c.EffectivePublishAt())
	}
	if roots := c.EffectiveLibraryRoots(); len(roots) != 1 || roots[0] != "/d" {
		t.Fatalf("library roots default: %v", roots)
	}
}

func TestLoadUnknownProfileFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("local_cache: {profile: turbo}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("expected profile error, got %v", err)
	}
}

func TestApplyEnvAndSaveStripsSecrets(t *testing.T) {
	c := validConfig()
	c.Categories = []string{"radarr_main", "sonarr_main"}
	c.Playback.Arrs = []Arr{{Category: "radarr_main", URL: "http://radarr:7878"}, {Category: "sonarr_main", URL: "http://sonarr:8989", APIKey: "from-yaml"}}
	env := map[string]string{
		"LAZARR_PLEX_TOKEN":              "plex-secret",
		"LAZARR_JELLYFIN_API_KEY":        "jf-secret",
		"LAZARR_ARR_API_KEY_RADARR_MAIN": "radarr-secret",
		"LAZARR_TORBOX_API_KEY":          "tb-secret",
	}
	ApplyEnv(c, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if c.Playback.Plex.Token != "plex-secret" || !c.Playback.EnvSet.PlexToken {
		t.Fatal("plex token not applied")
	}
	if c.Playback.Jellyfin.APIKey != "jf-secret" || !c.Playback.EnvSet.JellyfinAPIKey {
		t.Fatal("jellyfin key not applied")
	}
	if c.Playback.Arrs[0].APIKey != "radarr-secret" || !c.Playback.EnvSet.ArrAPIKeys["radarr_main"] {
		t.Fatal("arr key not applied")
	}
	if c.Playback.Arrs[1].APIKey != "from-yaml" || c.Playback.EnvSet.ArrAPIKeys["sonarr_main"] {
		t.Fatal("yaml arr key must survive untouched")
	}
	if c.TorBox.APIKey != "tb-secret" || !c.Playback.EnvSet.TorBoxAPIKey {
		t.Fatal("torbox key not applied")
	}
	if ArrEnvKey("radarr-4k.x") != "LAZARR_ARR_API_KEY_RADARR_4K_X" {
		t.Fatalf("ArrEnvKey: %s", ArrEnvKey("radarr-4k.x"))
	}

	// Save must not persist any env-sourced secret; the YAML-sourced one stays.
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	out := string(b)
	for _, secret := range []string{"plex-secret", "jf-secret", "radarr-secret", "tb-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("env secret %q leaked into config file", secret)
		}
	}
	if !strings.Contains(out, "from-yaml") {
		t.Fatal("yaml-sourced key must be preserved")
	}
	// The in-memory config is untouched by Save.
	if c.Playback.Plex.Token != "plex-secret" {
		t.Fatal("Save mutated the live config")
	}
}

func TestApplyEnvOwnership(t *testing.T) {
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}

	c := validConfig()
	c.Ownership = Ownership{PUID: 500, PGID: 500}
	ApplyEnv(c, lookup(map[string]string{"PUID": "1001", "PGID": "1004"}))
	if c.Ownership != (Ownership{PUID: 1001, PGID: 1004}) || !c.Playback.EnvSet.Ownership {
		t.Fatalf("PUID/PGID not applied: %+v", c.Ownership)
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if out := string(b); !strings.Contains(out, "puid: 500") || strings.Contains(out, "1001") {
		t.Fatalf("Save must keep the YAML ownership, got:\n%s", out)
	}

	c = validConfig()
	ApplyEnv(c, lookup(map[string]string{"PUID": "1001", "LAZARR_PUID": "2000", "PGID": "2000"}))
	if c.Ownership.PUID != 2000 {
		t.Fatalf("LAZARR_PUID must win over PUID: %d", c.Ownership.PUID)
	}

	c = validConfig()
	c.Ownership = Ownership{PUID: 500, PGID: 500}
	ApplyEnv(c, lookup(nil))
	if c.Playback.EnvSet.Ownership || c.Ownership.PUID != 500 {
		t.Fatal("no env must leave YAML ownership untouched")
	}

	c = validConfig()
	ApplyEnv(c, lookup(map[string]string{"PUID": "abc", "PGID": "1001"}))
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "PUID") {
		t.Fatalf("malformed PUID must fail validation, got %v", err)
	}
}

func TestValidateLocalCache(t *testing.T) {
	enabled := func(c *Config) {
		c.LocalCache.Enabled = true
		c.LocalCache.Dir = "/cache"
		c.Playback.Jellyfin.URL = "http://jf:8096"
		c.Playback.Jellyfin.APIKey = "k"
	}
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{name: "disabled defaults", mutate: func(*Config) {}},
		{name: "enabled minimal", mutate: enabled},
		{name: "enabled, grab-only signal", mutate: func(c *Config) {
			c.LocalCache.Enabled = true
			c.LocalCache.Dir = "/cache"
			c.LocalCache.PullOnGrab = []string{"radarr_hq"}
		}},
		{name: "dir contains the catalog", mutate: func(c *Config) { enabled(c); c.Paths.DBPath = "/config/db.sqlite"; c.LocalCache.Dir = "/config" }, wantErr: "must not contain db_path"},
		{name: "dir is an ancestor of the probe cache", mutate: func(c *Config) { enabled(c); c.Paths.ProbeCacheDir = "/data/probe"; c.LocalCache.Dir = "/data" }, wantErr: "must not contain probe_cache_dir"},
		{name: "dir contains the config file", mutate: func(c *Config) { enabled(c); c.ConfigDir = "/config"; c.LocalCache.Dir = "/config" }, wantErr: "must not contain the config file"},
		{name: "dir beside the catalog is fine", mutate: func(c *Config) { enabled(c); c.Paths.DBPath = "/config/db.sqlite"; c.LocalCache.Dir = "/config/cache" }},
		{name: "dir is root", mutate: func(c *Config) { enabled(c); c.Paths.DBPath = "/x/db.sqlite"; c.LocalCache.Dir = "/" }, wantErr: "must not"},
		{name: "enabled without dir", mutate: func(c *Config) { enabled(c); c.LocalCache.Dir = "" }, wantErr: "local_cache.dir is required"},
		{name: "enabled without any signal", mutate: func(c *Config) {
			c.LocalCache.Enabled = true
			c.LocalCache.Dir = "/cache"
		}, wantErr: "nothing can ever fill"},
		{name: "plex url without token", mutate: func(c *Config) { enabled(c); c.Playback.Plex.URL = "http://plex:32400" }, wantErr: "LAZARR_PLEX_TOKEN"},
		{name: "jellyfin url without key", mutate: func(c *Config) { enabled(c); c.Playback.Jellyfin.APIKey = "" }, wantErr: "LAZARR_JELLYFIN_API_KEY"},
		{name: "publish_at not sibling", mutate: func(c *Config) { enabled(c); c.LocalCache.PublishAt = "/elsewhere/cache" }, wantErr: "sibling"},
		{name: "publish_at nests dir", mutate: func(c *Config) {
			enabled(c)
			c.LocalCache.Dir = "/mnt/lazarr_cache/x"
			c.LocalCache.PublishAt = "/mnt/lazarr_cache"
		}, wantErr: "must not nest"},
		{name: "pull_on_grab unknown category", mutate: func(c *Config) { c.LocalCache.PullOnGrab = []string{"nope"} }, wantErr: "not a configured category"},
		{name: "arr unknown category", mutate: func(c *Config) { c.Playback.Arrs = []Arr{{Category: "nope", URL: "http://x"}} }, wantErr: "not a configured category"},
		{name: "arr bad url", mutate: func(c *Config) { c.Playback.Arrs = []Arr{{Category: "radarr_hq", URL: "radarr:7878"}} }, wantErr: "http(s) URL"},
		{name: "arr dup", mutate: func(c *Config) {
			c.Playback.Arrs = []Arr{{Category: "radarr_hq", URL: "http://a"}, {Category: "radarr_hq", URL: "http://b"}}
		}, wantErr: "twice"},
		{name: "arr bad kind", mutate: func(c *Config) { c.Playback.Arrs = []Arr{{Category: "radarr_hq", URL: "http://a", Kind: "lidarr"}} }, wantErr: "radarr|sonarr"},
		{name: "plex bad url", mutate: func(c *Config) { c.Playback.Plex.URL = "ftp://plex" }, wantErr: "http(s) URL"},
		{name: "water marks inverted", mutate: func(c *Config) { c.LocalCache.FreeLowPct = 30; c.LocalCache.FreeHighPct = 20 }, wantErr: "free_low_pct"},
		{name: "negative ttl", mutate: func(c *Config) { c.LocalCache.TTL.Finished = Duration(-time.Hour) }, wantErr: "must be >= 0"},
		{name: "prefetch too high", mutate: func(c *Config) { c.LocalCache.PrefetchAhead = 21 }, wantErr: "prefetch_ahead"},
		{name: "zero pulls", mutate: func(c *Config) { c.LocalCache.MaxConcurrentPulls = 0 }, wantErr: "max_concurrent_pulls"},
		{name: "poll too fast", mutate: func(c *Config) { c.Playback.PollInterval = Duration(time.Second) }, wantErr: "poll_interval"},
		{name: "poll off ok", mutate: func(c *Config) { c.Playback.PollInterval = 0 }},
		{name: "watched pct", mutate: func(c *Config) { c.Playback.WatchedPct = 10 }, wantErr: "watched_pct"},
		{name: "path_map half", mutate: func(c *Config) { c.Playback.PathMap = []PathMap{{From: "/a"}} }, wantErr: "path_map"},
		{name: "bad profile", mutate: func(c *Config) { c.LocalCache.Profile = "turbo" }, wantErr: "profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestArrEffectiveKind(t *testing.T) {
	if (Arr{Category: "sonarr_4k"}).EffectiveKind() != "sonarr" {
		t.Fatal("sonarr inference")
	}
	if (Arr{Category: "radarr_main"}).EffectiveKind() != "radarr" {
		t.Fatal("radarr inference")
	}
	if (Arr{Category: "tv", Kind: "sonarr"}).EffectiveKind() != "sonarr" {
		t.Fatal("explicit kind")
	}
}

func TestAnalysisDefaultsOffAndValidates(t *testing.T) {
	c := Default()
	if c.Playback.Analysis.Enabled {
		t.Fatal("post-pull analysis must be off by default")
	}
	if got := strings.Join(c.Playback.Analysis.Actions, ","); got != "intro,credits" {
		t.Fatalf("default actions = %q", got)
	}
	if err := validateAnalysis(c.Playback.Analysis); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	bad := map[string]func(a *Analysis){
		"bogus":         func(a *Analysis) { a.Actions = []string{"intro", "bogus"} },
		"action":        func(a *Analysis) { a.Enabled, a.Actions = true, nil },
		"max_hold":      func(a *Analysis) { a.Enabled, a.MaxHold = true, Duration(time.Minute) },
		"poll_interval": func(a *Analysis) { a.Enabled, a.PollInterval = true, Duration(time.Second) },
	}
	for want, mutate := range bad {
		a := c.Playback.Analysis
		mutate(&a)
		if err := validateAnalysis(a); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestSmartCacheV2Knobs(t *testing.T) {
	c := Default()
	lc := c.LocalCache
	if lc.SeasonFill.Enabled || lc.SeasonFill.AfterCompleted != 2 || lc.SeasonFill.MaxEpisodes != 30 {
		t.Fatalf("season_fill defaults = %+v", lc.SeasonFill)
	}
	if lc.SpeculativeGBPerDay != 250 {
		t.Fatalf("speculative_gb_per_day = %d", lc.SpeculativeGBPerDay)
	}
	if lc.TTL.FinishedMovie.D() != 30*day {
		t.Fatalf("balanced finished_movie = %s", lc.TTL.FinishedMovie.D())
	}
	if len(lc.PullOnGrab) != 0 {
		t.Fatalf("pull_on_grab must default to empty (grabs are symlinks only), got %v", lc.PullOnGrab)
	}
	if c.Playback.Analysis.OpenGates || c.Playback.Analysis.AvoidHours != "01:30-05:30" {
		t.Fatalf("analysis gate defaults = %+v", c.Playback.Analysis)
	}
	a := c.Playback.Analysis
	a.AvoidHours = "25:00-03:00"
	if err := validateAnalysis(a); err == nil {
		t.Fatal("bad avoid_hours accepted")
	}
	if s, e, err := ParseHourWindow("23:00-02:30"); err != nil || s != 23*60 || e != 150 {
		t.Fatalf("ParseHourWindow = %d %d %v", s, e, err)
	}
}

func TestCategoryKind(t *testing.T) {
	c := Default()
	c.Playback.Arrs = []Arr{{Category: "tv_hd", Kind: "Sonarr", AlsoCategories: []string{"tv_hd_seerr"}}, {Category: "sonarr_x", Kind: "radarr"}}
	cases := map[string]string{
		"tv_hd": "sonarr", "tv_hd_seerr": "sonarr", // playback.arrs, by any category
		"sonarr_x":    "radarr",                        // an explicit kind wins over the name
		"sonarr_main": "sonarr", "Radarr_4k": "radarr", // inferred from the name
		"films": "", // unknown
	}
	for cat, want := range cases {
		if got := c.CategoryKind(cat); got != want {
			t.Errorf("CategoryKind(%q) = %q, want %q", cat, got, want)
		}
	}
}
