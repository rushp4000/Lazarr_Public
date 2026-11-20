package repair

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
)

// Areas of the empty-folder cleanup (metric label values).
const (
	AreaDownload = "download" // <download_dir>/<category>/<release>/...
	AreaLibrary  = "library"  // below cleanup.library_root_folders (hourly sweep)
	AreaRepair   = "repair"   // library folders the repair sweeps just emptied
)

// maxEmptyDirDepth bounds the walk below a root; anything deeper counts as content.
const maxEmptyDirDepth = 32

// ReleaseLister is the slice of catalog.Store the sweeper needs.
type ReleaseLister interface {
	ListByCategory(category string) ([]*catalog.Release, error)
}

// EmptyDirSweeper removes folders that hold nothing but other empty folders
// (cleanup.remove_empty_dirs; report-only when Remove is false).
//
// Safety rules, all enforced here:
//   - Only directories are removed, with rmdir(2): it fails on anything that is not an
//     empty directory, so a file or link that appears during a sweep makes the removal
//     fail harmlessly. Nothing is ever removed recursively.
//   - Symlinks are never followed: a symlink (to a file or to a directory) is content,
//     so a folder holding one is not empty. Folders on another filesystem (a mount
//     point) and folders deeper than maxEmptyDirDepth also count as content.
//   - A folder counts as empty only if no file, link or other non-directory exists
//     anywhere below it, and neither it nor any folder below it was modified within
//     Grace (a grab or an import may be creating it).
//   - The roots are never removed: the download category folders
//     (<download_dir>/<category>), the library root folders, nor anything above them.
//     Skip dirs (FUSE mount, cache trees, download_dir for library walks) are never
//     entered.
//   - Download area: the release folder of every release the arr can still see in the
//     qBit list (not detached), of every wait download, and of every release added
//     within Grace is left alone with everything in it: the arr may still import from
//     it. A catalog read error skips the category.
type EmptyDirSweeper struct {
	// DownloadDir and Categories locate the download category folders.
	DownloadDir string
	Categories  []string
	// Store protects the folders of releases the arr still sees.
	Store ReleaseLister
	// LibraryRootFolders: library folders are only ever removed strictly below these
	// (playback.arrs[].library_root).
	LibraryRootFolders []string
	// SweepRoot, when set, decides per root whether the hourly SweepLibrary run may
	// walk it (no_library_sweep, "Create empty folders" on, arr unreachable = false).
	// PruneEmptied ignores it.
	SweepRoot func(ctx context.Context, root string) bool
	// SkipDirs are never entered (FUSE mount, local cache dir, publish tree).
	SkipDirs []string
	// Remove is cleanup.remove_empty_dirs (false = report only).
	Remove bool
	// SweepLibrary is cleanup.sweep_library: the hourly run also walks the library roots.
	SweepLibrary bool
	// Grace is cleanup.empty_dir_grace.
	Grace time.Duration
	Now   func() time.Time

	mu   sync.Mutex // serializes runs and PruneEmptied
	last EmptyDirReport
}

// EmptyDirArea is one area's result.
type EmptyDirArea struct {
	Found   int      `json:"found"`   // empty folders seen (removed or not)
	Removed int      `json:"removed"` // folders removed
	Failed  int      `json:"failed"`  // removals that failed (non-empty by now, permission)
	Sample  []string `json:"sample"`  // up to 10 top-level empty folders
}

// EmptyDirReport is one run's result.
type EmptyDirReport struct {
	At         int64        `json:"at"` // unix; 0 = never ran
	RemoveMode bool         `json:"remove_enabled"`
	Download   EmptyDirArea `json:"download"`
	Library    EmptyDirArea `json:"library"`
}

