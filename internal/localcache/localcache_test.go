package localcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback"
)

const (
	hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hashU = "cccccccccccccccccccccccccccccccccccccccc" // usenet-sourced
)

// fakeInner is the materialize engine stand-in: an in-memory byte store per file.
type fakeInner struct {
	mu        sync.Mutex
	data      map[fileKey][]byte
	reads     int32
	throttled atomic.Bool
	released  []string
	failHash  string
	block     chan struct{} // when set, ReadAtCtx blocks until ctx is done (slot starvation)
}

func newFakeInner() *fakeInner { return &fakeInner{data: map[fileKey][]byte{}} }

func (f *fakeInner) put(hash string, fileID int, b []byte) { f.data[fileKey{hash, fileID}] = b }

func (f *fakeInner) ReadAt(hash string, fileID int, p []byte, off int64) (int, error) {
	return f.ReadAtCtx(context.Background(), hash, fileID, p, off)
}

func (f *fakeInner) ReadAtCtx(ctx context.Context, hash string, fileID int, p []byte, off int64) (int, error) {
	atomic.AddInt32(&f.reads, 1)
	if f.block != nil {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if hash == f.failHash {
		return 0, errors.New("upstream boom")
	}
	f.mu.Lock()
	b, ok := f.data[fileKey{hash, fileID}]
	f.mu.Unlock()
	if !ok {
		return 0, errors.New("unknown file")
	}
	if off >= int64(len(b)) {
		return 0, nil
	}
	n := copy(p, b[off:])
	return n, nil
}

func (f *fakeInner) Release(hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, hash)
	return nil
}

func (f *fakeInner) Throttled() bool { return f.throttled.Load() }
func (f *fakeInner) readCount() int  { return int(atomic.LoadInt32(&f.reads)) }

// fakeSource is a scripted media server.
type fakeSource struct {
	name      string
	sessions  []playback.Session
	next      map[string][]playback.Item // by series key
	refreshed []string
	mu        sync.Mutex
}

func (s *fakeSource) Name() string { return s.name }
func (s *fakeSource) Sessions(context.Context) ([]playback.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]playback.Session(nil), s.sessions...), nil
}
func (s *fakeSource) NextEpisodes(_ context.Context, sess playback.Session, n int) ([]playback.Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.next[sess.SeriesKey]
	if len(items) > n {
		items = items[:n]
	}
	return items, nil
}
func (s *fakeSource) Refresh(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshed = append(s.refreshed, id)
	return nil
}

// harness builds an enabled engine on a temp tree: <root>/lazarr_mount (fuse twin),
// <root>/lazarr_cache (cache dir == publish_at, publish off), <root>/lazarr (download
// dir), <root>/TV Shows/<cat>/... (library). Time is frozen and controllable.
type harness struct {
	t     *testing.T
	root  string
	store catalog.Store
	inner *fakeInner
	src   *fakeSource
	eng   *Engine
	cfg   config.LocalCache
	pb    config.Playback
	now   time.Time
	nowMu sync.Mutex
	// source, when set by mutate, replaces src as the engine's only media server.
	source playback.Source
}

