package qbit_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/playback"
	"github.com/your-username/lazarr/internal/qbit"
	"github.com/your-username/lazarr/internal/torbox"
	"github.com/your-username/lazarr/internal/vfs"
)

// fakeLinks is a qbit.LinkFinder with canned answers per hash.
type fakeLinks struct {
	links map[string][]libscan.Link
	err   error
	calls []string
}

func (f *fakeLinks) LinksTo(_ context.Context, hash string) ([]libscan.Link, error) {
	f.calls = append(f.calls, hash)
	if f.err != nil {
		return nil, f.err
	}
	return f.links[hash], nil
}

type detachEnv struct {
	*testEnv
	links *fakeLinks
	eng   *fakeReleaser
	srv   qbit.Server
}

func newDetachEnv(t *testing.T, detach bool) *detachEnv {
	t.Helper()
	cfg := config.Default()
	cfg.Paths.DownloadDir = "/data/symlinks"
	cfg.Paths.FuseMount = "/data/torbox"
	cfg.Categories = []string{"radarr_hq", "sonarr_main"}
	cfg.Policy.DetachOnDelete = detach
	st := newFakeStore()
	tb := &fakeTorBox{}
	sl := &fakeSymlink{}
	fl := &fakeLinks{links: map[string][]libscan.Link{}}
	eng := &fakeReleaser{}
	srv := qbit.New(qbit.Deps{Config: cfg, Store: st, TorBox: tb, Symlink: sl, Engine: eng, Links: fl})
	return &detachEnv{
		testEnv: &testEnv{store: st, torbox: tb, symlink: sl, handler: srv, cfg: cfg},
		links:   fl, eng: eng, srv: srv,
	}
}

func (e *detachEnv) add(hash, cat string) {
	body, ct := formBody("urls", magnetURI(hash, "Release"), "category", cat)
	e.do("POST", "/api/v2/torrents/add", body, ct)
}

func (e *detachEnv) del(hash string) int {
	body, ct := formBody("hashes", hash, "deleteFiles", "true")
	return e.do("POST", "/api/v2/torrents/delete", body, ct).Code
}

func (e *detachEnv) infoCount(query string) int {
	rec := e.do("GET", "/api/v2/torrents/info"+query, nil, "")
	var arr []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &arr)
	return len(arr)
}

func libLink(hash string) libscan.Link {
	return libscan.Link{Path: "/data/TV Shows/Show/S01E01.mkv", Kind: playback.TargetFuse, Hash: hash, RelPath: "e1.mkv"}
}

// Delete with library links: the row and its files stay (FUSE keeps serving them),
// only the download_dir tree goes, and the arr no longer sees the torrent.
func TestDelete_WithLibraryLinks_Detaches(t *testing.T) {
	e := newDetachEnv(t, true)
	e.add(cachedHash, "radarr_hq")
	e.links.links[cachedHash] = []libscan.Link{libLink(cachedHash), libLink(cachedHash)}

	require.Equal(t, http.StatusOK, e.del(cachedHash))

	rel, files, err := e.store.GetRelease(cachedHash)
	require.NoError(t, err, "row must survive")
	assert.NotZero(t, rel.DetachedAt)
	assert.Len(t, files, 3, "file rows (what FUSE serves) must survive")
	assert.NotContains(t, e.store.deleted, cachedHash)
	assert.Contains(t, e.symlink.removed, cachedHash, "download_dir tree is removed")
	assert.Equal(t, []string{cachedHash}, e.eng.released, "idle TorBox copy is released")

	// Invisible on every qBit read path.
	assert.Zero(t, e.infoCount("?category=radarr_hq"))
	assert.Zero(t, e.infoCount("?hashes="+cachedHash))
	assert.Zero(t, e.infoCount(""))
	assert.Equal(t, http.StatusNotFound, e.do("GET", "/api/v2/torrents/properties?hash="+cachedHash, nil, "").Code)
	assert.Equal(t, http.StatusNotFound, e.do("GET", "/api/v2/torrents/files?hash="+cachedHash, nil, "").Code)

	// A re-grab of the same hash makes it visible again.
	e.add(cachedHash, "radarr_hq")
	rel, _, _ = e.store.GetRelease(cachedHash)
	assert.Zero(t, rel.DetachedAt)
	assert.Equal(t, 1, e.infoCount("?category=radarr_hq"))
}

