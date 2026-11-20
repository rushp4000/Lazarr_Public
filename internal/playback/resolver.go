package playback

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/your-username/lazarr/internal/config"
)

// TargetKind says which Lazarr tree a symlink points into.
const (
	TargetFuse  = "fuse"
	TargetCache = "cache"
)

// Ref is a library path resolved to a catalog file.
type Ref struct {
	LinkPath string // the symlink (after path_map)
	Target   string // its cleaned readlink target
	Kind     string // TargetFuse | TargetCache
	Hash     string // 40-hex infohash
	RelPath  string // path within the release (catalog file.rel_path)
}

var infohashRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ErrNotLazarr is returned for paths that are not symlinks into a Lazarr tree
// (e.g. a real-download library the media server also serves).
var ErrNotLazarr = errors.New("playback: path is not a Lazarr library link")

// Resolver joins media-server paths to (hash, rel_path) via readlink.
type Resolver struct {
	fuseMount string
	cacheRoot string // publish_at; "" when the cache is disabled
	pathMap   []config.PathMap
}

// NewResolver builds a resolver for the given roots. cacheRoot may be empty.
func NewResolver(fuseMount, cacheRoot string, pathMap []config.PathMap) *Resolver {
	r := &Resolver{fuseMount: cleanAbs(fuseMount), pathMap: pathMap}
	if cacheRoot != "" {
		r.cacheRoot = cleanAbs(cacheRoot)
	}
	return r
}

// FuseMount / CacheRoot expose the cleaned roots.
func (r *Resolver) FuseMount() string { return r.fuseMount }
func (r *Resolver) CacheRoot() string { return r.cacheRoot }

// MapPath rewrites a server-reported path through path_map (first match wins).
func (r *Resolver) MapPath(p string) string {
	for _, m := range r.pathMap {
		from := strings.TrimRight(m.From, "/")
		if p == from || strings.HasPrefix(p, from+"/") {
			return cleanAbs(strings.TrimRight(m.To, "/") + strings.TrimPrefix(p, from))
		}
	}
	return p
}

// ParseTarget classifies a symlink target: which root it is under, and its
// hash + rel_path. ok=false when it is not under either root or malformed.
func (r *Resolver) ParseTarget(target string) (kind, hash, relPath string, ok bool) {
	t := filepath.Clean(target)
	roots := []struct{ root, kind string }{{r.fuseMount, TargetFuse}}
	if r.cacheRoot != "" {
		roots = append(roots, struct{ root, kind string }{r.cacheRoot, TargetCache})
	}
	for _, rt := range roots {
		if rt.root == "" {
			continue
		}
		rest, found := strings.CutPrefix(t, rt.root+string(filepath.Separator))
		if !found {
			continue
		}
		h, rel, has := strings.Cut(rest, string(filepath.Separator))
		if !has || rel == "" || !infohashRE.MatchString(h) {
			return "", "", "", false
		}
		if rel != filepath.Clean(rel) || strings.HasPrefix(rel, "..") {
			return "", "", "", false
		}
		return rt.kind, h, rel, true
	}
	return "", "", "", false
}

// TargetFor builds the twin target path for a file under the given root kind.
func (r *Resolver) TargetFor(kind, hash, relPath string) string {
	root := r.fuseMount
	if kind == TargetCache {
		root = r.cacheRoot
	}
	return filepath.Join(root, hash, relPath)
}

// Resolve maps a media-server path to a catalog file by Lstat + Readlink. Returns
// ErrNotLazarr when the path is not a symlink into a Lazarr tree.
func (r *Resolver) Resolve(serverPath string) (*Ref, error) {
	p := r.MapPath(serverPath)
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("%w: %q is not absolute", ErrNotLazarr, serverPath)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("playback: lstat %q: %w", p, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return nil, fmt.Errorf("%w: %q is not a symlink", ErrNotLazarr, p)
	}
	target, err := os.Readlink(p)
	if err != nil {
		return nil, fmt.Errorf("playback: readlink %q: %w", p, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	kind, hash, rel, ok := r.ParseTarget(target)
	if !ok {
		return nil, fmt.Errorf("%w: %q -> %q", ErrNotLazarr, p, target)
	}
	return &Ref{LinkPath: p, Target: filepath.Clean(target), Kind: kind, Hash: hash, RelPath: rel}, nil
}

func cleanAbs(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}
