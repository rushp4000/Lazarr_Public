// Package repair acts on the repair scan's findings.
package repair

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/materialize"
)

// Unlinker removes library symlinks whose FUSE target belongs to a release TorBox
// has evicted. Each such link is a file the arr believes it has but that fails every
// read with EIO, so the arr never searches for it again. With the link gone, the
// arr's next disk rescan marks the episode/movie missing and its normal missing
// search (or Cleanuparr's Seeker) re-grabs it.
//
// Only symlinks are removed, never regular files, and only those pointing straight
// into the FUSE tree at a confirmed-dead hash. Links into the local-cache tree are
// left alone (the bytes are on local disk, so they still play).
type Unlinker struct {
	// Roots are the library directories to walk (local_cache.library_roots).
	Roots []string
	// FuseMount is paths.fuse_mount; a link is a candidate only when its target is
	// <FuseMount>/<dead hash>/....
	FuseMount string
	// SkipDirs are never descended into: Lazarr's own download dir, the FUSE mount
	// and the cache trees.
	SkipDirs []string
	// ConfirmGap is how long before the current scan an earlier scan must already
	// have seen the hash evicted. One checkcached miss is never enough.
	ConfirmGap time.Duration
	// MaxPerRun caps how many dead releases one run may unlink (blast radius if
	// TorBox's checkcached ever misreports en masse).
	MaxPerRun int
	Now       func() time.Time
}

// Result summarizes one run.
type Result struct {
	Confirmed int      // dead releases eligible this run (after the cap)
	Deferred  int      // eligible but over MaxPerRun; picked up next run
	Removed   []string // library link paths removed
}

// Confirmed filters scan entries down to evictions safe to act on: seen evicted by
// an earlier scan at least ConfirmGap ago, and not currently on the account.
func (u *Unlinker) Confirmed(entries []materialize.RepairEntry) []string {
	now := u.now().Unix()
	var dead []string
	for _, e := range entries {
		if e.PrevEvictedAt == 0 || now-e.PrevEvictedAt < int64(u.ConfirmGap/time.Second) {
			continue
		}
		if e.State == catalog.StateMaterialized || e.State == catalog.StateDownloading {
			continue // on the account: readable regardless of checkcached
		}
		dead = append(dead, e.Hash)
	}
	slices.Sort(dead)
	return dead
}

// Run removes the library links of every confirmed-dead release in entries.
func (u *Unlinker) Run(ctx context.Context, entries []materialize.RepairEntry) (Result, error) {
	var res Result
	dead := u.Confirmed(entries)
	if u.MaxPerRun > 0 && len(dead) > u.MaxPerRun {
		res.Deferred = len(dead) - u.MaxPerRun
		dead = dead[:u.MaxPerRun]
	}
	res.Confirmed = len(dead)
	if len(dead) == 0 || u.FuseMount == "" {
		return res, nil
	}
	deadSet := make(map[string]struct{}, len(dead))
	for _, h := range dead {
		deadSet[h] = struct{}{}
	}
	fuse := filepath.Clean(u.FuseMount)
	skip := make(map[string]struct{}, len(u.SkipDirs)+1)
	skip[fuse] = struct{}{}
	for _, d := range u.SkipDirs {
		if d != "" {
			skip[filepath.Clean(d)] = struct{}{}
		}
	}

	var errs error
	for _, root := range u.Roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if werr != nil {
				if p == root && errors.Is(werr, fs.ErrNotExist) {
					return filepath.SkipDir
				}
				return nil // unreadable entry: skip, keep walking
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
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			rest, ok := strings.CutPrefix(filepath.Clean(target), fuse+string(filepath.Separator))
			if !ok {
				return nil
			}
			hash, _, _ := strings.Cut(rest, string(filepath.Separator))
			if _, isDead := deadSet[hash]; !isDead {
				return nil
			}
			// Removing a symlink never follows it; WalkDir reported it as a symlink.
			if err := os.Remove(p); err != nil { //nolint:gosec // G122: symlink only, see above
				errs = errors.Join(errs, err)
				return nil
			}
			res.Removed = append(res.Removed, p)
			slog.Warn("repair: removed dead library link — the arr will see the file missing on its next rescan",
				"hash", hash, "link", p)
			return nil
		})
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return res, errs
}

func (u *Unlinker) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}
