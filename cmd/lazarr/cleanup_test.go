package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/your-username/lazarr/internal/config"
)

func cleanupTestConfig() *config.Config {
	c := config.Default()
	c.Categories = []string{"sonarr_main", "sonarr_main_seerr", "sonarr_hq", "radarr_main", "radarr_4k"}
	c.Paths.DownloadDir = "/data/lazarr"
	c.Paths.FuseMount = "/data/lazarr_mount"
	c.Playback.Arrs = []config.Arr{
		{Category: "sonarr_main", URL: "http://sonarr_main:8989", AlsoCategories: []string{"sonarr_main_seerr"}},
		{Category: "sonarr_hq", URL: "http://sonarr_hq:8989"},
		{Category: "radarr_main", URL: "http://radarr_main:7878"},
	}
	return c
}

// The cleanup block (incl. the per-arr library roots) is editable from the settings
// page; a form that does not send it (an older page) keeps the saved values, and the
// playback form never drops the roots.
func TestSettings_CleanupRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(path, cleanupTestConfig()); err != nil {
		t.Fatal(err)
	}
	running, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &webuiProvider{cfg: running, cfgPath: path}

	s := p.GetSettings()
	if s.Cleanup == nil || s.Cleanup.RemoveEmptyDirs || s.Cleanup.EmptyDirGrace != "1h0m0s" || len(s.Cleanup.LibraryRoots) != 0 {
		t.Fatalf("GetSettings cleanup = %+v", s.Cleanup)
	}
	s.Cleanup.RemoveEmptyDirs = true
	s.Cleanup.EmptyDirGrace = "6h"
	s.Cleanup.LibraryRoots = map[string]string{"sonarr_main": " /data/TV Shows/sonarr_main ", "sonarr_hq": "/data/TV Shows/sonarr_hq"}
	s.Cleanup.NoLibrarySweep = []string{"sonarr_hq"}
	s.Cleanup.SweepLibrary = true
	restart, err := p.SaveSettings(s)
	if err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if !restart {
		t.Fatal("a cleanup change must report restart required")
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cl := got.Cleanup
	if !cl.RemoveEmptyDirs || !cl.SweepLibrary || cl.EmptyDirGrace.D() != 6*time.Hour {
		t.Fatalf("saved cleanup = %+v", cl)
	}
	byCat := map[string]config.Arr{}
	for _, a := range got.Playback.Arrs {
		byCat[a.Category] = a
	}
	if byCat["sonarr_main"].LibraryRoot != "/data/TV Shows/sonarr_main" || byCat["sonarr_main"].NoLibrarySweep ||
		byCat["sonarr_hq"].LibraryRoot != "/data/TV Shows/sonarr_hq" || !byCat["sonarr_hq"].NoLibrarySweep ||
		byCat["radarr_main"].LibraryRoot != "" || len(byCat["sonarr_main"].AlsoCategories) != 1 {
		t.Fatalf("saved arrs = %+v", got.Playback.Arrs)
	}

	// An older form without the block keeps everything, and the playback form (which
	// rebuilds playback.arrs) keeps the roots.
	p.cfg = got
	s = p.GetSettings()
	s.Cleanup = nil
	if _, err := p.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings(nil cleanup): %v", err)
	}
	if got, _ = config.Load(path); !got.Cleanup.RemoveEmptyDirs || len(got.LibraryRootFolders()) != 2 || !got.Playback.Arrs[1].NoLibrarySweep {
		t.Fatalf("nil cleanup reset the block: %+v %+v", got.Cleanup, got.Playback.Arrs)
	}

	// Validation applies to the form.
	s = p.GetSettings()
	s.Cleanup.LibraryRoots = map[string]string{"sonarr_main": "/data"}
	if _, err := p.SaveSettings(s); err == nil {
		t.Fatal("a library root above the download dir must be rejected")
	}
	s = p.GetSettings()
	s.Cleanup.LibraryRoots = map[string]string{"sonarr_4k": "/data/TV Shows/sonarr_4k"}
	if _, err := p.SaveSettings(s); err == nil {
		t.Fatal("a root for an arr not in playback.arrs must be rejected")
	}
	s = p.GetSettings()
	s.Cleanup.EmptyDirGrace = "soon"
	if _, err := p.SaveSettings(s); err == nil {
		t.Fatal("a bad grace must be rejected")
	}
}

type fakeCreator struct {
	on  bool
	err error
}

func (f fakeCreator) CreatesEmptyFolders(context.Context) (bool, error) { return f.on, f.err }

// sweep_library skips an arr that opted out, an arr whose "Create empty folders" is on,
// an arr that cannot be asked, and any root that is not a configured library_root.
func TestLibrarySweepFilter(t *testing.T) {
	c := cleanupTestConfig()
	c.Playback.Arrs[0].LibraryRoot = "/data/TV Shows/sonarr_main"
	c.Playback.Arrs[1].LibraryRoot = "/data/TV Shows/sonarr_hq"
	c.Playback.Arrs[2].LibraryRoot = "/data/Movies/radarr_main"
	c.Playback.Arrs = append(c.Playback.Arrs, config.Arr{Category: "radarr_4k", URL: "http://radarr_4k:7878",
		LibraryRoot: "/data/Movies/radarr_4k", NoLibrarySweep: true})
	f := librarySweepFilter(c, map[string]emptyFolderCreator{
		"sonarr_main": fakeCreator{},
		"sonarr_hq":   fakeCreator{on: true},
		"radarr_main": fakeCreator{err: errors.New("down")},
		"radarr_4k":   fakeCreator{},
	})
	ctx := context.Background()
	for root, want := range map[string]bool{
		"/data/TV Shows/sonarr_main/": true,
		"/data/TV Shows/sonarr_hq":    false, // Create empty series folders on
		"/data/Movies/radarr_main":    false, // unknown = keep
		"/data/Movies/radarr_4k":      false, // no_library_sweep
		"/data/Movies/other":          false, // not a library_root
	} {
		if got := f(ctx, root); got != want {
			t.Errorf("sweep %s = %v, want %v", root, got, want)
		}
	}
}

// emptyDirSweeper takes its library roots from playback.arrs[].library_root.
func TestEmptyDirSweeperRootsFromArrs(t *testing.T) {
	c := cleanupTestConfig()
	c.Playback.Arrs[0].LibraryRoot = "/data/TV Shows/sonarr_main/"
	s := emptyDirSweeper(c, nil)
	if len(s.LibraryRootFolders) != 1 || s.LibraryRootFolders[0] != "/data/TV Shows/sonarr_main" || s.SweepRoot == nil {
		t.Fatalf("sweeper roots = %v", s.LibraryRootFolders)
	}
}
