// Package arrroots maps library paths between Lazarr's view and each arr's own view.
//
// The arrs run in their own containers and see their library under their own mount
// points: e.g. sonarr_main's root folder "/tv" may be
// "/debrid_symlinks/TV Shows/sonarr_main" for Lazarr. Every arr API path (series and
// movie folders, root folders) must be translated before it is compared with a path
// Lazarr found on disk, and back before it is sent to the arr.
//
// The host side of the mapping is playback.arrs[].library_root. When it is not set it
// is auto-detected: the arr's series/movie folders, relative to its root folder, are
// looked up among the library links Lazarr knows about (libscan), and the parent
// folder that most of them share is the root. A detection that is not clear-cut is
// refused, and the arr gets no mapping (one WARN per arr): its rescans are skipped.
package arrroots

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/your-username/lazarr/internal/playback/arr"
)

// Client is the slice of an arr API client the mapping and the rescanner need.
type Client interface {
	RootFolders(ctx context.Context) ([]string, error)
	LibraryItems(ctx context.Context) ([]arr.LibraryItem, error)
	Rescan(ctx context.Context, id int) error
}

// Arr is one playback.arrs instance.
type Arr struct {
	Category string
	Client   Client
	// LibraryRoot is playback.arrs[].library_root: the host path of the arr's root
	// folder. "" = auto-detect.
	LibraryRoot string
}

// Mapping pairs an arr's root folder with the same folder as Lazarr sees it.
type Mapping struct {
	Category string `json:"category"`
	HostRoot string `json:"host_root"`
	ArrRoot  string `json:"arr_root"`
	Detected bool   `json:"detected"` // auto-detected, not configured
}

// ToHost translates an arr path below its root folder into Lazarr's view.
func (m Mapping) ToHost(arrPath string) (string, bool) {
	return rebase(arrPath, m.ArrRoot, m.HostRoot)
}

// ToArr translates a Lazarr path below the host root into the arr's view.
func (m Mapping) ToArr(hostPath string) (string, bool) {
	return rebase(hostPath, m.HostRoot, m.ArrRoot)
}

// rebase moves p from below from to below to. The arrs are Linux containers, so arr
// paths are always slash-separated.
func rebase(p, from, to string) (string, bool) {
	p, from = path.Clean(p), path.Clean(from)
	if p == from {
		return path.Clean(to), true
	}
	if from == "/" {
		return path.Join(to, p), true
	}
	if !strings.HasPrefix(p, from+"/") {
		return "", false
	}
	return path.Join(to, strings.TrimPrefix(p, from+"/")), true
}

// ErrUnresolved: the arr has no usable mapping (see the WARN logged for it).
var ErrUnresolved = errors.New("arr library root unresolved")

const (
	okTTL   = 6 * time.Hour // root folders rarely change
	failTTL = time.Hour     // retry an unresolved arr at most hourly
)

// Resolver resolves and caches one Mapping per arr.
type Resolver struct {
	Arrs []Arr
	// Walk calls fn for every library link Lazarr knows (libscan.Scanner.Walk). Needed
	// only for auto-detection; nil disables it.
	Walk func(ctx context.Context, fn func(linkPath string)) error
	Now  func() time.Time

	mu     sync.Mutex
	cache  map[string]cached
	warned map[string]bool
}

type cached struct {
	m   Mapping
	err error
	at  time.Time
}

// Mapping returns the arr's mapping, resolving it when the cached answer is stale.
func (r *Resolver) Mapping(ctx context.Context, category string) (Mapping, error) {
	a, ok := r.arr(category)
	if !ok {
		return Mapping{}, fmt.Errorf("%w: %s is not in playback.arrs", ErrUnresolved, category)
	}
	now := r.now()
	r.mu.Lock()
	c, hit := r.cache[category]
	r.mu.Unlock()
	if hit && ((c.err == nil && now.Sub(c.at) < okTTL) || (c.err != nil && now.Sub(c.at) < failTTL)) {
		return c.m, c.err
	}
	m, err := r.resolve(ctx, a)
	if err != nil && ctx.Err() != nil {
		return Mapping{}, err // shutting down: do not cache or warn
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache, r.warned = map[string]cached{}, map[string]bool{}
	}
	r.cache[category] = cached{m: m, err: err, at: now}
	switch {
	case err != nil && !r.warned[category]:
		r.warned[category] = true
		slog.Warn("arr library root unresolved: rescans after removed library links are disabled for this arr; set playback.arrs[].library_root",
			"arr", category, "err", err)
	case err == nil && (!hit || c.err != nil || c.m != m):
		r.warned[category] = false
		slog.Info("arr library root mapped", "arr", category, "arr_root", m.ArrRoot,
			"library_root", m.HostRoot, "auto_detected", m.Detected)
	}
	return m, err
}