func newHarness(t *testing.T, mutate func(h *harness)) *harness {
	t.Helper()
	root := t.TempDir()
	st, err := catalog.OpenSQLite(filepath.Join(root, "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, root: root, store: st, inner: newFakeInner(), src: &fakeSource{name: "jellyfin", next: map[string][]playback.Item{}}}
	h.now = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	h.cfg = config.Default().LocalCache
	h.cfg.Enabled = true
	h.cfg.Dir = filepath.Join(root, "lazarr_cache")
	h.cfg.PublishAt = h.cfg.Dir
	h.cfg.Publish = false
	h.cfg.MinFileMiB = 0
	h.cfg.PullWindowMiB = 1
	h.cfg.PullOnGrab = []string{"radarr_main"}
	h.cfg.LibraryRoots = []string{filepath.Join(root, "TV Shows"), filepath.Join(root, "Movies")}
	h.pb = config.Default().Playback
	if mutate != nil {
		mutate(h)
	}
	srcs := []playback.Source{h.src}
	if h.source != nil {
		srcs = []playback.Source{h.source}
	}
	eng, err := New(Deps{
		Store: st, Inner: h.inner, Cache: h.cfg, Playback: h.pb,
		Paths:      config.Paths{DownloadDir: filepath.Join(root, "lazarr"), FuseMount: filepath.Join(root, "lazarr_mount")},
		Categories: []string{"radarr_main", "sonarr_main"},
		Sources:    srcs,
		PublishFn:  func(string, string) error { return nil },
	})
	require.NoError(t, err)
	eng.now = func() time.Time { h.nowMu.Lock(); defer h.nowMu.Unlock(); return h.now }
	eng.links.now = eng.now
	h.eng = eng
	t.Cleanup(func() { _ = eng.Close() })
	return h
}

func (h *harness) advance(d time.Duration) {
	h.nowMu.Lock()
	h.now = h.now.Add(d)
	h.nowMu.Unlock()
}

// addRelease stores a release with one file of `size` random bytes in the fake
// inner engine and a library symlink pointing at the FUSE twin. Returns the link path.
func (h *harness) addRelease(hash, category, name, rel string, size int, source catalog.Source) string {
	h.t.Helper()
	b := make([]byte, size)
	_, _ = rand.Read(b)
	h.inner.put(hash, 1, b)
	r := &catalog.Release{Hash: hash, Name: name, Category: category, State: catalog.StateVirtual, Cached: true, Source: source, AddedOn: h.now.Unix() - 3600, TotalSize: int64(size)}
	require.NoError(h.t, h.store.UpsertRelease(r, []catalog.File{{Hash: hash, FileID: 1, RelPath: rel, Size: int64(size)}}))
	libDir := filepath.Join(h.root, "TV Shows", category, name)
	if category == "radarr_main" {
		libDir = filepath.Join(h.root, "Movies", category, name)
	}
	require.NoError(h.t, os.MkdirAll(libDir, 0o755))
	link := filepath.Join(libDir, filepath.Base(rel))
	require.NoError(h.t, os.Symlink(filepath.Join(h.root, "lazarr_mount", hash, rel), link))
	return link
}

func (h *harness) bytesOf(hash string) []byte { return h.inner.data[fileKey{hash, 1}] }

func (h *harness) waitComplete(hash string) *catalog.LocalFile {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lf, err := h.store.GetLocalFile(hash, 1)
		if err == nil && lf.State == catalog.LocalComplete {
			return lf
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("file %s never completed", hash[:8])
	return nil
}

// waitLink polls until the symlink at p points at want (retargeting runs just after
// the catalog row flips to complete, so a completed pull may briefly still be linked
// to FUSE).
func waitLink(t *testing.T, p, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tgt, err := os.Readlink(p); err == nil && tgt == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	tgt, _ := os.Readlink(p)
	t.Fatalf("link %s points at %s, want %s", p, tgt, want)
}

func readlink(t *testing.T, p string) string {
	t.Helper()
	tgt, err := os.Readlink(p)
	require.NoError(t, err)
	return tgt
}

// ---- tests ----------------------------------------------------------------------

func TestPullEndToEndRetargetsLink(t *testing.T) {
	h := newHarness(t, nil)
	link := h.addRelease(hashA, "sonarr_main", "Show", "Show.S01E01/ep1.mkv", 3<<20+123, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks() // index the library link

	require.True(t, h.eng.Enqueue(hashA, 1, ReasonManual))
	assert.False(t, h.eng.Enqueue(hashA, 1, ReasonManual), "duplicate enqueue is a no-op")
	lf := h.waitComplete(hashA)
	assert.Equal(t, int64(3<<20+123), lf.Frontier)
	assert.Equal(t, ReasonManual, lf.Reason)
	assert.True(t, lf.PulledAt > 0)

	// Bytes on disk are identical; mtime is the epoch (stable_mtime off).
	onDisk, err := os.ReadFile(filepath.Join(h.cfg.Dir, hashA, "Show.S01E01/ep1.mkv"))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(onDisk, h.bytesOf(hashA)))
	fi, _ := os.Stat(filepath.Join(h.cfg.Dir, hashA, "Show.S01E01/ep1.mkv"))
	assert.Equal(t, int64(0), fi.ModTime().Unix())

	// The library link now points at the cache twin.
	waitLink(t, link, filepath.Join(h.cfg.Dir, hashA, "Show.S01E01/ep1.mkv"))
	deadline := time.Now().Add(5 * time.Second)
	for lf, _ = h.store.GetLocalFile(hashA, 1); !lf.Linked && time.Now().Before(deadline); lf, _ = h.store.GetLocalFile(hashA, 1) {
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(t, lf.Linked)
	links, _ := h.store.ListLibraryLinks(hashA, 1)
	require.Len(t, links, 1)
	assert.Equal(t, playback.TargetCache, links[0].TargetKind)

	// Reads via the decorator are served locally, without touching the inner engine.
	before := h.inner.readCount()
	buf := make([]byte, 4096)
	n, err := h.eng.ReadAt(hashA, 1, buf, 1<<20)
	require.NoError(t, err)
	assert.Equal(t, 4096, n)
	assert.Equal(t, h.bytesOf(hashA)[1<<20:1<<20+4096], buf)
	assert.Equal(t, before, h.inner.readCount())
	// EOF semantics: short read at the tail.
	n, err = h.eng.ReadAt(hashA, 1, buf, int64(3<<20+100))
	require.NoError(t, err)
	assert.Equal(t, 23, n)

	snap := h.eng.Snapshot()
	assert.Equal(t, 1, snap.Files["complete"])
	assert.Equal(t, int64(3<<20+123), snap.LocalBytes)
	files := h.eng.LocalFiles()
	require.Len(t, files, 1)
	assert.Equal(t, "Show", files[0].Name)
}

func TestStableMtimeStampsGrabTime(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.StableMtime = true })
	h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1024, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonManual))
	h.waitComplete(hashA)
	fi, err := os.Stat(filepath.Join(h.cfg.Dir, hashA, "ep1.mkv"))
	require.NoError(t, err)
	assert.Equal(t, h.now.Unix()-3600, fi.ModTime().Unix())
}

