package localcache

import (
	"errors"
	"io"
	"os"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
)

// touchEvery bounds catalog writes for last_read on hot local reads.
const touchEvery = 60 * time.Second

// ReadAt implements vfs.Materializer: complete files and the pulled prefix of
// in-progress files are served from disk; everything else goes to the inner engine
// exactly as before (docs/28 §4.2).
func (e *Engine) ReadAt(hash string, fileID int, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	key := fileKey{hash, fileID}
	e.mu.Lock()
	fs, ok := e.files[key]
	var (
		state    catalog.LocalState
		frontier int64
		rel      string
		lastRead int64
	)
	if ok {
		state, frontier, rel, lastRead = fs.State, fs.Frontier, fs.rel, fs.LastRead
	}
	e.mu.Unlock()
	if !ok {
		return e.inner.ReadAt(hash, fileID, p, off)
	}

	switch state {
	case catalog.LocalComplete:
		path, err := e.disk.path(hash, rel)
		if err == nil {
			n, rerr := preadFile(path, p, off)
			if rerr == nil {
				e.touchLocal(key, lastRead)
				return n, nil
			}
			e.markFailed(key, rerr)
		}
	case catalog.LocalPulling:
		if off+int64(len(p)) <= frontier {
			path, err := e.disk.partPath(hash, rel)
			if err == nil {
				if n, rerr := preadFile(path, p, off); rerr == nil {
					return n, nil
				}
			}
		}
	}
	return e.inner.ReadAt(hash, fileID, p, off)
}

// preadFile reads len(p) at off with short-read-at-EOF semantics.
func preadFile(path string, p []byte, off int64) (int, error) {
	f, err := os.Open(path) //nolint:gosec // path built from validated hash + rel_path
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	n, err := f.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return n, nil
}

// touchLocal stamps last_read (memory always, catalog at most every touchEvery) and
// keeps the release's "last played" honest without touching the TorBox idle clock.
func (e *Engine) touchLocal(key fileKey, lastRead int64) {
	now := e.now().Unix()
	if now-lastRead < int64(touchEvery.Seconds()) {
		return
	}
	e.mu.Lock()
	fs, ok := e.files[key]
	if ok {
		fs.LastRead = now
	}
	var row catalog.LocalFile
	if ok {
		row = fs.LocalFile
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	_ = e.store.UpsertLocalFile(&row)
	_ = e.store.TouchAccess(key.hash, now) // virtual release: idle reaper ignores it
}

// markFailed flips a file to failed (falling through to the inner engine on the same
// read) and retargets its links back to FUSE so playback keeps working.
func (e *Engine) markFailed(key fileKey, cause error) {
	e.mu.Lock()
	fs, ok := e.files[key]
	if !ok {
		e.mu.Unlock()
		return
	}
	fs.State = catalog.LocalFailed
	fs.Error = cause.Error()
	row := fs.LocalFile
	rel := fs.rel
	e.mu.Unlock()
	e.log.Warn("localcache: local read failed — falling back to streaming", "hash", short(key.hash), "file_id", key.fileID, "err", cause)
	if e.links != nil {
		if _, err := e.links.retarget(key.hash, key.fileID, rel, "fuse"); err != nil {
			e.log.Warn("localcache: retarget to fuse after failure", "err", err)
		}
	}
	row.Linked = false
	_ = e.store.UpsertLocalFile(&row)
}