// Run sweeps the download category folders and, with SweepLibrary, the library root
// folders once.
func (s *EmptyDirSweeper) Run(ctx context.Context) (EmptyDirReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rep := EmptyDirReport{At: now.Unix(), RemoveMode: s.Remove}
	var errs error

	if s.DownloadDir != "" {
		dd := filepath.Clean(s.DownloadDir)
		seen := map[string]bool{}
		for _, cat := range s.Categories {
			if cat == "" || seen[cat] || strings.ContainsRune(cat, filepath.Separator) || cat == "." || cat == ".." {
				continue
			}
			seen[cat] = true
			protect, err := s.protectedReleaseDirs(dd, cat, now)
			if err != nil {
				errs = errors.Join(errs, err)
				continue // unknown which folders are live: leave the category alone
			}
			s.sweepRoot(ctx, filepath.Join(dd, cat), protect, s.skipSet(false), now, &rep.Download)
		}
	}
	if s.SweepLibrary {
		roots := s.libraryRoots()
		skip := s.skipSet(true)
		for _, r := range roots {
			skip[r] = true // a root nested in another root is never removed by its walk
		}
		for _, root := range roots {
			if s.SweepRoot != nil && !s.SweepRoot(ctx, root) {
				continue
			}
			s.sweepRoot(ctx, root, nil, skip, now, &rep.Library)
		}
	}
	if err := ctx.Err(); err != nil {
		errs = errors.Join(errs, err)
	}

	metrics.SetEmptyDirs(AreaDownload, rep.Download.Found-rep.Download.Removed)
	metrics.SetEmptyDirs(AreaLibrary, rep.Library.Found-rep.Library.Removed)
	for _, a := range []struct {
		name string
		a    EmptyDirArea
	}{{AreaDownload, rep.Download}, {AreaLibrary, rep.Library}} {
		if a.a.Removed > 0 {
			metrics.AddEmptyDirsRemoved(a.name, a.a.Removed)
		}
		if a.a.Found == 0 {
			continue
		}
		if s.Remove {
			slog.Info("cleanup: removed empty folders", "area", a.name,
				"found", a.a.Found, "removed", a.a.Removed, "failed", a.a.Failed, "sample", a.a.Sample)
		} else {
			slog.Info("cleanup: empty folders found (report only; cleanup.remove_empty_dirs is off)",
				"area", a.name, "found", a.a.Found, "sample", a.a.Sample)
		}
	}
	s.last = rep
	return rep, errs
}

// Last returns the most recent report.
func (s *EmptyDirSweeper) Last() EmptyDirReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// PruneEmptied removes the library folders a repair sweep just emptied: for each
// removed link it walks up from the link's folder and rmdirs each folder until one is
// not empty, stopping before the library root folder that contains it. Folders not
// strictly below a LibraryRootFolders entry are never touched, and Grace does not
// apply (Lazarr itself just modified them). Only when Remove is set. Returns the
// number of folders removed.
func (s *EmptyDirSweeper) PruneEmptied(links []string) int {
	if !s.Remove || len(links) == 0 {
		return 0
	}
	roots := s.libraryRoots()
	if len(roots) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	skip := s.skipSet(true)
	removed := 0
	// Deepest first, so sibling seasons are gone before their series folder is tried. A
	// folder that is not empty yet is tried again for the next link (rmdir is cheap).
	sorted := append([]string(nil), links...)
	sort.Slice(sorted, func(i, j int) bool {
		return strings.Count(sorted[i], string(filepath.Separator)) > strings.Count(sorted[j], string(filepath.Separator))
	})
	for _, l := range sorted {
		for dir := filepath.Dir(filepath.Clean(l)); ; dir = filepath.Dir(dir) {
			root := containingRoot(dir, roots)
			if root == "" || s.isProtectedAncestor(dir, roots, skip) || !sameDevice(root, dir) {
				break
			}
			if err := rmdirOnly(dir); err != nil {
				if !errors.Is(err, fs.ErrNotExist) && !isNotEmpty(err) {
					slog.Warn("cleanup: remove emptied library folder", "dir", dir, "err", err)
				}
				break
			}
			removed++
			slog.Info("cleanup: removed library folder emptied by repair", "dir", dir)
		}
	}
	if removed > 0 {
		metrics.AddEmptyDirsRemoved(AreaRepair, removed)
	}
	return removed
}

// protectedReleaseDirs returns the release folders of one category that must not be
// touched: releases the arr still sees, wait downloads, and fresh grabs.
func (s *EmptyDirSweeper) protectedReleaseDirs(dd, cat string, now time.Time) (map[string]bool, error) {
	out := map[string]bool{}
	if s.Store == nil {
		return nil, errors.New("cleanup: no catalog")
	}
	rels, err := s.Store.ListByCategory(cat)
	if err != nil {
		return nil, err
	}
	fresh := now.Add(-s.Grace).Unix()
	for _, r := range rels {
		if r == nil || r.Name == "" {
			continue
		}
		if r.DetachedAt == 0 || r.State == catalog.StateDownloading || r.AddedOn >= fresh {
			out[filepath.Join(dd, cat, filepath.Clean(r.Name))] = true
		}
	}
	return out, nil
}

