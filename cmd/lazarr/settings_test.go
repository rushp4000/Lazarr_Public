package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/webui"
)

// The daily speculative cap is editable from the settings page and must survive a
// form round trip without disturbing settings the form does not expose.
func TestSettings_SpeculativeCapRoundTrip(t *testing.T) {
	nc := config.Default()
	nc.LocalCache.SeasonFill.Enabled = true // not on the form: must be preserved

	s := (&webuiProvider{cfg: nc}).GetSettings()
	if got, want := s.Cache.SpeculativeGBPerDay, nc.LocalCache.SpeculativeGBPerDay; got != want {
		t.Fatalf("GetSettings cap = %d, want %d", got, want)
	}

	s.Cache.SpeculativeGBPerDay = 80
	if err := applyCacheSettings(nc, s); err != nil {
		t.Fatalf("applyCacheSettings: %v", err)
	}
	if nc.LocalCache.SpeculativeGBPerDay != 80 {
		t.Fatalf("cap = %d, want 80", nc.LocalCache.SpeculativeGBPerDay)
	}
	if !nc.LocalCache.SeasonFill.Enabled {
		t.Fatal("season_fill.enabled was reset by saving the form")
	}

	s.Cache.SpeculativeGBPerDay = 0 // 0 = speculative pulls off, must be accepted
	if err := applyCacheSettings(nc, s); err != nil {
		t.Fatalf("applyCacheSettings(0): %v", err)
	}
	if nc.LocalCache.SpeculativeGBPerDay != 0 {
		t.Fatalf("cap = %d, want 0", nc.LocalCache.SpeculativeGBPerDay)
	}
}

// cache_wait_max is editable from the settings page (v1.2.8), and the v1.2.8 policy
// knobs the form does not expose survive a save.
func TestSettings_CacheWaitMaxRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := config.Default()
	c.Policy.OnCacheMiss = "wait"
	c.Policy.CacheWaitBudget = config.Duration(20 * time.Minute)
	c.Policy.RejectArchives = "any"
	c.Policy.WaitNoProgress = config.Duration(10 * time.Minute)
	if err := config.Save(path, c); err != nil {
		t.Fatal(err)
	}
	running, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &webuiProvider{cfg: running, cfgPath: path}
	s := p.GetSettings()
	if s.CacheWaitMax != "0s" {
		t.Fatalf("GetSettings cache_wait_max = %q, want 0s", s.CacheWaitMax)
	}
	s.CacheWaitMax = "45m"
	if _, err := p.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policy.CacheWaitMax.D() != 45*time.Minute {
		t.Fatalf("cache_wait_max = %s, want 45m", got.Policy.CacheWaitMax.D())
	}
	if got.Policy.RejectArchives != "any" || got.Policy.WaitNoProgress.D() != 10*time.Minute ||
		!got.Policy.RefuseAddErrors || !got.Policy.DetachOnDelete {
		t.Fatalf("form save reset v1.2.8 knobs: %+v", got.Policy)
	}
}

// wait_fail_blocklist reaches the owning arr for an aliased category too.
func TestWaitFailArrs_AlsoCategories(t *testing.T) {
	c := config.Default()
	c.Categories = []string{"sonarr_main", "sonarr_main_seerr"}
	c.Playback.Arrs = []config.Arr{{Category: "sonarr_main", URL: "http://sonarr_main:8989", APIKey: "k",
		AlsoCategories: []string{"sonarr_main_seerr"}}}
	got := waitFailArrs(c)
	if got["sonarr_main"] == nil || got["sonarr_main_seerr"] == nil || got["sonarr_main"] != got["sonarr_main_seerr"] {
		t.Fatalf("alias must map to the same arr client: %v", got)
	}
}