// ResolveAll resolves every arr once (boot-time visibility of the mappings / WARNs).
func (r *Resolver) ResolveAll(ctx context.Context) {
	for _, a := range r.Arrs {
		_, _ = r.Mapping(ctx, a.Category)
	}
}

func (r *Resolver) resolve(ctx context.Context, a Arr) (Mapping, error) {
	roots, err := a.Client.RootFolders(ctx)
	if err != nil {
		return Mapping{}, fmt.Errorf("%w: list root folders: %w", ErrUnresolved, err)
	}
	if len(roots) == 0 {
		return Mapping{}, fmt.Errorf("%w: the arr has no root folder", ErrUnresolved)
	}
	if a.LibraryRoot != "" {
		host := filepath.Clean(a.LibraryRoot)
		arrRoot, err := pickRoot(roots, host)
		if err != nil {
			return Mapping{}, err
		}
		return Mapping{Category: a.Category, HostRoot: host, ArrRoot: arrRoot}, nil
	}
	return r.detect(ctx, a, roots)
}

// pickRoot chooses the arr root folder that library_root stands for: the only one, or
// the one with the same last path element.
func pickRoot(roots []string, host string) (string, error) {
	if len(roots) == 1 {
		return path.Clean(roots[0]), nil
	}
	var match []string
	for _, r := range roots {
		if path.Base(path.Clean(r)) == filepath.Base(host) {
			match = append(match, path.Clean(r))
		}
	}
	if len(match) != 1 {
		return "", fmt.Errorf("%w: the arr has %d root folders %v and none (or several) is named like library_root %s",
			ErrUnresolved, len(roots), roots, host)
	}
	return match[0], nil
}

// detect finds the host root: for every series/movie folder directly below an arr root
// folder, the library links under a host folder of that name vote for that folder's
// parent. The winner needs a clear majority (more than twice the runner-up), must
// exist on disk, and must not be another arr's configured library_root.
func (r *Resolver) detect(ctx context.Context, a Arr, roots []string) (Mapping, error) {
	if r.Walk == nil {
		return Mapping{}, fmt.Errorf("%w: library_root not set and auto-detection unavailable", ErrUnresolved)
	}
	if len(roots) != 1 {
		return Mapping{}, fmt.Errorf("%w: library_root not set and the arr has %d root folders %v", ErrUnresolved, len(roots), roots)
	}
	arrRoot := path.Clean(roots[0])
	items, err := a.Client.LibraryItems(ctx)
	if err != nil {
		return Mapping{}, fmt.Errorf("%w: list library: %w", ErrUnresolved, err)
	}
	names := map[string]bool{}
	for _, it := range items {
		if it.Path == "" {
			continue
		}
		p := path.Clean(it.Path)
		if path.Dir(p) == arrRoot { // only folders directly below the root folder
			names[path.Base(p)] = true
		}
	}
	if len(names) == 0 {
		return Mapping{}, fmt.Errorf("%w: library_root not set and the arr has no series/movie below %s", ErrUnresolved, arrRoot)
	}
	votes := map[string]map[string]bool{} // candidate host root -> item names seen under it
	err = r.Walk(ctx, func(link string) {
		for d := filepath.Dir(filepath.Clean(link)); d != filepath.Dir(d); d = filepath.Dir(d) {
			if names[filepath.Base(d)] {
				root := filepath.Dir(d)
				if votes[root] == nil {
					votes[root] = map[string]bool{}
				}
				votes[root][filepath.Base(d)] = true
				return
			}
		}
	})
	if err != nil {
		return Mapping{}, fmt.Errorf("%w: walk library: %w", ErrUnresolved, err)
	}
	best, bestN, second := "", 0, 0
	for root, v := range votes {
		switch n := len(v); {
		case n > bestN:
			best, bestN, second = root, n, bestN
		case n > second:
			second = n
		}
	}
	if bestN == 0 {
		return Mapping{}, fmt.Errorf("%w: library_root not set and no library link matches a series/movie folder of the arr", ErrUnresolved)
	}
	if bestN <= 2*second {
		return Mapping{}, fmt.Errorf("%w: library_root not set and auto-detection is ambiguous (%s: %d folders, runner-up: %d)",
			ErrUnresolved, best, bestN, second)
	}
	if fi, err := os.Stat(best); err != nil || !fi.IsDir() {
		return Mapping{}, fmt.Errorf("%w: auto-detected library root %s is not a folder", ErrUnresolved, best)
	}
	for _, o := range r.Arrs {
		if o.Category != a.Category && o.LibraryRoot != "" && filepath.Clean(o.LibraryRoot) == best {
			return Mapping{}, fmt.Errorf("%w: auto-detected library root %s is %s's library_root", ErrUnresolved, best, o.Category)
		}
	}
	return Mapping{Category: a.Category, HostRoot: best, ArrRoot: arrRoot, Detected: true}, nil
}

