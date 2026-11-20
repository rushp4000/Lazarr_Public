package localcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback"
)

// linkIndex knows where the arrs moved every Lazarr symlink and can atomically
// retarget them between the FUSE tree and the cache tree (docs/28 §4.5).
type linkIndex struct {
	store    catalog.Store
	resolver *playback.Resolver
	roots    []string // library roots + download dir
	// downloadDir is Lazarr's own grab tree: its links are indexed and retargeted
	// like library links, but they are not a reference that keeps a cached file.
	downloadDir string
	puid        int
	pgid        int
	now         func() time.Time
	// walkMu serialises walks: the reaper, eviction and drop paths can all ask for one.
	walkMu sync.Mutex
}

// tmpSuffix is appended to a link's name while the replacement symlink is created;
// the final rename(2) over the live name is what makes the swap atomic.
const tmpSuffix = ".lazarr-tmp"

// errIncompleteWalk: part of a root could not be read, so links the walk did not
// see may still exist.
var errIncompleteWalk = errors.New("localcache: link walk incomplete")

// walk scans every root, records each Lazarr symlink it finds, puts links whose
// cache target is missing back on FUSE, and prunes rows the walk did not see (they
// were renamed/deleted by an arr). A walk that could not read part of a root prunes
// nothing: "not seen" is then not "gone". The FUSE and cache trees hold no links
// and are never descended. Returns the number seen and the number repaired.
func (li *linkIndex) walk() (seen, repaired int, err error) {
	li.walkMu.Lock()
	defer li.walkMu.Unlock()
	start := li.now()
	var errs error
	incomplete := false
	skip := map[string]bool{}
	for _, t := range []string{li.resolver.FuseMount(), li.resolver.CacheRoot()} {
		if t != "" {
			skip[filepath.Clean(t)] = true
		}
	}
	for _, root := range li.roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				if p == root && errors.Is(werr, fs.ErrNotExist) {
					return filepath.SkipDir // a root that does not exist yet is fine
				}
				// Unreadable entry: skip it, keep walking, but do not prune afterwards.
				incomplete = true
				errs = errors.Join(errs, fmt.Errorf("%w: %s: %w", errIncompleteWalk, p, werr))
				return nil
			}
			if d.IsDir() && p != root && skip[p] {
				return filepath.SkipDir
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			if strings.HasSuffix(p, tmpSuffix) {
				// Leftover from an interrupted retarget: a symlink WE created, named with
				// our private suffix, under a library root. Removing a symlink never
				// follows it; nothing else is ever deleted from a library.
				_ = os.Remove(p) //nolint:gosec // G122: see above
				return nil
			}
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			kind, hash, rel, ok := li.resolver.ParseTarget(target)
			if !ok {
				return nil
			}
			if kind == playback.TargetCache && !cacheFilePresent(target) {
				// A link into the cache tree whose bytes are gone (evicted without a
				// retarget, cache volume missing at boot, orphan sweep, …). A media server
				// scan in this state trashes the item, so put it back on FUSE now.
				if err := li.swap(p, li.resolver.TargetFor(playback.TargetFuse, hash, rel)); err != nil {
					errs = errors.Join(errs, err)
					return nil
				}
				metrics.IncLocalRetarget(playback.TargetFuse)
				metrics.IncLocalDanglingRepaired()
				repaired++
				kind = playback.TargetFuse
			}
			fileID, ok := li.fileIDFor(hash, rel)
			if !ok {
				return nil // a link into our tree for a release we no longer know
			}
			seen++
			_ = li.store.UpsertLibraryLink(&catalog.LibraryLink{
				LinkPath: p, Hash: hash, FileID: fileID, TargetKind: kind, LastVerified: start.Unix(),
			})
			return nil
		})
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}
	// Rows not refreshed by a complete walk are gone (renamed/upgraded/deleted).
	if !incomplete {
		if _, err := li.store.PruneLibraryLinks(start.Unix()); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return seen, repaired, errs
}

// cacheFilePresent reports whether a cache-tree target is a readable regular file.
// Any stat error counts as absent: on the soft SMB mount an unreachable share
// returns EIO, and a link the media server cannot open is as bad as a dangling one.
func cacheFilePresent(target string) bool {
	fi, err := os.Stat(target)
	return err == nil && fi.Mode().IsRegular()
}

