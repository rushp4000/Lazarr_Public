package localcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback"
)

// The 2026-10-09 incident: library links pointed into the cache tree after the
// cache bytes (and their local_file rows) were gone; Plex trashed the items. These
// tests pin every path that could leave a link outliving its file.

// pullAndLink pulls hashA to completion and waits for its link to point at the cache.
func pullAndLink(t *testing.T, h *harness, ctx context.Context) (link, cacheFile, fuseFile string) {
	t.Helper()
	link = h.addRelease(hashA, "sonarr_main", "Show", "Show.S01/ep1.mkv", 1<<20, catalog.SourceTorrent)
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	cacheFile = filepath.Join(h.cfg.Dir, hashA, "Show.S01/ep1.mkv")
	fuseFile = filepath.Join(h.root, "lazarr_mount", hashA, "Show.S01/ep1.mkv")
	waitLink(t, link, cacheFile)
	return link, cacheFile, fuseFile
}

func (h *harness) freshEngine(t *testing.T) *Engine {
	t.Helper()
	eng, err := New(Deps{Store: h.store, Inner: h.inner, Cache: h.cfg, Playback: h.pb,
		Paths:      config.Paths{DownloadDir: filepath.Join(h.root, "lazarr"), FuseMount: filepath.Join(h.root, "lazarr_mount")},
		Categories: []string{"sonarr_main"}, PublishFn: func(string, string) error { return nil }})
	require.NoError(t, err)
	eng.now = h.eng.now
	eng.links.now = eng.now
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func TestWalkRepairsDanglingCacheLink(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, cacheFile, fuseFile := pullAndLink(t, h, ctx)

	// Bytes vanish behind Lazarr's back (volume swapped, manual delete, …).
	require.NoError(t, os.Remove(cacheFile))
	_, repaired, err := h.eng.links.walk()
	require.NoError(t, err)
	assert.Equal(t, 1, repaired)

	assert.Equal(t, fuseFile, readlink(t, link), "dangling cache link put back on FUSE")
	links, _ := h.store.ListLibraryLinks(hashA, 1)
	require.Len(t, links, 1)
	assert.Equal(t, playback.TargetFuse, links[0].TargetKind)
}

func TestWalkRepairsCacheLinkOfForgottenRelease(t *testing.T) {
	h := newHarness(t, nil)
	// A link into the cache tree for a release the catalog no longer knows.
	dir := filepath.Join(h.root, "TV Shows", "sonarr_main", "Gone")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	link := filepath.Join(dir, "e.mkv")
	require.NoError(t, os.Symlink(filepath.Join(h.cfg.Dir, hashB, "rel", "e.mkv"), link))
	h.eng.walkLinks()
	assert.Equal(t, filepath.Join(h.root, "lazarr_mount", hashB, "rel", "e.mkv"), readlink(t, link))
}

func TestBootWithMissingCacheFileRepairsLinks(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, cacheFile, fuseFile := pullAndLink(t, h, ctx)
	require.NoError(t, h.eng.Close())

	// Reboot with the cache file unreadable (the incident's starting point).
	require.NoError(t, os.RemoveAll(filepath.Dir(filepath.Dir(cacheFile))))
	eng := h.freshEngine(t)
	_, err := h.store.GetLocalFile(hashA, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound, "row for a missing file is dropped at load")
	eng.walkLinks() // what runReaper does first on boot
	assert.Equal(t, fuseFile, readlink(t, link))
}

func TestEvictKeepsBytesWhenRetargetFails(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link, cacheFile, _ := pullAndLink(t, h, ctx)

	// Block the atomic swap: a non-empty directory squats on the temp name, so both
	// the pre-clean Remove and the Symlink fail.
	squat := link + tmpSuffix
	require.NoError(t, os.MkdirAll(filepath.Join(squat, "x"), 0o755))
	defer func() { _ = os.RemoveAll(squat) }()

	err := h.eng.evict(fileKey{hashA, 1}, "finished", "ttl")
	require.Error(t, err)
	_, statErr := os.Stat(cacheFile)
	assert.NoError(t, statErr, "bytes kept while a link still points at them")
	assert.Equal(t, cacheFile, readlink(t, link))
	lf, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err)
	assert.Equal(t, catalog.LocalComplete, lf.State)
}

func TestOrphanSweepKeepsDirsWithCatalogRows(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "sonarr_main", "A", "a.mkv", 1024, catalog.SourceTorrent)
	// A dir whose row exists in the catalog but not in memory (e.g. written by a
	// previous process mid-boot) is not an orphan.
	require.NoError(t, os.MkdirAll(filepath.Join(h.cfg.Dir, hashA), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.cfg.Dir, hashA, "a.mkv.lazarr-part"), []byte("p"), 0o644))
	require.NoError(t, h.store.UpsertLocalFile(&catalog.LocalFile{Hash: hashA, FileID: 1, State: catalog.LocalPulling, Size: 1024, Frontier: 1}))
	h.eng.sweepOrphans()
	_, err := os.Stat(filepath.Join(h.cfg.Dir, hashA))
	assert.NoError(t, err)
}

func TestPublishFailureNeverLinksButStillRepairs(t *testing.T) {
	h := newHarness(t, nil)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	// A pre-existing dangling cache link from an earlier run.
	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(filepath.Join(h.cfg.Dir, hashA, "ep1.mkv"), link))
	h.eng.publish = func(string, string) error { return os.ErrPermission }
	h.eng.cfg.Publish = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	fuse := filepath.Join(h.root, "lazarr_mount", hashA, "ep1.mkv")
	assert.Equal(t, fuse, readlink(t, link), "repaired even though linking is off")

	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	h.eng.walkLinks()
	assert.Equal(t, fuse, readlink(t, link), "never retargeted into an unpublished tree")
	assert.False(t, h.eng.Snapshot().Linking)
}
