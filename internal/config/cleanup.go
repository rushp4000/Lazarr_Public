package config

import (
	"fmt"
	"path/filepath"
	"time"
)

// Cleanup configures the empty-folder cleanup (v1.2.11). Lazarr's download_dir keeps
// one folder per grab (<download_dir>/<category>/<release>/...), and the arrs' library
// keeps series/season/movie folders after their files are gone. When a release is
// deleted, or library links are removed by the repair sweeps, those folders are left
// behind empty. See internal/repair/emptydirs.go for the exact rules.
type Cleanup struct {
	// RemoveEmptyDirs removes empty folders (directories that contain nothing but other
	// empty directories) instead of only reporting them:
	//   - release folders under <download_dir>/<category>/ whose release is no longer in
	//     the arr's download list (never the category folder itself), hourly;
	//   - library folders emptied by the repair sweeps (repair.unlink_orphans,
	//     policy.repair_unlink_dead), right after the links are removed, strictly below
	//     a playback.arrs[].library_root only;
	//   - with SweepLibrary, any empty folder below those library roots, hourly.
	// Default false: the hourly sweep only reports what it would remove (log + metric).
	RemoveEmptyDirs bool `yaml:"remove_empty_dirs"`
	// EmptyDirGrace: a folder is left alone while it, or any folder below it, was
	// modified less than this long ago (a grab or an import may be creating it right
	// now). 5m..168h, default 1h. Not applied to folders the repair sweeps empty
	// themselves (they are pruned right after Lazarr removed the links in them).
	EmptyDirGrace Duration `yaml:"empty_dir_grace"`
	// SweepLibrary also sweeps the empty folders that already exist below each
	// playback.arrs[].library_root (e.g. a movie folder of a monitored movie whose file
	// was removed, a series folder left after the series was deleted in the arr).
	// Skipped for an arr with no_library_sweep, for an arr with "Create empty
	// series/movie folders" on, and for an arr that cannot be asked. Requires at least
	// one library_root. Default false.
	SweepLibrary bool `yaml:"sweep_library"`
}

// defaultCleanup is the cleanup section's defaults (report only, 1h grace).
func defaultCleanup() Cleanup {
	return Cleanup{EmptyDirGrace: Duration(time.Hour)}
}

// LibraryRootFolders returns the configured playback.arrs[].library_root values.
func (c *Config) LibraryRootFolders() []string {
	var out []string
	for _, a := range c.Playback.Arrs {
		if a.LibraryRoot != "" {
			out = append(out, filepath.Clean(a.LibraryRoot))
		}
	}
	return out
}

// validateCleanup checks the cleanup knobs and every playback.arrs[].library_root
// (also while removal is off, so a bad value is caught when it is written). A library
// root must be an arr root folder: an absolute path, not "/", not inside one of
// Lazarr's own trees, not a folder that CONTAINS a library scan root or one of Lazarr's
// trees (that would put sibling arr root folders and download category folders within
// reach of the sweep), and not shared by two arrs.
func (c *Config) validateCleanup() error {
	cl := c.Cleanup
	if d := cl.EmptyDirGrace.D(); d < 5*time.Minute || d > 168*time.Hour {
		return fmt.Errorf("cleanup.empty_dir_grace (%s) must be 5m..168h", d)
	}
	if cl.SweepLibrary && len(c.LibraryRootFolders()) == 0 {
		return fmt.Errorf("cleanup.sweep_library requires a library_root on at least one playback.arrs entry")
	}
	lazarrTrees := []string{c.Paths.DownloadDir, c.Paths.FuseMount, c.LocalCache.Dir, c.EffectivePublishAt()}
	private := append([]string{c.ConfigDir, c.Paths.DBPath}, lazarrTrees...)
	seen := map[string]string{}
	for _, a := range c.Playback.Arrs {
		f := a.LibraryRoot
		if f == "" {
			continue
		}
		field := fmt.Sprintf("playback.arrs[%s].library_root", a.Category)
		if !filepath.IsAbs(f) {
			return fmt.Errorf("%s %q must be an absolute path", field, f)
		}
		cf := filepath.Clean(f)
		if cf == "/" {
			return fmt.Errorf("%s must not be \"/\"", field)
		}
		if other, dup := seen[cf]; dup {
			return fmt.Errorf("%s %q is also %s's library_root", field, f, other)
		}
		seen[cf] = a.Category
		for _, p := range private {
			if p != "" && underOrEqual(filepath.Clean(p), cf) {
				return fmt.Errorf("%s %q contains %s: set the arr's root folder itself", field, f, p)
			}
		}
		for _, p := range lazarrTrees {
			if p != "" && underOrEqual(cf, filepath.Clean(p)) {
				return fmt.Errorf("%s %q is inside Lazarr's own tree %s", field, f, p)
			}
		}
		// A library scan root may BE an arr root folder, but a folder above one is the
		// shared tree, not a root folder.
		for _, r := range c.EffectiveLibraryRoots() {
			if cr := filepath.Clean(r); cr != cf && underOrEqual(cr, cf) {
				return fmt.Errorf("%s %q contains library root %s: set the arr's root folder itself", field, f, r)
			}
		}
	}
	return nil
}