// fileIDFor maps (hash, rel_path) to the catalog file id.
func (li *linkIndex) fileIDFor(hash, rel string) (int, bool) {
	_, files, err := li.store.GetRelease(hash)
	if err != nil {
		return 0, false
	}
	for _, f := range files {
		if filepath.Clean(f.RelPath) == rel {
			return f.FileID, true
		}
	}
	return 0, false
}

// observe records a link learned from a playback session (server-reported path).
func (li *linkIndex) observe(ref *playback.Ref, fileID int) {
	_ = li.store.UpsertLibraryLink(&catalog.LibraryLink{
		LinkPath: ref.LinkPath, Hash: ref.Hash, FileID: fileID, TargetKind: ref.Kind, LastVerified: li.now().Unix(),
	})
}

// retarget repoints every known link of (hash, fileID) at the `to` tree
// (playback.TargetCache or playback.TargetFuse). Each link is re-read immediately
// before the swap and skipped unless it still points into one of our trees at this
// exact file. Returns how many links were switched.
func (li *linkIndex) retarget(hash string, fileID int, rel, to string) (int, error) {
	links, err := li.store.ListLibraryLinks(hash, fileID)
	if err != nil {
		return 0, err
	}
	want := li.resolver.TargetFor(to, hash, rel)
	switched := 0
	var errs error
	for _, l := range links {
		cur, err := os.Readlink(l.LinkPath)
		if err != nil {
			_ = li.store.DeleteLibraryLink(l.LinkPath) // vanished (arr removed it)
			continue
		}
		if !filepath.IsAbs(cur) {
			cur = filepath.Join(filepath.Dir(l.LinkPath), cur)
		}
		kind, h, r, ok := li.resolver.ParseTarget(cur)
		if !ok || h != hash || r != rel {
			_ = li.store.DeleteLibraryLink(l.LinkPath) // no longer ours / points elsewhere
			continue
		}
		if kind == to {
			continue // already there
		}
		if err := li.swap(l.LinkPath, want); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		switched++
		metrics.IncLocalRetarget(to)
		_ = li.store.UpsertLibraryLink(&catalog.LibraryLink{
			LinkPath: l.LinkPath, Hash: hash, FileID: fileID, TargetKind: to, LastVerified: li.now().Unix(),
		})
	}
	return switched, errs
}

// swap atomically replaces the symlink at linkPath with one pointing at target:
// symlink(target, tmp) then rename(tmp, linkPath). Refuses to touch anything that is
// not currently a symlink (never clobber a real file).
func (li *linkIndex) swap(linkPath, target string) error {
	fi, err := os.Lstat(linkPath)
	if err != nil {
		return fmt.Errorf("localcache: lstat %s: %w", linkPath, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("localcache: refusing to replace non-symlink %s", linkPath)
	}
	tmp := linkPath + tmpSuffix
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("localcache: symlink %s: %w", tmp, err)
	}
	if li.puid > 0 && li.pgid > 0 {
		_ = os.Lchown(tmp, li.puid, li.pgid) // best-effort, same as symlink.Manager
	}
	if err := os.Rename(tmp, linkPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("localcache: rename %s over %s: %w", tmp, linkPath, err)
	}
	return nil
}

// libraryLinks returns how many indexed links outside download_dir reference the
// file. Lazarr's own grab-tree link is not counted: the arrs never delete it on an
// upgrade or a delete-with-files while the download stays in the client
// (removeCompletedDownloads off), so counting it would keep a replaced file's
// cached copy until its TTL instead of unreferenced_grace.
func (li *linkIndex) libraryLinks(hash string, fileID int) int {
	links, err := li.store.ListLibraryLinks(hash, fileID)
	if err != nil {
		return 1 // unknown is not "unreferenced"
	}
	n := 0
	for _, l := range links {
		if li.downloadDir != "" && underDir(l.LinkPath, li.downloadDir) {
			continue
		}
		n++
	}
	return n
}

// underDir reports whether p is dir or lies below it.
func underDir(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