// Target is one series (Sonarr) or movie (Radarr) that owns removed library paths.
type Target struct {
	Category string
	ItemID   int
	// ArrPaths are the removed paths as the arr sees them (its root folder view), so
	// they can be matched against the arr's own file records.
	ArrPaths []string
}

// Rescan asks each arr to rescan the series/movies that own the given (Lazarr-side)
// library paths. An arr without a mapping is skipped (its WARN was logged once).
func (r *Resolver) Rescan(ctx context.Context, paths []string) {
	r.RescanTargets(ctx, r.Targets(ctx, paths))
}

// Targets maps Lazarr-side library paths to the series/movies that own them, per arr.
// Paths outside every mapped arr, or below no known series/movie folder, are dropped.
func (r *Resolver) Targets(ctx context.Context, paths []string) []Target {
	var out []Target
	for _, a := range r.Arrs {
		m, err := r.Mapping(ctx, a.Category)
		if err != nil {
			continue
		}
		var mine []string
		for _, p := range paths {
			if _, ok := m.ToArr(p); ok {
				mine = append(mine, filepath.Clean(p))
			}
		}
		if len(mine) == 0 {
			continue
		}
		items, err := a.Client.LibraryItems(ctx)
		if err != nil {
			slog.Warn("repair: rescan: list library", "arr", a.Category, "err", err)
			continue
		}
		folders := make(map[string]int, len(items)) // host path -> id
		for _, it := range items {
			if hp, ok := m.ToHost(it.Path); ok && it.Path != "" {
				folders[hp] = it.ID
			}
		}
		idx := map[int]int{} // item id -> index in out
		for _, p := range mine {
			id := ownerOf(p, folders)
			if id < 0 {
				continue
			}
			ap, _ := m.ToArr(p)
			i, seen := idx[id]
			if !seen {
				i = len(out)
				idx[id] = i
				out = append(out, Target{Category: a.Category, ItemID: id})
			}
			out[i].ArrPaths = append(out[i].ArrPaths, ap)
		}
	}
	return out
}

// RescanTargets asks each target's arr to rescan that series/movie and returns the
// targets whose rescan command the arr accepted.
func (r *Resolver) RescanTargets(ctx context.Context, targets []Target) []Target {
	var ok []Target
	for _, t := range targets {
		a, found := r.arr(t.Category)
		if !found {
			continue
		}
		if err := a.Client.Rescan(ctx, t.ItemID); err != nil {
			slog.Warn("repair: rescan", "arr", t.Category, "id", t.ItemID, "err", err)
			continue
		}
		slog.Info("repair: asked arr to rescan after removing library links", "arr", t.Category, "id", t.ItemID)
		ok = append(ok, t)
	}
	return ok
}

// ownerOf returns the item whose folder contains p (longest matching prefix), or -1.
func ownerOf(p string, folders map[string]int) int {
	best, id := "", -1
	for f, fid := range folders {
		if (p == f || strings.HasPrefix(p, f+string(filepath.Separator))) && len(f) > len(best) {
			best, id = f, fid
		}
	}
	return id
}

func (r *Resolver) arr(category string) (Arr, bool) {
	for _, a := range r.Arrs {
		if a.Category == category && a.Client != nil {
			return a, true
		}
	}
	return Arr{}, false
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