func TestDecoratorServesPulledPrefixThenFallsThrough(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	// Simulate a pull in progress with 64 KiB durable.
	data := h.bytesOf(hashA)
	f, err := h.eng.disk.openPart(hashA, "ep1.mkv", 0)
	require.NoError(t, err)
	_, _ = f.Write(data[:65536])
	require.NoError(t, f.Close())
	h.eng.mu.Lock()
	h.eng.files[fileKey{hashA, 1}] = &fileState{LocalFile: catalog.LocalFile{Hash: hashA, FileID: 1, State: catalog.LocalPulling, Size: 1 << 20, Frontier: 65536}, rel: "ep1.mkv"}
	h.eng.mu.Unlock()

	buf := make([]byte, 4096)
	before := h.inner.readCount()
	n, err := h.eng.ReadAt(hashA, 1, buf, 60000) // inside the frontier
	require.NoError(t, err)
	assert.Equal(t, 4096, n)
	assert.Equal(t, data[60000:64096], buf)
	assert.Equal(t, before, h.inner.readCount(), "prefix served from the part file")

	n, err = h.eng.ReadAt(hashA, 1, buf, 70000) // past the frontier → inner engine
	require.NoError(t, err)
	assert.Equal(t, 4096, n)
	assert.Equal(t, data[70000:74096], buf)
	assert.Equal(t, before+1, h.inner.readCount())

	// Unknown file → inner engine, byte for byte.
	_, err = h.eng.ReadAt(hashB, 1, buf, 0)
	require.Error(t, err)
	assert.Equal(t, before+2, h.inner.readCount())

	// Release passes through.
	require.NoError(t, h.eng.Release(hashA))
	assert.Equal(t, []string{hashA}, h.inner.released)
}

