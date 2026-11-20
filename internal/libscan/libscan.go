// Package libscan finds the library symlinks that point into Lazarr's trees.
//
// A "library link" is a symlink the arr moved out of paths.download_dir at import time
// (into its root folder: "TV Shows/...", "Movies/...") whose target is a file under
// <fuse_mount>/<hash>/ or under the local-cache publish tree <publish_at>/<hash>/. The
// media servers index those paths, so a release that still has one must never lose
// its catalog row: the FUSE tree only serves hashes the catalog knows.
//
// The download_dir tree itself is NOT a library: it holds Lazarr's own per-grab
// symlinks, which the qBit emulation creates and removes. The local cache's link
// index records both trees, so callers here must never count download_dir entries.
package libscan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback"
)

// Link is one library symlink resolved to a Lazarr file.
type Link struct {
	Path    string // the symlink itself
	Kind    string // playback.TargetFuse | playback.TargetCache
	Hash    string
	RelPath string
	// Chained is true when the link points at a download_dir entry (which in turn
	// points into a Lazarr tree) instead of straight into FUSE/cache. Removing the
	// download_dir tree would break such a link.
	Chained bool
}

// Scanner walks library roots for Lazarr links. The zero value is not usable; set
// Roots, DownloadDir and Resolver.
type Scanner struct {
	// Roots are the directories to walk (config EffectiveLibraryRoots).
	Roots []string
	// DownloadDir is paths.download_dir: never counted as a library, never descended.
	DownloadDir string
	// SkipDirs are never descended into either (FUSE mount, cache dir, publish tree).
	SkipDirs []string
	// Resolver classifies symlink targets (FUSE mount + cache publish root).
	Resolver *playback.Resolver
	// Store, when set, gives a fast path: library_link rows for the hash are re-verified
	// with readlink before any walk. Optional.
	Store catalog.Store

	mu sync.Mutex // serializes walks (a delete storm must not run N parallel walks)
}

// LinksTo returns the library links that reference hash. It first re-verifies the
// link index rows for the hash (excluding download_dir) and returns them if any still
// point at it; otherwise it walks the roots, because the index refreshes only every
// link_index_interval and a fresh import may not be in it yet.
//
// An error means the answer is unknown (walk failed or ctx expired); callers must
// then assume the release IS linked. "No links" is only answered after a complete
// walk: no roots configured, a root that is missing or not a directory, or a
// directory that could not be read all return ErrIncompleteWalk instead, because a
// purge on a false "no links" (an unmounted library, a permissions slip) would
// dangle every imported file of the release for good.
func (s *Scanner) LinksTo(ctx context.Context, hash string) ([]Link, error) {
	if links := s.indexed(hash); len(links) > 0 {
		return links, nil
	}
	var out []Link
	err := s.walk(ctx, true, func(l Link) {
		if l.Hash == hash {
			out = append(out, l)
		}
	})
	return out, err
}

// indexed returns the verified link-index rows for hash outside download_dir.
func (s *Scanner) indexed(hash string) []Link {
	if s.Store == nil {
		return nil
	}
	_, files, err := s.Store.GetRelease(hash)
	if err != nil {
		return nil
	}
	var out []Link
	for _, f := range files {
		rows, err := s.Store.ListLibraryLinks(hash, f.FileID)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if s.inDownloadDir(r.LinkPath) {
				continue // Lazarr's own grab tree, not a library
			}
			if l, ok := s.resolve(r.LinkPath); ok && l.Hash == hash {
				out = append(out, l)
			}
		}
	}
	return out
}

// Walk calls fn for every Lazarr library link under the roots. It never descends
// into download_dir, the skip dirs, or the FUSE/cache trees themselves. Unreadable
// entries are skipped; a root that does not exist is fine. Returns ctx.Err() when
// the walk was cut short.
func (s *Scanner) Walk(ctx context.Context, fn func(Link)) error {
	return s.walk(ctx, false, fn)
}

// ErrIncompleteWalk is returned by LinksTo when part of the library could not be
// walked, so "no links" cannot be told apart from "links not seen".
var ErrIncompleteWalk = errors.New("libscan: library walk incomplete")

// walk is Walk; strict turns every skipped part of the library (no roots, a missing
// root, an unreadable directory) into an ErrIncompleteWalk error after the walk.
func (s *Scanner) walk(ctx context.Context, strict bool, fn func(Link)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	skip := s.skipSet()
	var errs error
	incomplete := func(p string, err error) {
		if strict {
			errs = errors.Join(errs, fmt.Errorf("%w: %s: %w", ErrIncompleteWalk, p, err))
		}
	}
	walked := 0
	for _, root := range s.Roots {
		if root == "" {
			continue
		}
		walked++
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if werr != nil {
				if p == root {
					incomplete(p, werr)
					if errors.Is(werr, fs.ErrNotExist) {
						return filepath.SkipDir
					}
					return nil
				}
				incomplete(p, werr)
				return nil // unreadable entry: skip, keep walking
			}
			if p == root && !d.IsDir() {
				incomplete(p, errors.New("root is not a directory"))
				return filepath.SkipDir
			}
			if d.IsDir() {
				if _, ok := skip[filepath.Clean(p)]; ok {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			if l, ok := s.resolve(p); ok {
				fn(l)
			}
			return nil
		})
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}
	if walked == 0 {
		incomplete("library roots", errors.New("none configured"))
	}
	return errs
}

// resolve reads one symlink and classifies it. Links into download_dir are followed
// exactly one more hop (an arr that linked to its download instead of moving it).
func (s *Scanner) resolve(p string) (Link, bool) {
	target, ok := readTarget(p)
	if !ok {
		return Link{}, false
	}
	chained := false
	if s.inDownloadDir(target) {
		next, ok := readTarget(target)
		if !ok {
			return Link{}, false
		}
		target, chained = next, true
	}
	kind, hash, rel, ok := s.Resolver.ParseTarget(target)
	if !ok {
		return Link{}, false
	}
	return Link{Path: p, Kind: kind, Hash: hash, RelPath: rel, Chained: chained}, true
}

// readTarget returns the absolute, cleaned target of symlink p.
func readTarget(p string) (string, bool) {
	t, err := os.Readlink(p)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(p), t)
	}
	return filepath.Clean(t), true
}

func (s *Scanner) inDownloadDir(p string) bool {
	if s.DownloadDir == "" {
		return false
	}
	dd := filepath.Clean(s.DownloadDir)
	c := filepath.Clean(p)
	return c == dd || strings.HasPrefix(c, dd+string(filepath.Separator))
}

func (s *Scanner) skipSet() map[string]struct{} {
	skip := make(map[string]struct{}, len(s.SkipDirs)+3)
	add := func(d string) {
		if d != "" {
			skip[filepath.Clean(d)] = struct{}{}
		}
	}
	add(s.DownloadDir)
	for _, d := range s.SkipDirs {
		add(d)
	}
	if s.Resolver != nil {
		add(s.Resolver.FuseMount())
		add(s.Resolver.CacheRoot())
	}
	return skip
}
