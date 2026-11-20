package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validConfig returns a Config that passes validate(); cases mutate one field.
func validConfig() *Config {
	c := Default()
	c.Categories = []string{"radarr_hq"}
	c.Paths.DownloadDir = "/downloads"
	c.Paths.FuseMount = "/mnt/lazarr"
	c.Policy.IdleTTL = Duration(15 * time.Minute)
	c.Policy.MaxHold = Duration(24 * time.Hour)
	c.Policy.ActiveSlots = 3
	return c
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // substring; "" means expect no error
	}{
		{
			name:   "valid",
			mutate: func(*Config) {},
		},
		{
			name:    "categories without download_dir",
			mutate:  func(c *Config) { c.Paths.DownloadDir = "" },
			wantErr: "download_dir is required",
		},
		{
			name:    "categories without fuse_mount",
			mutate:  func(c *Config) { c.Paths.FuseMount = "" },
			wantErr: "fuse_mount is required",
		},
		{
			name: "no categories tolerates empty paths",
			mutate: func(c *Config) {
				c.Categories = nil
				c.Paths.DownloadDir = ""
				c.Paths.FuseMount = ""
			},
		},
		{
			name:    "torbox max_requests_per_minute above TorBox's limit",
			mutate:  func(c *Config) { c.TorBox.MaxRequestsPerMinute = 301 },
			wantErr: "max_requests_per_minute",
		},
		{
			name:    "negative requestdl_negative_ttl",
			mutate:  func(c *Config) { c.Policy.RequestDLNegativeTTL = Duration(-time.Second) },
			wantErr: "requestdl_negative_ttl",
		},
		{
			name:    "backoff_read_wait too long",
			mutate:  func(c *Config) { c.Policy.BackoffReadWait = Duration(2 * time.Minute) },
			wantErr: "backoff_read_wait",
		},
		{
			name: "pack_janitor without detach_on_delete",
			mutate: func(c *Config) {
				c.Policy.PackJanitor = true
				c.Policy.DetachOnDelete = false
			},
			wantErr: "pack_janitor requires detach_on_delete",
		},
		{
			name:    "cache_wait_max below budget",
			mutate:  func(c *Config) { c.Policy.CacheWaitMax = Duration(5 * time.Minute) },
			wantErr: "cache_wait_max",
		},
		{
			name:    "cache_wait_max negative",
			mutate:  func(c *Config) { c.Policy.CacheWaitMax = Duration(-time.Minute) },
			wantErr: "cache_wait_max",
		},
		{
			name:   "cache_wait_max equal to budget",
			mutate: func(c *Config) { c.Policy.CacheWaitMax = c.Policy.CacheWaitBudget },
		},
		{
			name:    "idle_ttl equal to max_hold",
			mutate:  func(c *Config) { c.Policy.IdleTTL = c.Policy.MaxHold },
			wantErr: "must be strictly less than max_hold",
		},
		{
			name: "idle_ttl greater than max_hold",
			mutate: func(c *Config) {
				c.Policy.IdleTTL = Duration(48 * time.Hour)
			},
			wantErr: "must be strictly less than max_hold",
		},
		{
			name:    "negative active_slots",
			mutate:  func(c *Config) { c.Policy.ActiveSlots = -1 },
			wantErr: "active_slots",
		},
		{
			name:   "zero active_slots is valid (auto-detect)",
			mutate: func(c *Config) { c.Policy.ActiveSlots = 0 },
		},
		{
			name:    "negative puid",
			mutate:  func(c *Config) { c.Ownership.PUID = -5 },
			wantErr: "puid",
		},
		{
			name:    "negative pgid",
			mutate:  func(c *Config) { c.Ownership.PGID = -5 },
			wantErr: "pgid",
		},
		{
			name: "valid puid/pgid set",
			mutate: func(c *Config) {
				c.Ownership.PUID = 1001
				c.Ownership.PGID = 1001
			},
		},
		{
			name:    "puid set without pgid (S5)",
			mutate:  func(c *Config) { c.Ownership.PUID = 1001; c.Ownership.PGID = 0 },
			wantErr: "must both be set",
		},
		{
			name:    "pgid set without puid (S5)",
			mutate:  func(c *Config) { c.Ownership.PUID = 0; c.Ownership.PGID = 1001 },
			wantErr: "must both be set",
		},
		{
			name:   "extra_cdn_hosts valid suffixes",
			mutate: func(c *Config) { c.TorBox.ExtraCDNHosts = []string{".tb-cdn.net", "tb-cdn.example"} },
		},
		{
			name:    "extra_cdn_hosts single-label suffix",
			mutate:  func(c *Config) { c.TorBox.ExtraCDNHosts = []string{".io"} },
			wantErr: "extra_cdn_hosts",
		},
		{
			name:    "extra_cdn_hosts with scheme junk",
			mutate:  func(c *Config) { c.TorBox.ExtraCDNHosts = []string{"https://tb-cdn.net"} },
			wantErr: "extra_cdn_hosts",
		},
		{
			name:    "extra_cdn_hosts empty entry",
			mutate:  func(c *Config) { c.TorBox.ExtraCDNHosts = []string{""} },
			wantErr: "extra_cdn_hosts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("validate() = %q, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoad_RejectsInvalid proves Load wires validate() in (and wraps the error).
func TestLoad_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// categories set but no paths -> validate must reject.
	yaml := "categories: [radarr_hq]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want validation failure")
	}
	if !strings.Contains(err.Error(), "config:") {
		t.Fatalf("Load() error %q missing wrap prefix %q", err, "config:")
	}
}