// sweepRoot walks one root and removes (or reports) its empty folders. The root itself
// is never removed. protect: folders left alone with everything below them.
func (s *EmptyDirSweeper) sweepRoot(ctx context.Context, root string, protect, skip map[string]bool, now time.Time, area *EmptyDirArea) {
	fi, err := os.Lstat(root)
	if err != nil || !fi.IsDir() { // missing, or a symlink: never followed
		if err == nil {
			slog.Warn("cleanup: root is not a real directory, skipped", "root", root)
		}
		return
	}
	w := emptyWalk{
		ctx: ctx, protect: protect, skip: skip,
		cutoff: now.Add(-s.Grace), dev: devOf(fi),
	}
	w.walk(root, 0)
	if ctx.Err() != nil {
		return
	}
	area.Found += len(w.empty)
	isEmpty := make(map[string]bool, len(w.empty))
	for _, d := range w.empty {
		isEmpty[d] = true
	}
	for _, d := range w.empty {
		if !isEmpty[filepath.Dir(d)] && len(area.Sample) < 10 {
			area.Sample = append(area.Sample, d) // a top-level empty folder
		}
	}
	if !s.Remove {
		return
	}
	// w.empty is post-order: children before parents.
	failed := map[string]bool{}
	for _, d := range w.empty {
		if failed[d] {
			continue
		}
		if err := rmdirOnly(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			area.Failed++
			for p := filepath.Dir(d); p != root && strings.HasPrefix(p, root); p = filepath.Dir(p) {
				failed[p] = true // its parents cannot be empty now
			}
			if !isNotEmpty(err) {
				slog.Warn("cleanup: remove empty folder", "dir", d, "err", err)
			}
			continue
		}
		area.Removed++
	}
}

// emptyWalk is one post-order walk below a root.
type emptyWalk struct {
	ctx     context.Context
	protect map[string]bool
	skip    map[string]bool
	cutoff  time.Time
	dev     uint64
	empty   []string // removable folders, children before parents
}

// walk reports whether dir is removable (empty and old enough). depth 0 = the root,
// which is walked but never reported.
func (w *emptyWalk) walk(dir string, depth int) bool {
	if w.ctx.Err() != nil || depth > maxEmptyDirDepth {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || devOf(fi) != w.dev {
		return false
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false // unreadable = unknown = content
	}
	ok := !fi.ModTime().After(w.cutoff)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if !e.IsDir() || w.skip[p] || w.protect[p] { // files, symlinks (never followed), sockets...
			ok = false
			continue
		}
		if !w.walk(p, depth+1) {
			ok = false
		}
	}
	if depth == 0 || !ok {
		return false
	}
	w.empty = append(w.empty, dir)
	return true
}

// libraryRoots returns the cleaned library root folders.
func (s *EmptyDirSweeper) libraryRoots() []string {
	out := make([]string, 0, len(s.LibraryRootFolders))
	for _, r := range s.LibraryRootFolders {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		if c := filepath.Clean(r); c != "/" {
			out = append(out, c)
		}
	}
	return out
}

// skipSet returns the folders never entered. withDownload adds download_dir (library
// walks must not reach Lazarr's own grab tree).
func (s *EmptyDirSweeper) skipSet(withDownload bool) map[string]bool {
	skip := map[string]bool{}
	for _, d := range s.SkipDirs {
		if d != "" {
			skip[filepath.Clean(d)] = true
		}
	}
	if withDownload && s.DownloadDir != "" {
		skip[filepath.Clean(s.DownloadDir)] = true
	}
	return skip
}

// isProtectedAncestor: dir is, or contains, a library root folder or a skip dir.
func (s *EmptyDirSweeper) isProtectedAncestor(dir string, roots []string, skip map[string]bool) bool {
	for _, r := range roots {
		if isUnderOrEqual(r, dir) {
			return true
		}
	}
	for d := range skip {
		if isUnderOrEqual(d, dir) || isUnderOrEqual(dir, d) {
			return true
		}
	}
	return false
}

// containingRoot returns the root folder dir lies strictly below, or "".
func containingRoot(dir string, roots []string) string {
	best := ""
	for _, r := range roots {
		if dir != r && isUnderOrEqual(dir, r) && len(r) > len(best) {
			best = r
		}
	}
	return best
}

func isUnderOrEqual(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// rmdirOnly removes dir only if it is an empty directory. rmdir(2) never unlinks a
// file or a symlink (ENOTDIR) and never recurses (ENOTEMPTY), unlike os.Remove, which
// falls back to unlink.
func rmdirOnly(dir string) error {
	if err := syscall.Rmdir(dir); err != nil {
		return &fs.PathError{Op: "rmdir", Path: dir, Err: err}
	}
	return nil
}

func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

func devOf(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}

// sameDevice reports whether dir is a real directory on root's filesystem.
func sameDevice(root, dir string) bool {
	rfi, err := os.Lstat(root)
	if err != nil {
		return false
	}
	dfi, err := os.Lstat(dir)
	if err != nil || !dfi.IsDir() {
		return false
	}
	return devOf(rfi) == devOf(dfi)
}

func (s *EmptyDirSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
