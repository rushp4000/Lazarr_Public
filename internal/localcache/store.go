package localcache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// partSuffix marks an in-progress pull. Chosen so it can never collide with a
// catalog rel_path (those come from torrent file lists, which never end this way).
const partSuffix = ".lazarr-part"

var infohashRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// diskStore is the on-disk layout: <dir>/<infohash>/<rel_path>[.lazarr-part].
// Every path it builds is derived from a validated hash and a rel_path that
// symlink.safeRelPath accepted at grab time; nothing else reaches the filesystem.
type diskStore struct {
	dir string
}

func newDiskStore(dir string) (*diskStore, error) {
	if dir == "" {
		return nil, errors.New("localcache: empty dir")
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // cache tree must be world-readable (Plex/arrs read it)
		return nil, fmt.Errorf("localcache: mkdir %s: %w", dir, err)
	}
	return &diskStore{dir: dir}, nil
}

// safeRel mirrors symlink.safeRelPath: relative, cleaned, no parent traversal.
func safeRel(rel string) (string, error) {
	if rel == "" {
		return "", errors.New("empty rel_path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("rel_path %q is absolute", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == "." {
		return "", fmt.Errorf("rel_path %q traverses parent directory", rel)
	}
	return clean, nil
}

// path returns the final (complete) path for a file, validating both components.
func (d *diskStore) path(hash, rel string) (string, error) {
	if !infohashRE.MatchString(hash) {
		return "", fmt.Errorf("localcache: bad hash %q", hash)
	}
	clean, err := safeRel(rel)
	if err != nil {
		return "", fmt.Errorf("localcache: %w", err)
	}
	p := filepath.Join(d.dir, hash, clean)
	if !strings.HasPrefix(p, d.dir+string(filepath.Separator)) {
		return "", fmt.Errorf("localcache: path %q escapes %s", p, d.dir)
	}
	return p, nil
}

func (d *diskStore) partPath(hash, rel string) (string, error) {
	p, err := d.path(hash, rel)
	if err != nil {
		return "", err
	}
	return p + partSuffix, nil
}

// openPart opens (creating parents) the .part file for appending at `frontier`.
// If the existing part is longer than the recorded frontier it is truncated back to
// it (the tail past the durable frontier is not trusted after a crash).
func (d *diskStore) openPart(hash, rel string, frontier int64) (*os.File, error) {
	p, err := d.partPath(hash, rel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // see newDiskStore
		return nil, fmt.Errorf("localcache: mkdir %s: %w", filepath.Dir(p), err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644) //nolint:gosec // world-readable by design
	if err != nil {
		return nil, fmt.Errorf("localcache: open part %s: %w", p, err)
	}
	if err := f.Truncate(frontier); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("localcache: truncate part %s to %d: %w", p, frontier, err)
	}
	if _, err := f.Seek(frontier, 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("localcache: seek part %s: %w", p, err)
	}
	return f, nil
}

// finalize fsyncs and renames the part into place, then stamps mtime.
func (d *diskStore) finalize(f *os.File, hash, rel string, mtime time.Time) (string, error) {
	final, err := d.path(hash, rel)
	if err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("localcache: fsync %s: %w", f.Name(), err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("localcache: close %s: %w", f.Name(), err)
	}
	if err := os.Rename(final+partSuffix, final); err != nil {
		return "", fmt.Errorf("localcache: rename into place %s: %w", final, err)
	}
	// Best-effort: CIFS with forced uid may refuse utimes; a mismatch only means the
	// media server may re-analyse the file once.
	_ = os.Chtimes(final, mtime, mtime)
	return final, nil
}

// remove deletes the complete file and any part file, then prunes empty parents up
// to (not including) the store root. Missing files are fine.
func (d *diskStore) remove(hash, rel string) error {
	p, err := d.path(hash, rel)
	if err != nil {
		return err
	}
	var errs error
	for _, target := range []string{p, p + partSuffix} {
		if rerr := os.Remove(target); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			errs = errors.Join(errs, fmt.Errorf("localcache: remove %s: %w", target, rerr))
		}
	}
	d.pruneEmpty(filepath.Dir(p))
	return errs
}

// pruneEmpty removes empty directories from dir upward, stopping at the store root.
func (d *diskStore) pruneEmpty(dir string) {
	for strings.HasPrefix(dir, d.dir+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil {
			return // not empty or gone
		}
		dir = filepath.Dir(dir)
	}
}

// removeHash deletes an entire <hash> subtree (used when a release is forgotten).
func (d *diskStore) removeHash(hash string) error {
	if !infohashRE.MatchString(hash) {
		return fmt.Errorf("localcache: bad hash %q", hash)
	}
	return os.RemoveAll(filepath.Join(d.dir, hash))
}

// stat returns the size of the complete file (ok=false when absent).
func (d *diskStore) stat(hash, rel string) (size int64, ok bool) {
	p, err := d.path(hash, rel)
	if err != nil {
		return 0, false
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	return fi.Size(), true
}

// hashDirs lists the <infohash> directories present on disk (for the orphan sweep).
func (d *diskStore) hashDirs() ([]string, error) {
	ents, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, fmt.Errorf("localcache: readdir %s: %w", d.dir, err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && infohashRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// disk reports free/total bytes of the volume holding the store.
func (d *diskStore) disk() (DiskStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(d.dir, &st); err != nil {
		return DiskStat{}, fmt.Errorf("localcache: statfs %s: %w", d.dir, err)
	}
	bs := uint64(st.Bsize) //nolint:gosec // block size is positive
	return DiskStat{Free: st.Bavail * bs, Total: st.Blocks * bs}, nil
}
