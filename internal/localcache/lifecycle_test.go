package localcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback"
)

// These tests drive the reaper the way production does: a link-index walk every
// link_index_interval and a sweep in between, while an arr renames, upgrades or
// deletes the library link of a cached file.

// tick advances the fake clock by d in link_index_interval steps, walking and
// sweeping at each step like runReaper does.
func (h *harness) tick(ctx context.Context, d time.Duration) {
	h.t.Helper()
	step := h.cfg.LinkIndexInterval.D()
	if step <= 0 {
		step = 10 * time.Minute
	}
	for elapsed := time.Duration(0); elapsed < d; elapsed += step {
		h.advance(step)
		h.eng.walkLinks()
		h.eng.Sweep(ctx)
	}
}

// cachedWithLink pulls hashA ("ep1.mkv") to the cache and waits until its library
// link points at the cache copy. Returns the link and the cache file path.
func cachedWithLink(t *testing.T, h *harness, ctx context.Context) (link, cacheFile string) {
	t.Helper()
	link = h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	cacheFile = filepath.Join(h.cfg.Dir, hashA, "ep1.mkv")
	waitLink(t, link, cacheFile)
	return link, cacheFile
}

// addDownloadLink creates Lazarr's own grab-tree symlink for hashA (download_dir),
// which an attached release keeps while the arr has not removed the download
// (removeCompletedDownloads=false in a typical setup).
func addDownloadLink(t *testing.T, h *harness) string {
	t.Helper()
	dir := filepath.Join(h.root, "lazarr", "sonarr_main", "Show")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	dl := filepath.Join(dir, "ep1.mkv")
	require.NoError(t, os.Symlink(filepath.Join(h.root, "lazarr_mount", hashA, "ep1.mkv"), dl))
	return dl
}

func gone(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	return errors.Is(err, os.ErrNotExist)
}

// (b)/(c) Upgrade or delete: the arr removes the only library link. With the walk
// running every 10 minutes (as in production), the file must still be evicted once
// unreferenced_grace has passed — the grace clock must not restart on every walk.
func TestUnreferencedEvictedWithRegularWalks(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, cacheFile := cachedWithLink(t, h, ctx)

	require.NoError(t, os.Remove(link)) // Sonarr upgrade / delete-with-files
	h.tick(ctx, h.cfg.UnreferencedGrace.D()/2)
	_, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err, "inside the grace window it stays")

	h.tick(ctx, h.cfg.UnreferencedGrace.D()+h.cfg.LinkIndexInterval.D())
	_, err = h.store.GetLocalFile(hashA, 1)
	require.ErrorIs(t, err, catalog.ErrNotFound, "unreferenced file evicted after the grace")
	assert.True(t, gone(t, cacheFile), "cache bytes removed")
}

// Upgrade while the old release is still attached in the arr's client: Lazarr's own
// download_dir symlink survives, but it is not a library reference — the cached
// copy must still go after the grace, and the download_dir link must be put back
// on FUSE (never left dangling).
func TestUpgradeWithDownloadDirLinkStillEvicts(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dl := addDownloadLink(t, h)
	link, cacheFile := cachedWithLink(t, h, ctx)
	waitLink(t, dl, cacheFile)

	require.NoError(t, os.Remove(link)) // upgrade: old library link deleted
	h.tick(ctx, h.cfg.UnreferencedGrace.D()+2*h.cfg.LinkIndexInterval.D())

	_, err := h.store.GetLocalFile(hashA, 1)
	require.ErrorIs(t, err, catalog.ErrNotFound)
	assert.True(t, gone(t, cacheFile))
	assert.Equal(t, filepath.Join(h.root, "lazarr_mount", hashA, "ep1.mkv"), readlink(t, dl),
		"download_dir link back on FUSE")
}

