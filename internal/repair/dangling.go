package repair

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/metrics"
)

// DanglingSweeper finds library symlinks into Lazarr's FUSE or cache tree whose release
// is gone from the catalog (v1.2.8). The repair scan only walks catalogued hashes, so
// these were invisible: 57 links / 8 hashes on sonarr_main on 2026-10-09, all from
// pre-1.2.8 deletes. An arr keeps the file record of a dangling link (it still shows in
// the directory listing), so nothing ever re-searches them.
//
// Report-only by default. With repair.unlink_orphans the links are removed (symlinks
// only, never followed) and the owning series/movie is rescanned, so the arr records the
// files as missing and its missing search re-grabs them. A hash must be seen dangling by
// two runs at least ConfirmGap apart before anything is unlinked.
type DanglingSweeper struct {
	Scanner *libscan.Scanner
	Store   catalog.Store
	// Unlink is repair.unlink_orphans.
	Unlink bool
	// ConfirmGap: a hash must already have been dangling this long ago.
	ConfirmGap time.Duration
	// MaxHashesPerRun caps unlinking per run.
	MaxHashesPerRun int
	// Rescan, when set, asks the owning arrs to rescan after links were removed.
	Rescan func(ctx context.Context, linkPaths []string)
	// PruneEmptied, when set (cleanup.remove_empty_dirs), removes the library folders
	// the unlinks left empty, before the rescan. Returns the number removed.
	PruneEmptied func(linkPaths []string) int
	Now          func() time.Time

	mu        sync.Mutex
	firstSeen map[string]time.Time // hash -> first run that saw it dangling
	last      DanglingReport
}

// DanglingHash is one rowless hash and its library links.
type DanglingHash struct {
	Hash      string    `json:"hash"`
	Links     []string  `json:"links"`
	FirstSeen time.Time `json:"first_seen"`
	Unlinked  bool      `json:"unlinked"`
}

// DanglingReport is one run's result (Web UI / metrics).
type DanglingReport struct {
	At         int64          `json:"at"` // unix; 0 = never ran
	Links      int            `json:"links"`
	Hashes     []DanglingHash `json:"hashes"`
	UnlinkMode bool           `json:"unlink_enabled"`
	Removed    int            `json:"removed"`
	// DirsRemoved: library folders left empty by the unlinks and removed
	// (cleanup.remove_empty_dirs).
	DirsRemoved int `json:"dirs_removed"`
}

// Run walks the library once.
func (s *DanglingSweeper) Run(ctx context.Context) (DanglingReport, error) {
	now := s.now()
	byHash := map[string][]string{}
	known := map[string]bool{}
	err := s.Scanner.Walk(ctx, func(l libscan.Link) {
		ok, seen := known[l.Hash]
		if !seen {
			_, _, gerr := s.Store.GetRelease(l.Hash)
			ok = !errors.Is(gerr, catalog.ErrNotFound) // store errors count as "known": never act on doubt
			known[l.Hash] = ok
		}
		if ok {
			return
		}
		// A cache-tree link whose bytes still exist plays fine; only report what is broken.
		if _, serr := os.Stat(l.Path); serr == nil {
			return
		}
		byHash[l.Hash] = append(byHash[l.Hash], l.Path)
	})
	if err != nil {
		return DanglingReport{}, err
	}

	s.mu.Lock()
	if s.firstSeen == nil {
		s.firstSeen = map[string]time.Time{}
	}
	for h := range s.firstSeen {
		if _, still := byHash[h]; !still {
			delete(s.firstSeen, h)
		}
	}
	rep := DanglingReport{At: now.Unix(), UnlinkMode: s.Unlink}
	hashes := make([]string, 0, len(byHash))
	for h := range byHash {
		hashes = append(hashes, h)
		if _, ok := s.firstSeen[h]; !ok {
			s.firstSeen[h] = now
		}
	}
	sort.Strings(hashes)
	for _, h := range hashes {
		links := byHash[h]
		sort.Strings(links)
		rep.Links += len(links)
		rep.Hashes = append(rep.Hashes, DanglingHash{Hash: h, Links: links, FirstSeen: s.firstSeen[h]})
	}
	s.mu.Unlock()

	if s.Unlink {
		var removed []string
		acted := 0
		for i := range rep.Hashes {
			dh := &rep.Hashes[i]
			if now.Sub(dh.FirstSeen) < s.ConfirmGap {
				continue
			}
			if s.MaxHashesPerRun > 0 && acted >= s.MaxHashesPerRun {
				break
			}
			acted++
			for _, p := range dh.Links {
				if err := removeSymlink(p); err != nil {
					slog.Warn("repair: unlink dangling library link", "link", p, "err", err)
					continue
				}
				removed = append(removed, p)
				dh.Unlinked = true
				slog.Warn("repair: removed dangling library link (release no longer in Lazarr)",
					"hash", dh.Hash, "link", p)
			}
		}
		rep.Removed = len(removed)
		if len(removed) > 0 && s.PruneEmptied != nil {
			rep.DirsRemoved = s.PruneEmptied(removed)
		}
		if len(removed) > 0 && s.Rescan != nil {
			s.Rescan(ctx, removed)
		}
	}

	metrics.SetDanglingLinks(rep.Links-rep.Removed, len(rep.Hashes))
	if rep.Links > 0 {
		slog.Warn("repair: dangling library links (release gone from Lazarr)",
			"links", rep.Links, "hashes", len(rep.Hashes), "removed", rep.Removed,
			"dirs_removed", rep.DirsRemoved, "unlink_enabled", s.Unlink)
	}
	s.mu.Lock()
	s.last = rep
	s.mu.Unlock()
	return rep, nil
}

// Last returns the most recent report.
func (s *DanglingSweeper) Last() DanglingReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// removeSymlink removes p only if it is (still) a symlink.
func removeSymlink(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return errors.New("not a symlink")
	}
	return os.Remove(p) //nolint:gosec // G122: Lstat-checked symlink, never followed
}

func (s *DanglingSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// OwnerOf returns the item whose folder contains p (longest matching folder prefix),
// or -1. Helper for the arr rescan mapping.
func OwnerOf(p string, folders map[string]int) int {
	best, id := "", -1
	for f, fid := range folders {
		f = filepath.Clean(f)
		if (p == f || strings.HasPrefix(p, f+string(filepath.Separator))) && len(f) > len(best) {
			best, id = f, fid
		}
	}
	return id
}