func TestUsenetIsNeverCached(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.PullOnGrab = []string{"radarr_main", "sonarr_main"} })
	link := h.addRelease(hashU, "sonarr_main", "NZBShow", "ep1.mkv", 1<<20, catalog.SourceUsenet)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()

	// 1. Explicit pull request is refused before any I/O.
	before := h.inner.readCount()
	assert.False(t, h.eng.Enqueue(hashU, 1, ReasonManual))
	// 2. Grab hook in a pull_on_grab category is a no-op.
	rel, files, _ := h.store.GetRelease(hashU)
	h.eng.OnGrab(rel, files)
	// 3. A live session on it records the watch but plans nothing.
	h.src.sessions = []playback.Session{{Server: "jellyfin", UserID: "u1", Kind: playback.KindEpisode, SeriesKey: "s", Season: 1, Episode: 1, Path: link, Position: 10 * time.Minute, Duration: 40 * time.Minute}}
	h.eng.PollOnce(ctx)
	time.Sleep(100 * time.Millisecond)
	snap := h.eng.Snapshot()
	assert.Empty(t, snap.Queue)
	assert.Empty(t, snap.Pulling)
	assert.Equal(t, 0, snap.Files["complete"]+snap.Files["pulling"])
	assert.Equal(t, before, h.inner.readCount(), "zero engine reads")
	lfs, _ := h.store.ListLocalFiles("")
	assert.Empty(t, lfs)
	ws, _ := h.store.ListWatchByFile(hashU, 1)
	assert.Len(t, ws, 1, "observed for the record")
	// 4. The link is untouched.
	assert.Equal(t, filepath.Join(h.root, "lazarr_mount", hashU, "ep1.mkv"), readlink(t, link))
	// 5. ReadAt passes straight through.
	buf := make([]byte, 16)
	_, err := h.eng.ReadAt(hashU, 1, buf, 0)
	require.NoError(t, err)
	assert.Equal(t, before+1, h.inner.readCount())
}

func TestGrabHookPullsOnlyConfiguredCategories(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "radarr_main", "Movie (2026)", "Movie.2026.mkv", 2<<20, catalog.SourceTorrent)
	h.addRelease(hashB, "sonarr_main", "Show", "ep1.mkv", 2<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	for _, hash := range []string{hashA, hashB} {
		rel, files, _ := h.store.GetRelease(hash)
		h.eng.OnGrab(rel, files)
	}
	lf := h.waitComplete(hashA)
	assert.Equal(t, ReasonGrab, lf.Reason)
	time.Sleep(50 * time.Millisecond)
	_, err := h.store.GetLocalFile(hashB, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound, "sonarr category is not in pull_on_grab")
}

func TestPlannerPrefetchesNextEpisodesFirst(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.MaxConcurrentPulls = 1 })
	cur := h.addRelease(hashA, "sonarr_main", "Show S01", "e01.mkv", 1<<20, catalog.SourceTorrent)
	next := h.addRelease(hashB, "sonarr_main", "Show S02", "e02.mkv", 1<<20, catalog.SourceTorrent)
	h.src.next["series-1"] = []playback.Item{{ItemID: "n1", Season: 1, Episode: 2, Path: next}, {ItemID: "n2", Season: 1, Episode: 3, Path: "/nowhere/e03.mkv"}}
	h.src.sessions = []playback.Session{{Server: "jellyfin", UserID: "u1", UserName: "admin", ItemID: "cur", Kind: playback.KindEpisode, SeriesKey: "series-1", Season: 1, Episode: 1, Path: cur, Position: 2 * time.Minute, Duration: 40 * time.Minute, State: playback.StatePlaying}}

	// Plan without workers running so the queue order is observable.
	h.eng.PollOnce(context.Background())
	snap := h.eng.Snapshot()
	require.Len(t, snap.Queue, 2)
	assert.Equal(t, hashB, snap.Queue[0].Hash, "next episode is queued before the current one")
	assert.Equal(t, ReasonPrefetch, snap.Queue[0].Reason)
	assert.Equal(t, hashA, snap.Queue[1].Hash)
	assert.Equal(t, ReasonWatch, snap.Queue[1].Reason)
	assert.True(t, h.eng.isWanted(fileKey{hashA, 1}, h.now))
	assert.True(t, h.eng.isWanted(fileKey{hashB, 1}, h.now))
	assert.Len(t, snap.Sessions, 1)
	assert.True(t, snap.Sessions[0].Lazarr)

	ws, _ := h.store.ListWatchByFile(hashA, 1)
	require.Len(t, ws, 1)
	assert.Equal(t, "u1", ws[0].UserID)
	assert.InDelta(t, 5.0, ws[0].ProgressPct, 0.01)
	assert.False(t, ws[0].Completed)
	assert.Equal(t, "series-1", ws[0].SeriesKey)

	// Links learned from the session are in the index without a walk.
	links, _ := h.store.ListLibraryLinks(hashB, 1)
	assert.Len(t, links, 1)

	// Now run the workers: both complete; the current one, being watched, is
	// linked to the cache too.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.waitComplete(hashB)
	h.waitComplete(hashA)
	waitLink(t, next, filepath.Join(h.cfg.Dir, hashB, "e02.mkv"))

	// Completion at >= watched_pct flips the watch row.
	h.src.sessions[0].Position = 37 * time.Minute
	h.eng.PollOnce(ctx)
	ws, _ = h.store.ListWatchByFile(hashA, 1)
	assert.True(t, ws[0].Completed)

	// A stale poll never keeps things wanted.
	h.advance(time.Hour)
	assert.False(t, h.eng.isWanted(fileKey{hashA, 1}, h.now))
}

