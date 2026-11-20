package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleanupConfig is laid out like a typical deploy: one shared tree (/data) holding
// the download dir, the FUSE mount, the cache publish tree and the arrs' root folders.
func cleanupConfig() *Config {
	c := validConfig()
	c.Paths.DownloadDir = "/data/lazarr"
	c.Paths.FuseMount = "/data/lazarr_mount"
	c.LocalCache.Dir = "/cache"
	c.Paths.DBPath = "/config/db.sqlite"
	c.ConfigDir = "/config"
	c.Categories = []string{"sonarr_main", "radarr_4k", "sonarr_hq"}
	return c
}

// arrsWithRoots returns playback.arrs entries with the given library roots.
func arrsWithRoots(roots ...string) []Arr {
	cats := []string{"sonarr_main", "radarr_4k", "sonarr_hq"}
	var out []Arr
	for i, r := range roots {
		out = append(out, Arr{Category: cats[i], URL: "http://" + cats[i] + ":8989", LibraryRoot: r})
	}
	return out
}

func TestValidateCleanup(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*Config)
		wantErr string
	}{
		{"defaults", func(*Config) {}, ""},
		{"remove on, no library roots", func(c *Config) { c.Cleanup.RemoveEmptyDirs = true }, ""},
		{"arr root folders", func(c *Config) {
			c.Playback.Arrs = arrsWithRoots("/data/TV Shows/sonarr_main", "/data/Movies/radarr_4k/")
			c.Cleanup.SweepLibrary = true
		}, ""},
		{"grace too short", func(c *Config) { c.Cleanup.EmptyDirGrace = Duration(time.Minute) }, "empty_dir_grace"},
		{"grace zero", func(c *Config) { c.Cleanup.EmptyDirGrace = 0 }, "empty_dir_grace"},
		{"grace too long", func(c *Config) { c.Cleanup.EmptyDirGrace = Duration(200 * time.Hour) }, "empty_dir_grace"},
		{"sweep without roots", func(c *Config) { c.Cleanup.SweepLibrary = true }, "requires a library_root"},
		{"relative root", func(c *Config) { c.Playback.Arrs = arrsWithRoots("TV Shows") }, "absolute"},
		{"slash", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/") }, `must not be "/"`},
		{"the shared tree", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/data") }, "contains"},
		{"contains the config", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/config") }, "contains"},
		{"inside the download dir", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/data/lazarr/sonarr_main") }, "inside Lazarr's own tree"},
		{"inside the FUSE mount", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/data/lazarr_mount/x") }, "inside Lazarr's own tree"},
		{"inside the publish tree", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/data/lazarr_cache/x") }, "inside Lazarr's own tree"},
		{"shared by two arrs", func(c *Config) { c.Playback.Arrs = arrsWithRoots("/data/tv", "/data/tv/") }, "also sonarr_main's library_root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cleanupConfig()
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

// A library scan root may itself be an arr root folder; a folder above one may not.
func TestValidateCleanup_LibraryRoots(t *testing.T) {
	c := cleanupConfig()
	c.LocalCache.LibraryRoots = []string{"/data/tv", "/data/movies"}
	c.Playback.Arrs = arrsWithRoots("/data/tv")
	if err := c.validate(); err != nil {
		t.Fatalf("library root == arr root folder: %v", err)
	}
	c.LocalCache.LibraryRoots = []string{"/media/tv"}
	c.Playback.Arrs = arrsWithRoots("/media")
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "library root") {
		t.Fatalf("folder above a library root: %v", err)
	}
}

// A config without a cleanup section (every config before v1.2.11) loads with the
// defaults, and the section plus the per-arr roots survive a Save/Load round trip.
func TestCleanupDefaultsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("categories: [sonarr_main]\npaths:\n  download_dir: /data/lazarr\n  fuse_mount: /data/lazarr_mount\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cleanup.RemoveEmptyDirs || c.Cleanup.SweepLibrary || len(c.LibraryRootFolders()) != 0 ||
		c.Cleanup.EmptyDirGrace.D() != time.Hour {
		t.Fatalf("defaults = %+v", c.Cleanup)
	}
	c.Cleanup = Cleanup{RemoveEmptyDirs: true, EmptyDirGrace: Duration(6 * time.Hour), SweepLibrary: true}
	c.Playback.Arrs = []Arr{{Category: "sonarr_main", URL: "http://sonarr_main:8989", LibraryRoot: "/data/TV Shows/sonarr_main/", NoLibrarySweep: true}}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cleanup.RemoveEmptyDirs || !got.Cleanup.SweepLibrary || got.Cleanup.EmptyDirGrace.D() != 6*time.Hour {
		t.Fatalf("round trip = %+v", got.Cleanup)
	}
	if r := got.LibraryRootFolders(); len(r) != 1 || r[0] != "/data/TV Shows/sonarr_main" || !got.Playback.Arrs[0].NoLibrarySweep {
		t.Fatalf("round trip arrs = %+v", got.Playback.Arrs)
	}
}