// Delete without library links: unchanged pre-1.2.8 behavior (row removed).
func TestDelete_WithoutLibraryLinks_Purges(t *testing.T) {
	e := newDetachEnv(t, true)
	e.add(cachedHash, "radarr_hq")
	require.Equal(t, http.StatusOK, e.del(cachedHash))
	assert.Contains(t, e.store.deleted, cachedHash)
	assert.Equal(t, []string{cachedHash}, e.links.calls)
}

// The link check failing (walk error / timeout) is "unknown": keep the row.
func TestDelete_LinkCheckError_KeepsRow(t *testing.T) {
	e := newDetachEnv(t, true)
	e.add(cachedHash, "radarr_hq")
	e.links.err = errors.New("walk: permission denied")
	require.Equal(t, http.StatusOK, e.del(cachedHash))
	rel, _, err := e.store.GetRelease(cachedHash)
	require.NoError(t, err)
	assert.NotZero(t, rel.DetachedAt)
}

// A link chained through download_dir would break if the download tree went.
func TestDelete_ChainedLink_KeepsDownloadTree(t *testing.T) {
	e := newDetachEnv(t, true)
	e.add(cachedHash, "radarr_hq")
	l := libLink(cachedHash)
	l.Chained = true
	e.links.links[cachedHash] = []libscan.Link{l}
	require.Equal(t, http.StatusOK, e.del(cachedHash))
	assert.NotContains(t, e.symlink.removed, cachedHash)
}

// policy.detach_on_delete=false is the old behavior even with links.
func TestDelete_DetachDisabled_Purges(t *testing.T) {
	e := newDetachEnv(t, false)
	e.add(cachedHash, "radarr_hq")
	e.links.links[cachedHash] = []libscan.Link{libLink(cachedHash)}
	require.Equal(t, http.StatusOK, e.del(cachedHash))
	assert.Contains(t, e.store.deleted, cachedHash)
	assert.Empty(t, e.links.calls)
}

// A second delete of a detached release whose links are gone removes it.
func TestDelete_RepeatAfterLinksGone_Purges(t *testing.T) {
	e := newDetachEnv(t, true)
	e.add(cachedHash, "radarr_hq")
	e.links.links[cachedHash] = []libscan.Link{libLink(cachedHash)}
	e.del(cachedHash)
	require.NotContains(t, e.store.deleted, cachedHash)
	delete(e.links.links, cachedHash)
	e.del(cachedHash)
	assert.Contains(t, e.store.deleted, cachedHash)
}