func TestReaperEvictsFinishedAndRetargetsBack(t *testing.T) {
	h := newHarness(t, nil)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	waitLink(t, link, filepath.Join(h.cfg.Dir, hashA, "ep1.mkv"))

	// Everyone who watched it finished it, 49h ago → past the 48h finished TTL.
	require.NoError(t, h.store.UpsertWatch(&catalog.Watch{Server: "jellyfin", UserID: "u1", Hash: hashA, FileID: 1, SeriesKey: "s", Completed: true, FirstSeen: h.now.Unix(), LastSeen: h.now.Unix()}))
	h.advance(49 * time.Hour)
	h.eng.Sweep(ctx)

	_, err := h.store.GetLocalFile(hashA, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
	_, err = os.Stat(filepath.Join(h.cfg.Dir, hashA))
	assert.True(t, errors.Is(err, os.ErrNotExist), "hash dir pruned")
	assert.Equal(t, filepath.Join(h.root, "lazarr_mount", hashA, "ep1.mkv"), readlink(t, link), "link back on FUSE")
	links, _ := h.store.ListLibraryLinks(hashA, 1)
	require.Len(t, links, 1)
	assert.Equal(t, playback.TargetFuse, links[0].TargetKind)

	// Playback still works: reads go to the inner engine.
	buf := make([]byte, 8)
	_, err = h.eng.ReadAt(hashA, 1, buf, 0)
	require.NoError(t, err)
}

func TestReaperClassPersistedAndWantedNeverEvicted(t *testing.T) {
	h := newHarness(t, nil)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonPrefetch))
	h.waitComplete(hashA)

	h.eng.Sweep(ctx)
	lf, _ := h.store.GetLocalFile(hashA, 1)
	assert.Equal(t, "untouched", lf.Class)

	// A live session on it makes it wanted; even ancient, it stays.
	h.src.sessions = []playback.Session{{Server: "jellyfin", UserID: "u1", Kind: playback.KindEpisode, SeriesKey: "s", Season: 1, Episode: 1, Path: link, Position: time.Minute, Duration: time.Hour}}
	h.eng.PollOnce(ctx)
	h.eng.mu.Lock()
	h.eng.files[fileKey{hashA, 1}].PulledAt = h.now.Add(-400 * 24 * time.Hour).Unix()
	row := h.eng.files[fileKey{hashA, 1}].LocalFile
	h.eng.mu.Unlock()
	_ = h.store.UpsertLocalFile(&row)
	h.eng.Sweep(ctx)
	lf, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err)
	assert.Equal(t, "wanted", lf.Class)
}

func TestUnreferencedEvictedAfterGrace(t *testing.T) {
	h := newHarness(t, nil)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)

	// Sonarr upgrades: the library link is deleted.
	require.NoError(t, os.Remove(link))
	h.eng.walkLinks() // index notices
	h.eng.Sweep(ctx)
	_, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err, "inside the grace window it stays")
	h.advance(2 * time.Hour)
	h.eng.Sweep(ctx)
	_, err = h.store.GetLocalFile(hashA, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
}