// TestLoad_Valid parses a complete, valid config including ownership.
func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := strings.Join([]string{
		"categories: [radarr_hq]",
		"paths:",
		"  download_dir: /downloads",
		"  fuse_mount: /mnt/lazarr",
		"policy:",
		"  idle_ttl: 15m",
		"  max_hold: 24h",
		"  active_slots: 0",
		"ownership:",
		"  puid: 1001",
		"  pgid: 1001",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if c.Ownership.PUID != 1001 || c.Ownership.PGID != 1001 {
		t.Fatalf("ownership = %+v, want 1001/1001", c.Ownership)
	}
	if c.Policy.ActiveSlots != 0 {
		t.Fatalf("active_slots = %d, want 0 (auto-detect)", c.Policy.ActiveSlots)
	}
}

// TestSaveLoadRoundTrip guards the Web UI settings editor: a Config written by Save
// must Load back identical (Duration marshal, log_level, secrets intact, mode 0600).
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	c := Default()
	c.LogLevel = "debug"
	c.TorBox.APIKey = "k-123"
	c.Paths.DownloadDir = "/dl"
	c.Paths.FuseMount = "/fuse"
	c.Paths.DBPath = "/data/x.sqlite"
	c.Paths.ProbeCacheDir = "/data/probe"
	c.Categories = []string{"radarr_hq", "sonarr_hq"}
	c.Ownership = Ownership{PUID: 1001, PGID: 1001}
	c.WebUI = WebUI{Listen: ":8082"}
	c.Policy.WaitFailBlocklist = true
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := Save(path, c); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode = %v, want 0600 (holds api key)", fi.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.LogLevel != "debug" || got.TorBox.APIKey != "k-123" ||
		got.Policy.IdleTTL.D() != c.Policy.IdleTTL.D() ||
		got.Policy.MaxHold.D() != c.Policy.MaxHold.D() ||
		len(got.Categories) != 2 || got.Categories[1] != "sonarr_hq" ||
		!got.Policy.WaitFailBlocklist {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestValidateLogLevel(t *testing.T) {
	c := Default()
	c.LogLevel = "loud"
	if err := c.Validate(); err == nil {
		t.Fatalf("expected log_level validation error")
	}
}

func TestEffectiveCacheWaitMax(t *testing.T) {
	p := Policy{CacheWaitBudget: Duration(10 * time.Minute)}
	if got := p.EffectiveCacheWaitMax(); got != 30*time.Minute {
		t.Errorf("auto max = %s, want 30m (3x budget)", got)
	}
	p.CacheWaitMax = Duration(45 * time.Minute)
	if got := p.EffectiveCacheWaitMax(); got != 45*time.Minute {
		t.Errorf("explicit max = %s, want 45m", got)
	}
	p.CacheWaitMax = Duration(time.Minute)
	if got := p.EffectiveCacheWaitMax(); got != 10*time.Minute {
		t.Errorf("max below budget = %s, want clamped to the 10m budget", got)
	}
}

func TestV128Defaults(t *testing.T) {
	c := Default()
	if c.TorBox.MaxRequestsPerMinute != 240 || c.Policy.RequestDLNegativeTTL.D() != 90*time.Second ||
		c.Policy.BackoffReadWait.D() != 0 || !c.Policy.DetachOnDelete {
		t.Fatalf("v1.2.8 defaults: %+v / %+v", c.TorBox, c.Policy)
	}
}

func TestEffectiveOnCacheMiss(t *testing.T) {
	p := Policy{OnCacheMiss: "wait", OnCacheMissByCategory: map[string]string{"sonarr_main_seerr": "reject"}}
	if got := p.EffectiveOnCacheMiss("sonarr_main_seerr"); got != "reject" {
		t.Fatalf("override = %q", got)
	}
	if got := p.EffectiveOnCacheMiss("sonarr_main"); got != "wait" {
		t.Fatalf("fallback = %q", got)
	}
	if got := (Policy{}).EffectiveOnCacheMiss("x"); got != "error" {
		t.Fatalf("empty = %q", got)
	}
	if !(Policy{OnCacheMiss: "reject", OnCacheMissByCategory: map[string]string{"a": "wait"}}).AnyWait() {
		t.Fatal("AnyWait must see a per-category wait")
	}
}

func TestValidateCategoryMissPolicyAndAliases(t *testing.T) {
	base := func() *Config {
		c := Default()
		c.Paths.DownloadDir, c.Paths.FuseMount = "/d", "/f"
		c.Categories = []string{"sonarr_main", "sonarr_main_seerr"}
		return c
	}
	c := base()
	c.Policy.OnCacheMissByCategory = map[string]string{"sonarr_main_seerr": "skip"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "on_cache_miss_by_category") {
		t.Fatalf("bad mode: %v", err)
	}
	c = base()
	c.Playback.Arrs = []Arr{{Category: "sonarr_main", URL: "http://sonarr:8989", AlsoCategories: []string{"sonarr_main_seerr"}}}
	if err := c.validate(); err != nil {
		t.Fatalf("alias: %v", err)
	}
	c.Playback.Arrs[0].AlsoCategories = []string{"nope"}
	if err := c.validate(); err == nil {
		t.Fatal("alias to an unconfigured category must fail")
	}
	c.Playback.Arrs = []Arr{
		{Category: "sonarr_main", URL: "http://a:1", AlsoCategories: []string{"sonarr_main_seerr"}},
		{Category: "sonarr_main_seerr", URL: "http://b:1"},
	}
	if err := c.validate(); err == nil {
		t.Fatal("a category owned twice must fail")
	}
}

func TestValidateAuditDeleteOrphans(t *testing.T) {
	c := Default()
	c.Audit.DeleteOrphansOlderThan = Duration(10 * time.Minute)
	if err := c.validate(); err == nil {
		t.Fatal("under 1h must be rejected")
	}
	c.Audit.DeleteOrphansOlderThan = Duration(168 * time.Hour)
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSlowLane(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*SlowLane)
		wantErr string
	}{
		{"defaults", func(*SlowLane) {}, ""},
		{"enabled with adoption", func(s *SlowLane) { s.Enabled, s.AdoptWaitFailures = true, true }, ""},
		{"max age off", func(s *SlowLane) { s.MaxAge = 0 }, ""},
		{"concurrency 0", func(s *SlowLane) { s.MaxConcurrent = 0 }, "max_concurrent"},
		{"concurrency 6", func(s *SlowLane) { s.MaxConcurrent = 6 }, "max_concurrent"},
		{"poll too fast", func(s *SlowLane) { s.PollInterval = Duration(30 * time.Second) }, "poll_interval"},
		{"poll too slow", func(s *SlowLane) { s.PollInterval = Duration(25 * time.Hour) }, "poll_interval"},
		{"max age under 1h", func(s *SlowLane) { s.MaxAge = Duration(10 * time.Minute) }, "max_age"},
		{"max age negative", func(s *SlowLane) { s.MaxAge = Duration(-time.Hour) }, "max_age"},
		{"adopt while disabled", func(s *SlowLane) { s.AdoptWaitFailures = true }, "requires slow_lane.enabled"},
		{"auto import off", func(s *SlowLane) { s.AutoImport = false }, ""},
		{"auto import after 0", func(s *SlowLane) { s.AutoImportAfter = 0 }, "auto_import_after"},
		{"auto import after too long", func(s *SlowLane) { s.AutoImportAfter = Duration(25 * time.Hour) }, "auto_import_after"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mut(&c.SlowLane)
			err := c.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// The slow-lane block must survive a Save/Load round trip (the settings editor
// rewrites the whole file).
func TestSlowLaneRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := Default()
	c.SlowLane = SlowLane{Enabled: true, MaxConcurrent: 2, PollInterval: Duration(15 * time.Minute),
		MaxAge: Duration(72 * time.Hour), AdoptWaitFailures: true, AutoImport: false, AutoImportAfter: Duration(30 * time.Minute)}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SlowLane != c.SlowLane {
		t.Fatalf("round trip = %+v, want %+v", got.SlowLane, c.SlowLane)
	}
}

// auto_import defaults to on with a 15m head start for the arr, also when a config
// file has a slow_lane block that does not mention it.
func TestSlowLaneAutoImportDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("torbox:\n  api_key: x\nslow_lane:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.SlowLane.AutoImport || c.SlowLane.AutoImportAfter.D() != 15*time.Minute {
		t.Fatalf("defaults = %+v", c.SlowLane)
	}
}

func TestValidateMissing(t *testing.T) {
	c := Default()
	if !c.Missing.Enabled || c.Missing.RefreshInterval.D() != 30*time.Minute {
		t.Fatalf("defaults = %+v, want enabled every 30m", c.Missing)
	}
	for _, tc := range []struct {
		d  time.Duration
		ok bool
	}{{5 * time.Minute, true}, {24 * time.Hour, true}, {time.Minute, false}, {0, false}, {25 * time.Hour, false}} {
		c := Default()
		c.Missing.RefreshInterval = Duration(tc.d)
		err := c.validate()
		if tc.ok != (err == nil) || (err != nil && !strings.Contains(err.Error(), "missing.refresh_interval")) {
			t.Fatalf("refresh_interval %s: validate() = %v, want ok=%v", tc.d, err, tc.ok)
		}
	}
}

func TestValidateSearchReplacement(t *testing.T) {
	d := Default().Repair.SearchReplacement
	if d.Enabled || d.MaxPerHour != 10 || d.Cooldown.D() != 72*time.Hour {
		t.Fatalf("defaults = %+v, want disabled, 10/h, 72h", d)
	}
	for _, tc := range []struct {
		name    string
		mut     func(c *Config)
		wantErr string
	}{
		{"defaults", func(*Config) {}, ""},
		{"enabled with repair_unlink_dead", func(c *Config) {
			c.Policy.RepairUnlinkDead = true
			c.Repair.SearchReplacement.Enabled = true
		}, ""},
		{"enabled with unlink_orphans", func(c *Config) {
			c.Repair.UnlinkOrphans = true
			c.Repair.SearchReplacement.Enabled = true
		}, ""},
		{"enabled without a source", func(c *Config) { c.Repair.SearchReplacement.Enabled = true },
			"requires policy.repair_unlink_dead or repair.unlink_orphans"},
		{"max_per_hour 0", func(c *Config) { c.Repair.SearchReplacement.MaxPerHour = 0 }, "max_per_hour"},
		{"max_per_hour 31", func(c *Config) { c.Repair.SearchReplacement.MaxPerHour = 31 }, "max_per_hour"},
		{"max_per_hour 30", func(c *Config) { c.Repair.SearchReplacement.MaxPerHour = 30 }, ""},
		{"cooldown 23h", func(c *Config) { c.Repair.SearchReplacement.Cooldown = Duration(23 * time.Hour) }, "cooldown"},
		{"cooldown 721h", func(c *Config) { c.Repair.SearchReplacement.Cooldown = Duration(721 * time.Hour) }, "cooldown"},
		{"cooldown 24h", func(c *Config) { c.Repair.SearchReplacement.Cooldown = Duration(24 * time.Hour) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mut(c)
			err := c.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// A config file without a search_replacement block gets the defaults; an explicit
// block survives a Save/Load round trip.
func TestSearchReplacementLoadAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("torbox:\n  api_key: x\nrepair:\n  unlink_orphans: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Repair.SearchReplacement != Default().Repair.SearchReplacement {
		t.Fatalf("defaults = %+v", c.Repair.SearchReplacement)
	}
	c.Repair.SearchReplacement = SearchReplacement{Enabled: true, MaxPerHour: 5, Cooldown: Duration(96 * time.Hour)}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repair != c.Repair {
		t.Fatalf("round trip = %+v, want %+v", got.Repair, c.Repair)
	}
}

// missing.enabled defaults to true, so an explicit false must survive a Save/Load.
func TestMissingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := Default()
	c.Missing = Missing{Enabled: false, RefreshInterval: Duration(time.Hour)}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Missing != c.Missing {
		t.Fatalf("round trip = %+v, want %+v", got.Missing, c.Missing)
	}
}