// Deleting a downloading (on_cache_miss=wait) release removes the TorBox transfer
// exactly once and then the row — the v1.2.7 leak.
// addWaitMiss starts an on_cache_miss=wait download for uncachedHash (torbox id 9001).
func addWaitMiss(t *testing.T) (*testEnv, qbit.Server) {
	t.Helper()
	e := newWaitEnv("wait")
	e.torbox.createOK = true
	body, ct := formBody("urls", magnetURI(uncachedHash, "Waited+Release"), "category", "radarr_hq")
	require.Equal(t, "Ok.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
	return e, e.handler.(qbit.Server)
}

func TestDelete_Downloading_ControlDeletesOnce(t *testing.T) {
	e, _ := addWaitMiss(t)
	require.Equal(t, catalog.StateDownloading, e.store.releases[uncachedHash].State)

	body, ct := formBody("hashes", uncachedHash)
	require.Equal(t, http.StatusOK, e.do("POST", "/api/v2/torrents/delete", body, ct).Code)

	assert.Equal(t, []int64{9001}, e.torbox.deletedIDs, "the transfer is deleted exactly once")
	assert.Contains(t, e.store.deleted, uncachedHash)
	assert.Empty(t, e.torbox.myListCalls, "no poll needed")
}

// TorBox refusing that delete leaves the row detached (hidden, still counted as held)
// and the wait poller retries on the next tick until it succeeds.
func TestDelete_Downloading_RetriesOnNextTick(t *testing.T) {
	e, srv := addWaitMiss(t)
	e.torbox.deleteErr = errors.New("torbox: HTTP 502")

	body, ct := formBody("hashes", uncachedHash)
	require.Equal(t, http.StatusOK, e.do("POST", "/api/v2/torrents/delete", body, ct).Code)
	rel := e.store.releases[uncachedHash]
	require.NotNil(t, rel, "row kept while the transfer is still on the account")
	assert.NotZero(t, rel.DetachedAt)
	rec := e.do("GET", "/api/v2/torrents/info", nil, "")
	assert.JSONEq(t, "[]", rec.Body.String(), "hidden from the arr meanwhile")

	qbit.PollWaitDownloadsForTest(srv) // still failing
	assert.NotNil(t, e.store.releases[uncachedHash])

	e.torbox.deleteErr = nil
	qbit.PollWaitDownloadsForTest(srv)
	assert.Nil(t, e.store.releases[uncachedHash])
	assert.Contains(t, e.torbox.deletedIDs, int64(9001))
	assert.Empty(t, e.torbox.myListCalls, "a pending cancel never polls progress")
}

// The daily sweep removes detached rows whose links are gone, keeps the rest, and
// never touches attached rows.
func TestSweepDetached(t *testing.T) {
	e := newDetachEnv(t, true)
	e.add(cachedHash, "radarr_hq")
	e.add(torrentInfoH, "radarr_hq")
	e.links.links[cachedHash] = []libscan.Link{libLink(cachedHash)}
	e.links.links[torrentInfoH] = []libscan.Link{libLink(torrentInfoH)}
	e.del(cachedHash)
	e.del(torrentInfoH)

	// Nothing changed yet: both still linked.
	assert.Zero(t, e.srv.SweepDetached(context.Background()))

	delete(e.links.links, torrentInfoH)
	e.links.err = nil
	assert.Equal(t, 1, e.srv.SweepDetached(context.Background()))
	assert.Contains(t, e.store.deleted, torrentInfoH)
	assert.NotContains(t, e.store.deleted, cachedHash)

	// Link check errors keep the row.
	delete(e.links.links, cachedHash)
	e.links.err = errors.New("boom")
	assert.Zero(t, e.srv.SweepDetached(context.Background()))
	assert.NotContains(t, e.store.deleted, cachedHash)
}

// End to end through a real FUSE mount and a real library walk: after the arr deletes
// a release whose episode it already imported, the library symlink still reads.
func TestDelete_Detached_LibraryLinkStillReadsThroughFUSE(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("FUSE not available")
	}
	root := t.TempDir()
	dl := filepath.Join(root, "lazarr")
	fuse := filepath.Join(root, "lazarr_mount")
	lib := filepath.Join(root, "Movies")
	for _, d := range []string{dl, fuse, lib} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	store, err := catalog.OpenSQLite(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	fsys := vfs.New(fuse, store, &constMat{})
	if err := fsys.Mount(); err != nil {
		t.Skipf("FUSE mount failed: %v", err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	cfg := config.Default()
	cfg.Paths.DownloadDir = dl
	cfg.Paths.FuseMount = fuse
	cfg.Categories = []string{"radarr_hq"}
	scanner := &libscan.Scanner{
		Roots: []string{root}, DownloadDir: dl, Store: store,
		Resolver: playback.NewResolver(fuse, filepath.Join(root, "lazarr_cache"), nil),
	}
	srv := qbit.New(qbit.Deps{Config: cfg, Store: store, TorBox: &fakeTorBox{}, Symlink: &fakeSymlink{}, Links: scanner})
	env := &testEnv{handler: srv}

	body, ct := formBody("urls", magnetURI(cachedHash, "Big Buck Bunny"), "category", "radarr_hq")
	require.Equal(t, http.StatusOK, env.do("POST", "/api/v2/torrents/add", body, ct).Code)
	// The "import": the arr moved a symlink into its library.
	mp4 := "Big Buck Bunny (2008) [1080p]/Big.Buck.Bunny.2008.1080p.mp4"
	link := filepath.Join(lib, "Big Buck Bunny (2008)", "Big Buck Bunny (2008).mp4")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(fuse, cachedHash, mp4), link))

	body, ct = formBody("hashes", cachedHash)
	require.Equal(t, http.StatusOK, env.do("POST", "/api/v2/torrents/delete", body, ct).Code)

	f, err := os.Open(link)
	require.NoError(t, err, "library link must still resolve after the arr's delete")
	buf := make([]byte, 16)
	n, err := f.ReadAt(buf, 0)
	_ = f.Close()
	require.NoError(t, err)
	assert.Equal(t, 16, n)

	// Once the library link is gone, the sweep removes the row and FUSE stops serving it.
	require.NoError(t, os.Remove(link))
	assert.Equal(t, 1, srv.SweepDetached(context.Background()))
	_, _, err = store.GetRelease(cachedHash)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
}

// constMat is a vfs.Materializer that serves 0x5A bytes.
type constMat struct{}

func (constMat) ReadAt(_ string, _ int, p []byte, _ int64) (int, error) {
	for i := range p {
		p[i] = 0x5A
	}
	return len(p), nil
}
func (constMat) Release(string) error { return nil }

// The add-time content gate end to end: the live .exe fake is refused with "Fails."
// (the arr falls through to its next release) and nothing is stored or symlinked.
func TestAdd_ContentGateRefusesLiveExeFake(t *testing.T) {
	e := newTestEnv(false)
	const fake = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e.torbox.items = map[string]torbox.CachedItem{fake: {
		Hash: fake, Name: "Its.Always.Rainy.in.Portland.S18E10.The.Guys.Have.a.Sleepover.1080p.WEB.H264-FLUX",
		Size: 886772008,
		Files: []torbox.CachedFile{{ID: 0,
			Name: "Its.Always.Rainy.in.Portland.S18E10.The.Guys.Have.a.Sleepover.1080p.WEB.H264-FLUX.exe", Size: 886772008}},
	}}
	body, ct := formBody("urls", magnetURI(fake, "Its.Always.Rainy.in.Portland.S18E10.1080p.WEB.H264-FLUX"), "category", "sonarr_main")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "Fails.", rec.Body.String())
	_, _, err := e.store.GetRelease(fake)
	assert.Error(t, err, "a refused add stores nothing")
	assert.Empty(t, e.symlink.created)

	// The real cached release still goes through.
	body, ct = formBody("urls", magnetURI(cachedHash, "Big+Buck+Bunny"), "category", "radarr_hq")
	assert.Equal(t, "Ok.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())
}

// A wait download that finishes as an .exe is failed, not imported.
func TestWaitFinish_ContentGate(t *testing.T) {
	e, srv := addWaitMiss(t)
	e.torbox.myListByIDFn = func(id int64) *torbox.TorrentDetail {
		return &torbox.TorrentDetail{ID: id, DownloadState: "completed", DownloadFinished: true, Progress: 1}
	}
	e.torbox.items = map[string]torbox.CachedItem{uncachedHash: {Hash: uncachedHash, Name: "Fake",
		Size: 929063766, Files: []torbox.CachedFile{{Name: "Show.S01E01.1080p.WEB.H264-CAKES.exe", Size: 929063766}}}}
	qbit.PollWaitDownloadsForTest(srv)
	rel := e.store.releases[uncachedHash]
	require.NotNil(t, rel)
	assert.Equal(t, catalog.StateError, rel.State)
	assert.Empty(t, e.symlink.created)
}