// local_cache.ttl.finished_movie is editable from the settings page (v1.2.8).
func TestSettings_FinishedMovieRoundTrip(t *testing.T) {
	nc := config.Default()
	s := (&webuiProvider{cfg: nc}).GetSettings()
	if s.Cache.TTLFinishedMovie != nc.LocalCache.TTL.FinishedMovie.D().String() {
		t.Fatalf("GetSettings ttl_finished_movie = %q", s.Cache.TTLFinishedMovie)
	}
	s.Cache.TTLFinishedMovie = "90d"
	if err := applyCacheSettings(nc, s); err != nil {
		t.Fatal(err)
	}
	if nc.LocalCache.TTL.FinishedMovie.D() != 90*24*time.Hour {
		t.Fatalf("finished_movie = %s", nc.LocalCache.TTL.FinishedMovie.D())
	}
}

// The slow-lane block round-trips through the settings form; empty durations keep the
// current values, and adoption can never be on while the lane is off.
func TestSettings_SlowLaneRoundTrip(t *testing.T) {
	nc := config.Default()
	s := (&webuiProvider{cfg: nc}).GetSettings()
	if s.SlowLane.MaxConcurrent != 1 || s.SlowLane.PollInterval != "10m0s" || s.SlowLane.MaxAge != "168h0m0s" {
		t.Fatalf("GetSettings slow lane = %+v", s.SlowLane)
	}

	if s.SlowLane.AutoImport == nil || !*s.SlowLane.AutoImport || s.SlowLane.AutoImportAfter != "15m0s" {
		t.Fatalf("GetSettings auto import = %v %q", s.SlowLane.AutoImport, s.SlowLane.AutoImportAfter)
	}

	off := false
	s.SlowLane = webui.SlowLaneSettings{Enabled: true, MaxConcurrent: 2, PollInterval: "15m", MaxAge: "3d", AdoptWaitFailures: true,
		AutoImport: &off, AutoImportAfter: "30m"}
	if err := applySlowLaneSettings(nc, s.SlowLane); err != nil {
		t.Fatal(err)
	}
	want := config.SlowLane{Enabled: true, MaxConcurrent: 2, PollInterval: config.Duration(15 * time.Minute),
		MaxAge: config.Duration(72 * time.Hour), AdoptWaitFailures: true, AutoImport: false, AutoImportAfter: config.Duration(30 * time.Minute)}
	if nc.SlowLane != want {
		t.Fatalf("applied = %+v, want %+v", nc.SlowLane, want)
	}
	if err := nc.Validate(); err != nil {
		t.Fatal(err)
	}

	// Lane off: adoption is forced off too; blank fields keep their values.
	if err := applySlowLaneSettings(nc, webui.SlowLaneSettings{AdoptWaitFailures: true}); err != nil {
		t.Fatal(err)
	}
	if nc.SlowLane.Enabled || nc.SlowLane.AdoptWaitFailures || nc.SlowLane.MaxConcurrent != 2 || nc.SlowLane.PollInterval.D() != 15*time.Minute ||
		nc.SlowLane.AutoImport || nc.SlowLane.AutoImportAfter.D() != 30*time.Minute {
		t.Fatalf("disable kept = %+v", nc.SlowLane)
	}
	if err := nc.Validate(); err != nil {
		t.Fatal(err)
	}

	if err := applySlowLaneSettings(nc, webui.SlowLaneSettings{PollInterval: "soon"}); err == nil {
		t.Fatal("a bad duration must be rejected")
	}
	if err := applySlowLaneSettings(nc, webui.SlowLaneSettings{AutoImportAfter: "later"}); err == nil {
		t.Fatal("a bad auto import delay must be rejected")
	}
}

// Saving the whole form writes slow_lane to disk.
func TestSettings_SlowLaneSavedToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatal(err)
	}
	cur, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &webuiProvider{cfg: cur, cfgPath: path}
	s := p.GetSettings()
	s.SlowLane.Enabled = true
	s.SlowLane.MaxAge = "0s"
	restart, err := p.SaveSettings(s)
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("enabling the slow lane needs a restart")
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SlowLane.Enabled || got.SlowLane.MaxAge != 0 {
		t.Fatalf("on disk = %+v", got.SlowLane)
	}
}
