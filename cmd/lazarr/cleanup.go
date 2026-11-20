package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/arrroots"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/repair"
	"github.com/your-username/lazarr/internal/replsearch"
	"github.com/your-username/lazarr/internal/webui"
)

// emptyDirSweepFirst / emptyDirSweepEvery: the download_dir walk is cheap (~1.8k
// folders on a large library); the library walk only runs with sweep_library.
// arrRootsFirst: the arr library roots are resolved (and WARNs logged) this long after
// boot, so a misconfiguration shows up before the first repair sweep needs it.
const (
	emptyDirSweepFirst = 5 * time.Minute
	emptyDirSweepEvery = time.Hour
	arrRootsFirst      = time.Minute
)

// arrRootResolver maps library paths between Lazarr's and each arr's view
// (playback.arrs[].library_root, else auto-detected from the library links).
func arrRootResolver(cfg *config.Config, links *libscan.Scanner) *arrroots.Resolver {
	r := &arrroots.Resolver{
		Walk: func(ctx context.Context, fn func(string)) error {
			return links.Walk(ctx, func(l libscan.Link) { fn(l.Path) })
		},
	}
	for _, a := range cfg.Playback.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		r.Arrs = append(r.Arrs, arrroots.Arr{Category: a.Category, LibraryRoot: a.LibraryRoot,
			Client: arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)})
	}
	return r
}

// replacementSearcher builds the replacement search (repair.search_replacement), or
// returns nil when it is off, the store cannot persist it, or no arr has an API key.
func replacementSearcher(cfg *config.Config, store catalog.Store) *replsearch.Searcher {
	sr := cfg.Repair.SearchReplacement
	rs, ok := store.(catalog.ReplacementStore)
	if !sr.Enabled || !ok {
		return nil
	}
	arrs := map[string]replsearch.Arr{}
	for _, a := range cfg.Playback.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		arrs[a.Category] = arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)
	}
	if len(arrs) == 0 {
		slog.Warn("replacement search enabled but no playback.arrs entry has a url and api_key; it stays off")
		return nil
	}
	slog.Info("replacement search enabled", "arrs", len(arrs), "max_per_hour", sr.MaxPerHour,
		"cooldown", sr.Cooldown.D())
	return &replsearch.Searcher{Store: rs, Arrs: arrs, MaxPerHour: sr.MaxPerHour, Cooldown: sr.Cooldown.D()}
}

// removedLinkRescanner is what runs after library links were removed: the owning arrs
// rescan, and with the replacement search on, the episodes/movies those links were
// (read from the arr BEFORE its rescan forgets them) are queued for a bounded search.
func removedLinkRescanner(r *arrroots.Resolver, rs *replsearch.Searcher) func(ctx context.Context, paths []string) {
	return func(ctx context.Context, paths []string) {
		targets := r.Targets(ctx, paths)
		var cands []replsearch.Candidate
		if rs != nil {
			cands = rs.Collect(ctx, targets)
		}
		ok := r.RescanTargets(ctx, targets)
		if rs != nil {
			rs.Enqueue(cands, ok)
		}
	}
}

// startArrRootResolve resolves every arr's library root once shortly after boot.
func startArrRootResolve(ctx context.Context, r *arrroots.Resolver) {
	if len(r.Arrs) == 0 {
		return
	}
	go func() {
		t := time.NewTimer(arrRootsFirst)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
			r.ResolveAll(ctx)
		}
	}()
}

// emptyFolderCreator is the arr call that tells whether its disk scan recreates
// missing series/movie folders.
type emptyFolderCreator interface {
	CreatesEmptyFolders(ctx context.Context) (bool, error)
}

// emptyDirSweeper builds the empty-folder cleanup (cleanup.*). It always runs: with
// cleanup.remove_empty_dirs off it only reports.
func emptyDirSweeper(cfg *config.Config, store catalog.Store) *repair.EmptyDirSweeper {
	return &repair.EmptyDirSweeper{
		DownloadDir:        cfg.Paths.DownloadDir,
		Categories:         cfg.Categories,
		Store:              store,
		LibraryRootFolders: cfg.LibraryRootFolders(),
		SweepRoot:          librarySweepFilter(cfg, nil),
		SkipDirs:           []string{cfg.Paths.FuseMount, cfg.LocalCache.Dir, cfg.EffectivePublishAt()},
		Remove:             cfg.Cleanup.RemoveEmptyDirs,
		SweepLibrary:       cfg.Cleanup.SweepLibrary,
		Grace:              cfg.Cleanup.EmptyDirGrace.D(),
	}
}