func TestForgetDropsEverything(t *testing.T) {
	h := newHarness(t, nil)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	require.NoError(t, h.eng.Forget(hashA))
	_, err := os.Stat(filepath.Join(h.cfg.Dir, hashA))
	assert.True(t, errors.Is(err, os.ErrNotExist))
	assert.Equal(t, filepath.Join(h.root, "lazarr_mount", hashA, "ep1.mkv"), readlink(t, link))
	lfs, _ := h.store.ListLocalFiles("")
	assert.Empty(t, lfs)
}

func TestPullFailureFallsBackAndRetries(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	h.inner.failHash = hashA
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	deadline := time.Now().Add(5 * time.Second)
	var lf *catalog.LocalFile
	for time.Now().Before(deadline) {
		lf, _ = h.store.GetLocalFile(hashA, 1)
		if lf != nil && lf.State == catalog.LocalFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotNil(t, lf)
	assert.Equal(t, catalog.LocalFailed, lf.State)
	assert.Contains(t, lf.Error, "boom")
	// Reads fall through (and fail the same way the inner engine does).
	_, err := h.eng.ReadAt(hashA, 1, make([]byte, 8), 0)
	require.Error(t, err)
	// Once upstream recovers, a re-enqueue resumes and completes.
	h.inner.failHash = ""
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
}

func TestGuardsSkipBeforeIO(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.MinFileMiB = 1 })
	h.addRelease(hashA, "sonarr_main", "Tiny", "sample.mkv", 100, catalog.SourceTorrent)
	assert.False(t, h.eng.Enqueue(hashA, 1, ReasonManual), "below min_file_mib")

	h.addRelease(hashB, "sonarr_main", "Big", "ep.mkv", 2<<20, catalog.SourceTorrent)
	rel, files, _ := h.store.GetRelease(hashB)
	rel.Cached = false
	require.NoError(t, h.store.UpsertRelease(rel, files))
	assert.False(t, h.eng.Enqueue(hashB, 1, ReasonManual), "uncached")
	rel.Cached = true
	rel.CacheStatus = catalog.CacheStatusEvicted
	require.NoError(t, h.store.UpsertRelease(rel, files))
	assert.False(t, h.eng.Enqueue(hashB, 1, ReasonManual), "evicted on TorBox")
	rel.CacheStatus = catalog.CacheStatusCached
	require.NoError(t, h.store.UpsertRelease(rel, files))
	assert.True(t, h.eng.Enqueue(hashB, 1, ReasonManual))
	assert.Equal(t, 0, h.inner.readCount())
}

func TestHourlyBudgetDefers(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.MaxPullsPerHour = 1 })
	h.addRelease(hashA, "sonarr_main", "A", "a.mkv", 1<<20, catalog.SourceTorrent)
	h.addRelease(hashB, "sonarr_main", "B", "b.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonManual))
	h.waitComplete(hashA)
	require.True(t, h.eng.Enqueue(hashB, 1, ReasonManual))
	time.Sleep(300 * time.Millisecond)
	_, err := h.store.GetLocalFile(hashB, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound, "second pull deferred by the hourly budget")
}

func TestSlotStarvationRequeuesInsteadOfFailing(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "sonarr_main", "A", "a.mkv", 1<<20, catalog.SourceTorrent)
	h.inner.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonManual))
	time.Sleep(100 * time.Millisecond)
	snap := h.eng.Snapshot()
	assert.Len(t, snap.Pulling, 1)
	// Close cancels the in-flight pull; the row stays "pulling" (resumable), not failed.
	require.NoError(t, h.eng.Close())
	lf, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err)
	assert.Equal(t, catalog.LocalPulling, lf.State)
}