// A grab that is pulled before the arr imports it (pull_on_grab) has only its
// download_dir link; it must survive while the import is within the grace.
func TestDownloadDirOnlyKeptInsideGrace(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dl := addDownloadLink(t, h)
	link, cacheFile := cachedWithLink(t, h, ctx)
	require.NoError(t, os.Remove(link)) // not imported yet
	h.tick(ctx, h.cfg.UnreferencedGrace.D()/2)
	// The arr imports now: a new library link pointing at the download_dir link's
	// target (cache), in a different folder.
	newDir := filepath.Join(h.root, "TV Shows", "sonarr_main", "Show (2026)", "Season 1")
	require.NoError(t, os.MkdirAll(newDir, 0o755))
	imported := filepath.Join(newDir, "Show - S01E01.mkv")
	require.NoError(t, os.Symlink(readlink(t, dl), imported))
	h.tick(ctx, 3*h.cfg.UnreferencedGrace.D())
	_, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err, "imported before the grace ran out: kept")
	assert.Equal(t, cacheFile, readlink(t, imported))
}

// (a) Rename: the arr renames/moves the library link. The walk must follow it, the
// file must never be called unreferenced, and the renamed link keeps the cache.
func TestRenamedLinkKeepsCacheCopy(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, cacheFile := cachedWithLink(t, h, ctx)

	newDir := filepath.Join(h.root, "TV Shows", "sonarr_main", "Show (2026)", "Season 1")
	require.NoError(t, os.MkdirAll(newDir, 0o755))
	renamed := filepath.Join(newDir, "Show - S01E01 - Pilot WEBDL-1080p.mkv")
	require.NoError(t, os.Rename(link, renamed)) // rename(2) keeps the symlink as-is

	h.tick(ctx, 3*h.cfg.UnreferencedGrace.D())
	lf, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err)
	assert.NotEqual(t, "unreferenced", lf.Class)
	assert.Equal(t, cacheFile, readlink(t, renamed))
	links, _ := h.store.ListLibraryLinks(hashA, 1)
	require.Len(t, links, 1)
	assert.Equal(t, renamed, links[0].LinkPath, "index follows the rename, old row pruned")
	assert.Equal(t, playback.TargetCache, links[0].TargetKind)
}

// Rename right before an eviction: Sweep re-walks before evicting, so the renamed
// link (not yet in the index) is put back on FUSE, never left dangling.
func TestRenameBeforeEvictionRetargetsRenamedLink(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, cacheFile := cachedWithLink(t, h, ctx)
	require.NoError(t, h.store.UpsertWatch(&catalog.Watch{Server: "jellyfin", UserID: "u1", Hash: hashA, FileID: 1, SeriesKey: "s", Completed: true, FirstSeen: h.now.Unix(), LastSeen: h.now.Unix()}))

	renamed := filepath.Join(filepath.Dir(link), "renamed.mkv")
	require.NoError(t, os.Rename(link, renamed))
	h.advance(h.cfg.TTL.Finished.D() + time.Hour) // finished TTL expired, no walk yet
	h.eng.Sweep(ctx)

	_, err := h.store.GetLocalFile(hashA, 1)
	require.ErrorIs(t, err, catalog.ErrNotFound)
	assert.True(t, gone(t, cacheFile))
	assert.Equal(t, filepath.Join(h.root, "lazarr_mount", hashA, "ep1.mkv"), readlink(t, renamed))
}

// A walk that could not read part of the library must not prune index rows: the
// links it did not see may still exist, and pruning them would start the
// unreferenced clock on a file that is in use.
func TestIncompleteWalkDoesNotPrune(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = cachedWithLink(t, h, ctx)
	links, _ := h.store.ListLibraryLinks(hashA, 1)
	require.Len(t, links, 1)

	// A root below a regular file cannot be walked (ENOTDIR, not ENOENT).
	blocker := filepath.Join(h.root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
	h.eng.links.roots = append(h.eng.links.roots, filepath.Join(blocker, "sub"))
	// Make the real link invisible to this walk by moving the library away.
	tv := filepath.Join(h.root, "TV Shows")
	require.NoError(t, os.Rename(tv, tv+".off"))
	h.advance(h.cfg.LinkIndexInterval.D())
	_, _, err := h.eng.links.walk()
	require.Error(t, err)
	links, _ = h.store.ListLibraryLinks(hashA, 1)
	assert.Len(t, links, 1, "incomplete walk keeps the rows")
}