// librarySweepFilter decides per library root whether cleanup.sweep_library may walk
// it: never for no_library_sweep, never when the arr's "Create empty series/movie
// folders" is on (its next disk scan would recreate every folder), and never when the
// arr cannot be asked (unknown = keep). clients overrides the arr clients (tests).
func librarySweepFilter(cfg *config.Config, clients map[string]emptyFolderCreator) func(context.Context, string) bool {
	byRoot := map[string]config.Arr{}
	for _, a := range cfg.Playback.Arrs {
		if a.LibraryRoot != "" {
			byRoot[filepath.Clean(a.LibraryRoot)] = a
		}
	}
	return func(ctx context.Context, root string) bool {
		a, ok := byRoot[filepath.Clean(root)]
		if !ok || a.NoLibrarySweep {
			return false
		}
		c, ok := clients[a.Category]
		if !ok {
			if a.URL == "" || a.APIKey == "" {
				slog.Warn("cleanup: library sweep skipped: the arr has no URL/API key to check its \"Create empty folders\" setting", "arr", a.Category)
				return false
			}
			c = arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)
		}
		on, err := c.CreatesEmptyFolders(ctx)
		if err != nil {
			slog.Warn("cleanup: library sweep skipped: cannot read the arr's media management settings", "arr", a.Category, "err", err)
			return false
		}
		if on {
			slog.Info("cleanup: library sweep skipped: the arr recreates empty folders (Create empty series/movie folders is on)", "arr", a.Category)
			return false
		}
		return true
	}
}

// startEmptyDirSweep runs the sweep shortly after boot, then hourly.
func startEmptyDirSweep(ctx context.Context, s *repair.EmptyDirSweeper) {
	slog.Info("empty-folder cleanup", "remove", s.Remove, "grace", s.Grace,
		"library_roots", s.LibraryRootFolders, "sweep_library", s.SweepLibrary)
	go func() {
		first := time.NewTimer(emptyDirSweepFirst)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		t := time.NewTicker(emptyDirSweepEvery)
		defer t.Stop()
		for {
			if _, err := s.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("cleanup: empty-folder sweep", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func cleanupSettingsFrom(c *config.Config) *webui.CleanupSettings {
	cl := c.Cleanup
	out := &webui.CleanupSettings{
		RemoveEmptyDirs: cl.RemoveEmptyDirs,
		EmptyDirGrace:   cl.EmptyDirGrace.D().String(),
		LibraryRoots:    map[string]string{},
		NoLibrarySweep:  []string{},
		SweepLibrary:    cl.SweepLibrary,
	}
	for _, a := range c.Playback.Arrs {
		if a.LibraryRoot != "" {
			out.LibraryRoots[a.Category] = a.LibraryRoot
		}
		if a.NoLibrarySweep {
			out.NoLibrarySweep = append(out.NoLibrarySweep, a.Category)
		}
	}
	return out
}

// applyCleanupSettings overlays the cleanup form onto nc (after the playback form, so
// nc.Playback.Arrs is final). nil (an older form) keeps the current values; an empty
// grace keeps the current grace; nil LibraryRoots / NoLibrarySweep keep the arrs'
// current values. A root or opt-out for an arr not in playback.arrs is an error.
func applyCleanupSettings(nc *config.Config, s *webui.CleanupSettings) error {
	if s == nil {
		return nil
	}
	nc.Cleanup.RemoveEmptyDirs = s.RemoveEmptyDirs
	nc.Cleanup.SweepLibrary = s.SweepLibrary
	if s.EmptyDirGrace != "" {
		d, err := config.ParseDuration(s.EmptyDirGrace)
		if err != nil {
			return fmt.Errorf("empty folder grace %q: %w", s.EmptyDirGrace, err)
		}
		nc.Cleanup.EmptyDirGrace = config.Duration(d)
	}
	known := map[string]bool{}
	for _, a := range nc.Playback.Arrs {
		known[a.Category] = true
	}
	var unknown []string
	if s.LibraryRoots != nil {
		for cat := range s.LibraryRoots {
			if !known[strings.TrimSpace(cat)] {
				unknown = append(unknown, cat)
			}
		}
		roots := map[string]string{}
		for cat, r := range s.LibraryRoots {
			roots[strings.TrimSpace(cat)] = strings.TrimSpace(r)
		}
		for i := range nc.Playback.Arrs {
			nc.Playback.Arrs[i].LibraryRoot = roots[nc.Playback.Arrs[i].Category]
		}
	}
	if s.NoLibrarySweep != nil {
		skip := map[string]bool{}
		for _, cat := range s.NoLibrarySweep {
			cat = strings.TrimSpace(cat)
			if cat == "" {
				continue
			}
			if !known[cat] {
				unknown = append(unknown, cat)
			}
			skip[cat] = true
		}
		for i := range nc.Playback.Arrs {
			nc.Playback.Arrs[i].NoLibrarySweep = skip[nc.Playback.Arrs[i].Category]
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("empty folder cleanup: %s not in Playback → arrs", strings.Join(unknown, ", "))
	}
	return nil
}