func TestResumeOnStart(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "sonarr_main", "A", "a.mkv", 2<<20, catalog.SourceTorrent)
	data := h.bytesOf(hashA)
	// A prior process left 1 MiB durable.
	f, err := h.eng.disk.openPart(hashA, "a.mkv", 0)
	require.NoError(t, err)
	_, _ = f.Write(data[:1<<20])
	require.NoError(t, f.Close())
	require.NoError(t, h.store.UpsertLocalFile(&catalog.LocalFile{Hash: hashA, FileID: 1, State: catalog.LocalPulling, Size: 2 << 20, Frontier: 1 << 20, Reason: ReasonWatch}))
	// Fresh engine over the same store/dir.
	eng, err := New(Deps{Store: h.store, Inner: h.inner, Cache: h.cfg, Playback: h.pb,
		Paths:      config.Paths{DownloadDir: filepath.Join(h.root, "lazarr"), FuseMount: filepath.Join(h.root, "lazarr_mount")},
		Categories: []string{"sonarr_main"}, PublishFn: func(string, string) error { return nil }})
	require.NoError(t, err)
	defer func() { _ = eng.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	h.eng = eng
	h.waitComplete(hashA)
	onDisk, _ := os.ReadFile(filepath.Join(h.cfg.Dir, hashA, "a.mkv"))
	assert.True(t, bytes.Equal(onDisk, data))
	assert.LessOrEqual(t, h.inner.readCount(), 2, "only the missing half was fetched")
}

func TestOrphanSweepAndCompleteRowWithoutFile(t *testing.T) {
	h := newHarness(t, nil)
	h.addRelease(hashA, "sonarr_main", "A", "a.mkv", 1024, catalog.SourceTorrent)
	// Orphan dir on disk, no rows.
	require.NoError(t, os.MkdirAll(filepath.Join(h.cfg.Dir, hashB, "x"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.cfg.Dir, hashB, "x", "junk.mkv"), []byte("j"), 0o644))
	// Complete row without a file → dropped at load.
	require.NoError(t, h.store.UpsertLocalFile(&catalog.LocalFile{Hash: hashA, FileID: 1, State: catalog.LocalComplete, Size: 1024}))
	eng, err := New(Deps{Store: h.store, Inner: h.inner, Cache: h.cfg, Playback: h.pb,
		Paths:      config.Paths{DownloadDir: filepath.Join(h.root, "lazarr"), FuseMount: filepath.Join(h.root, "lazarr_mount")},
		Categories: []string{"sonarr_main"}, PublishFn: func(string, string) error { return nil }})
	require.NoError(t, err)
	defer func() { _ = eng.Close() }()
	_, err = h.store.GetLocalFile(hashA, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
	eng.Sweep(context.Background())
	_, err = os.Stat(filepath.Join(h.cfg.Dir, hashB))
	assert.True(t, errors.Is(err, os.ErrNotExist), "orphan removed")
}

func TestNewnessInputs(t *testing.T) {
	h := newHarness(t, nil)
	rel := &catalog.Release{Hash: hashA, Category: "radarr_main", Name: "Some.Movie.2026.1080p.WEB-DL"}
	date, isMovie, win := h.eng.newness(rel, h.now)
	assert.True(t, isMovie)
	assert.Equal(t, 2026, date.Year())
	assert.Equal(t, h.cfg.NewReleaseWindow.D(), win)

	old := &catalog.Release{Hash: hashB, Category: "radarr_main", Name: "Come and See (1985) Bluray-1080p"}
	date, _, _ = h.eng.newness(old, h.now)
	assert.Equal(t, 1985, date.Year())

	tv := &catalog.Release{Hash: hashU, Category: "sonarr_main", Name: "Show.2001.S21E01"}
	date, isMovie, win = h.eng.newness(tv, h.now)
	assert.False(t, isMovie)
	assert.True(t, date.IsZero(), "no name-year heuristic for TV")
	assert.Equal(t, time.Duration(0), win)

	// A session-observed premiere date takes over.
	h.eng.mu.Lock()
	h.eng.obsDate[hashU] = h.now.Add(-24 * time.Hour)
	h.eng.mu.Unlock()
	date, _, win = h.eng.newness(tv, h.now)
	assert.False(t, date.IsZero())
	assert.Equal(t, h.cfg.ActiveWindow.D()*episodeNewnessMultiplier, win)

	assert.Equal(t, 0, yearFromName("no.year.here", 2026))
	assert.Equal(t, 2024, yearFromName("Movie.2077.Remake.2024", 2026), "future years ignored")
}
