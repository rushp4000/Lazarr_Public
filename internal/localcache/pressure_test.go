//go:build pressure && linux

// Real-filesystem disk-pressure test for the local cache (docs/28 §13 criterion 4).
//
// Every scenario runs the real engine (puller goroutine, planner PollOnce, reaper
// Sweep, link index, sqlite catalog) against a private, small filesystem: by default
// a fresh 512 MiB tmpfs mounted by the test itself (needs root), or a scratch
// filesystem the caller already mounted (LAZARR_PRESSURE_DIR). Only the cache dir and
// the ballast live on it; the catalog, the library and the FUSE twin stay in a temp
// dir. The test refuses to run on anything larger than 4 GiB or under a production
// path, so it can never touch a real volume.
//
//	sudo go test -tags pressure -run TestPressure -v ./internal/localcache/
//
// See README.md "Disk-pressure test".
package localcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback"
)

const (
	pressureFSMiB  = 512
	pressureMaxFS  = 4 << 30 // never run on anything bigger: a real volume is far larger
	outcomePulled  = "pulled"
	outcomeRefused = "refused"
	outcomeFailed  = "failed"
)

// forbiddenRoots are typical production paths. The test refuses to
// run under any of them even if LAZARR_PRESSURE_DIR points there.
var forbiddenRoots = []string{"/cache", "/config", "/debrid_symlinks", "/data", "/mnt"}

// pressureFS returns a directory on a small private filesystem, unmounted and removed
// when the test ends.
func pressureFS(t *testing.T) string {
	t.Helper()
	var dir string
	if base := os.Getenv("LAZARR_PRESSURE_DIR"); base != "" {
		d, err := os.MkdirTemp(base, "run-")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		dir = d
	} else {
		if os.Geteuid() != 0 {
			t.Skip("needs root to mount a private tmpfs (or set LAZARR_PRESSURE_DIR to a mounted scratch fs)")
		}
		d, err := os.MkdirTemp("", "lazarr-pressure-")
		require.NoError(t, err)
		opts := fmt.Sprintf("size=%dm,mode=0755", pressureFSMiB)
		require.NoError(t, syscall.Mount("tmpfs", d, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, opts))
		t.Cleanup(func() {
			if err := syscall.Unmount(d, 0); err != nil {
				_ = syscall.Unmount(d, syscall.MNT_DETACH)
			}
			_ = os.Remove(d)
		})
		dir = d
	}
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	for _, f := range forbiddenRoots {
		if resolved == f || strings.HasPrefix(resolved, f+"/") {
			t.Fatalf("refusing to run the pressure test under production path %s", f)
		}
	}
	var st syscall.Statfs_t
	require.NoError(t, syscall.Statfs(dir, &st))
	if total := st.Blocks * uint64(st.Bsize); total > pressureMaxFS { //nolint:gosec // positive
		t.Fatalf("refusing: %s is on a %d-byte filesystem; the pressure test only runs on a small scratch fs", dir, total)
	}
	return dir
}

// ---- synthetic inner engine ------------------------------------------------------

// synthInner serves deterministic bytes (byte(off % 251)) for any registered file
// without holding them in memory.
type synthInner struct {
	mu    sync.Mutex
	sizes map[fileKey]int64
	reads atomic.Int64
	block []byte
}

func newSynthInner() *synthInner {
	b := make([]byte, 1<<20+251)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return &synthInner{sizes: map[fileKey]int64{}, block: b}
}

func (s *synthInner) ReadAt(hash string, fileID int, p []byte, off int64) (int, error) {
	return s.ReadAtCtx(context.Background(), hash, fileID, p, off)
}

func (s *synthInner) ReadAtCtx(ctx context.Context, hash string, fileID int, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.reads.Add(1)
	s.mu.Lock()
	size, ok := s.sizes[fileKey{hash, fileID}]
	s.mu.Unlock()
	if !ok {
		return 0, errors.New("unknown file")
	}
	if off >= size {
		return 0, nil
	}
	n := int(min(int64(len(p)), size-off))
	for done := 0; done < n; {
		done += copy(p[done:n], s.block[int((off+int64(done))%251):])
	}
	return n, nil
}

func (s *synthInner) Release(string) error { return nil }
func (s *synthInner) Throttled() bool      { return false }

// ---- rig -------------------------------------------------------------------------

type pfile struct {
	idx  int
	hash string
	rel  string
	link string
	size int64
	plan Class
}

type eviction struct {
	hash, class, reason string
}

type pressureStats struct {
	pulls, refused, failed int
	sweeps, stuckSweeps    int
	makeRoomPasses         int
	minPostSweepPct        float64
	minAfterPullPct        float64
}

type rig struct {
	t      *testing.T
	fsDir  string
	root   string
	cfg    config.LocalCache
	pb     config.Playback
	store  catalog.Store
	inner  *synthInner
	src    *fakeSource
	eng    *Engine
	logs   *logCapture
	ctx    context.Context
	nowMu  sync.Mutex
	now    time.Time
	files  map[string]*pfile // by short hash
	pulled map[string]bool   // short hashes that completed a pull
	wanted []*pfile
	stats  pressureStats

	minFree atomic.Uint64 // lowest free bytes the sampler saw
	total   uint64
}

func newRig(t *testing.T, mutate func(*config.LocalCache)) *rig {
	t.Helper()
	fsDir := pressureFS(t)
	root := t.TempDir()
	st, err := catalog.OpenSQLite(filepath.Join(root, "db.sqlite"))
	require.NoError(t, err)

	// Typical config: profile balanced, free_high 20,
	// free_low 5, reserve 2, one puller. Test-only: no size floor (files are 40-80 MiB
	// on a 512 MiB fs) and no hourly budget (hundreds of pulls in one simulated hour).
	cfg := config.Default().LocalCache
	require.Equal(t, config.ProfileBalanced, cfg.Profile)
	cfg.Enabled = true
	cfg.Dir = filepath.Join(fsDir, "cache")
	cfg.PublishAt = cfg.Dir
	cfg.Publish = false
	cfg.FreeHighPct, cfg.FreeLowPct, cfg.ReservePct = 20, 5, 2
	cfg.MaxConcurrentPulls = 1
	cfg.MinFileMiB = 1
	cfg.MaxPullsPerHour = 1 << 20
	cfg.PullOnGrab = nil
	cfg.LibraryRoots = []string{filepath.Join(root, "TV Shows"), filepath.Join(root, "Movies")}
	if mutate != nil {
		mutate(&cfg)
	}
	pb := config.Default().Playback

	r := &rig{
		t: t, fsDir: fsDir, root: root, cfg: cfg, pb: pb, store: st, inner: newSynthInner(),
		src:   &fakeSource{name: "jellyfin", next: map[string][]playback.Item{}},
		logs:  &logCapture{},
		now:   time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		files: map[string]*pfile{}, pulled: map[string]bool{},
	}
	if os.Getenv("LAZARR_PRESSURE_VERBOSE") != "" {
		r.logs.echo = func(l logRec) { fmt.Fprintf(os.Stderr, "    log %s %s %v\n", l.level, l.msg, l.attrs) }
	}
	r.stats.minPostSweepPct, r.stats.minAfterPullPct = 100, 100
	eng, err := New(Deps{
		Store: st, Inner: r.inner, Cache: cfg, Playback: pb,
		Paths:      config.Paths{DownloadDir: filepath.Join(root, "lazarr"), FuseMount: filepath.Join(root, "lazarr_mount")},
		Categories: []string{"radarr_main", "sonarr_main"},
		Sources:    []playback.Source{r.src},
		Logger:     slog.New(r.logs),
		PublishFn:  func(string, string) error { return nil },
	})
	require.NoError(t, err)
	eng.now = r.clock
	eng.links.now = r.clock
	r.eng = eng

	// The real puller loop (max_concurrent_pulls = 1); the test plays the reaper so
	// sweeps interleave deterministically with pulls.
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	eng.wg.Add(1)
	go func() { defer eng.wg.Done(); eng.runPuller(ctx) }()

	d, err := eng.disk.disk()
	require.NoError(t, err)
	r.total = d.Total
	r.minFree.Store(d.Free)
	sampDone := make(chan struct{})
	go func() { // statfs sampler: the instantaneous low point, including mid-pull
		defer close(sampDone)
		for ctx.Err() == nil {
			if d, err := eng.disk.disk(); err == nil {
				for {
					cur := r.minFree.Load()
					if d.Free >= cur || r.minFree.CompareAndSwap(cur, d.Free) {
						break
					}
				}
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-sampDone
		_ = eng.Close()
		_ = st.Close()
	})
	eng.walkLinks()
	return r
}

func (r *rig) clock() time.Time { r.nowMu.Lock(); defer r.nowMu.Unlock(); return r.now }
func (r *rig) advance(d time.Duration) {
	r.nowMu.Lock()
	r.now = r.now.Add(d)
	r.nowMu.Unlock()
}

func (r *rig) disk() DiskStat {
	d, err := r.eng.disk.disk()
	require.NoError(r.t, err)
	return d
}

func (r *rig) freePct() float64 { return r.disk().FreePct() }
func (r *rig) minFreePct() float64 {
	return float64(r.minFree.Load()) / float64(r.total) * 100
}

// addFile registers a release with one file, a library symlink pointing at the FUSE
// twin, and indexes the link. plan is the class the file is meant to end up in.
func (r *rig) addFile(i int, plan Class, size int64) *pfile {
	r.t.Helper()
	hash := strings.Repeat(fmt.Sprintf("%02x", i), 20)
	cat, name := "sonarr_main", fmt.Sprintf("Show %03d", i)
	rel := fmt.Sprintf("Show.%03d/Show.%03d.S01E01.mkv", i, i)
	libDir := filepath.Join(r.root, "TV Shows", cat, name)
	if plan == ClassNewMovie {
		cat, name = "radarr_main", fmt.Sprintf("Movie %03d 2026", i) // year token → new release
		rel = fmt.Sprintf("Movie.%03d.2026/Movie.%03d.2026.mkv", i, i)
		libDir = filepath.Join(r.root, "Movies", cat, name)
	}
	r.inner.mu.Lock()
	r.inner.sizes[fileKey{hash, 1}] = size
	r.inner.mu.Unlock()
	require.NoError(r.t, r.store.UpsertRelease(&catalog.Release{
		Hash: hash, Name: name, Category: cat, State: catalog.StateVirtual, Cached: true,
		Source: catalog.SourceTorrent, AddedOn: r.clock().Unix() - 3600, TotalSize: size,
	}, []catalog.File{{Hash: hash, FileID: 1, RelPath: rel, Size: size}}))
	require.NoError(r.t, os.MkdirAll(libDir, 0o755))
	link := filepath.Join(libDir, filepath.Base(rel))
	require.NoError(r.t, os.Symlink(filepath.Join(r.root, "lazarr_mount", hash, rel), link))
	r.eng.walkLinks()
	f := &pfile{idx: i, hash: hash, rel: rel, link: link, size: size, plan: plan}
	r.files[short(hash)] = f
	return f
}

// waitOutcome blocks until the puller logged the result of a pull of hash.
func (r *rig) waitOutcome(hash string, mark int) string {
	r.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		for _, l := range r.logs.since(mark) {
			if l.attrs["hash"] != short(hash) {
				continue
			}
			switch l.msg {
			case "localcache: pulled":
				r.stats.pulls++
				r.pulled[short(hash)] = true
				r.notePull()
				return outcomePulled
			case "localcache: not enough free space to pull":
				r.stats.refused++
				return outcomeRefused
			case "localcache: pull failed":
				r.stats.failed++
				return outcomeFailed
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.t.Fatalf("no pull outcome for %s", short(hash))
	return ""
}

func (r *rig) notePull() {
	// The pull's own goroutine may still be retargeting links; the bytes are final.
	if p := r.freePct(); p < r.stats.minAfterPullPct {
		r.stats.minAfterPullPct = p
	}
}

// pull asks for one file the way production does: wanted files through a playback
// session + PollOnce (planner, reason watch), the rest through Enqueue (prefetch).
func (r *rig) pull(f *pfile) string {
	r.t.Helper()
	pre := r.snapshot()
	mark := r.logs.mark()
	if f.plan == ClassWanted {
		r.wanted = append(r.wanted, f)
		r.setSessions()
		r.eng.PollOnce(r.ctx)
	} else if !r.eng.Enqueue(f.hash, 1, ReasonPrefetch) {
		r.t.Fatalf("enqueue %s refused by a static guard", short(f.hash))
	}
	out := r.waitOutcome(f.hash, mark)
	if ev := evictionsIn(r.logs.since(mark)); len(ev) > 0 {
		r.stats.makeRoomPasses++
		r.checkPass(pre, ev, r.snapshot(), short(f.hash))
	}
	return out
}

func (r *rig) setSessions() {
	ss := make([]playback.Session, 0, len(r.wanted))
	for _, w := range r.wanted {
		ss = append(ss, playback.Session{
			Server: "jellyfin", UserID: "viewer", Kind: playback.KindMovie, ItemID: short(w.hash),
			Path: w.link, Position: 10 * time.Minute, Duration: 2 * time.Hour,
		})
	}
	r.src.mu.Lock()
	r.src.sessions = ss
	r.src.mu.Unlock()
}

// poll is the 30 s session poll: keeps the wanted set fresh and re-asks for any
// wanted file that is not local yet.
func (r *rig) poll() {
	r.t.Helper()
	var missing []*pfile
	for _, w := range r.wanted {
		if lf, err := r.store.GetLocalFile(w.hash, 1); err != nil || lf.State != catalog.LocalComplete {
			missing = append(missing, w)
		}
	}
	mark := r.logs.mark()
	r.eng.PollOnce(r.ctx)
	for _, w := range missing {
		r.waitOutcome(w.hash, mark)
	}
}

// signal gives a freshly pulled file the playback history that makes it the planned
// class, through the same inputs production uses (watch rows, local reads, sessions).
func (r *rig) signal(f *pfile) {
	r.t.Helper()
	now := r.clock().Unix()
	switch f.plan {
	case ClassFinished:
		require.NoError(r.t, r.store.UpsertWatch(&catalog.Watch{Server: "jellyfin", UserID: "u1", Hash: f.hash, FileID: 1,
			SeriesKey: "fin-" + short(f.hash), Season: 1, Episode: 1, ProgressPct: 100, Completed: true, FirstSeen: now, LastSeen: now}))
	case ClassWatching:
		require.NoError(r.t, r.store.UpsertWatch(&catalog.Watch{Server: "jellyfin", UserID: "u2", Hash: f.hash, FileID: 1,
			SeriesKey: "watch-" + short(f.hash), Season: 1, Episode: 1, ProgressPct: 40, FirstSeen: now, LastSeen: now}))
	case ClassDefault:
		buf := make([]byte, 4096)
		_, err := r.eng.ReadAt(f.hash, 1, buf, 0) // a local read stamps last_read
		require.NoError(r.t, err)
	}
}

type preRow struct {
	row   catalog.LocalFile
	watch []catalog.Watch
}

func (r *rig) snapshot() map[string]preRow {
	rows, err := r.store.ListLocalFiles("")
	require.NoError(r.t, err)
	out := map[string]preRow{}
	for _, lf := range rows {
		if lf.State != catalog.LocalComplete {
			continue
		}
		ws, _ := r.store.ListWatchByFile(lf.Hash, lf.FileID)
		p := preRow{row: *lf}
		for _, w := range ws {
			p.watch = append(p.watch, *w)
		}
		out[short(lf.Hash)] = p
	}
	return out
}

func classRank(s string) Class {
	for c := ClassWanted; c <= ClassUnreferenced; c++ {
		if c.String() == s {
			return c
		}
	}
	return ClassDefault
}

// sweep runs one reaper pass and checks it against the policy: wanted and pinned
// files survive, pressure evictions go finished → untouched → default → new_movie →
// watching (oldest anchor first), and free space ends above the low-water mark
// unless only wanted/pinned files are left.
func (r *rig) sweep() (evicted []eviction, stuck bool) {
	r.t.Helper()
	pre := r.snapshot()
	mark := r.logs.mark()
	r.eng.Sweep(r.ctx)
	r.stats.sweeps++
	evicted = evictionsIn(r.logs.since(mark))
	post := r.snapshot()
	r.checkPass(pre, evicted, post, "")
	pinned := r.cfg.TTL.Watching.D() == 0
	free := r.freePct()
	if free < r.stats.minPostSweepPct {
		r.stats.minPostSweepPct = free
	}
	if free <= float64(r.cfg.FreeLowPct) {
		for h, p := range post {
			cls := classRank(p.row.Class)
			assert.True(r.t, cls == ClassWanted || (cls == ClassWatching && pinned),
				"free %.2f%% <= low-water after a sweep but evictable %s (%s) survived", free, h, p.row.Class)
		}
		stuck = true
		r.stats.stuckSweeps++
	}
	return evicted, stuck
}

func evictionsIn(recs []logRec) []eviction {
	var out []eviction
	for _, l := range recs {
		if l.msg == "localcache: evicted" {
			out = append(out, eviction{l.attrs["hash"], l.attrs["class"], l.attrs["reason"]})
		}
	}
	return out
}

// checkPass checks one retention pass (a reaper sweep, or the puller's make-room
// pass before a pull) against the policy: wanted and pinned files are never evicted,
// evicted files' links are back on FUSE, pressure evictions go finished → untouched
// → default → new_movie → watching by oldest anchor, and nothing more evictable (or
// older in the same class) survived a pressure eviction. skip is the file being
// pulled (not a candidate of the pass).
func (r *rig) checkPass(pre map[string]preRow, evicted []eviction, post map[string]preRow, skip string) {
	r.t.Helper()
	pinned := r.cfg.TTL.Watching.D() == 0
	anchor := func(p preRow) time.Time { return Anchor(Candidate{File: p.row, Watch: p.watch}) }
	var pressure []eviction
	for _, ev := range evicted {
		assert.NotEqual(r.t, ClassWanted.String(), ev.class, "wanted file %s evicted", ev.hash)
		if pinned {
			assert.NotEqual(r.t, ClassWatching.String(), ev.class, "pinned watching file %s evicted", ev.hash)
		}
		if f := r.files[ev.hash]; f != nil {
			assert.NotEqual(r.t, ClassWanted, f.plan, "planned-wanted file %s evicted", ev.hash)
			assert.Equal(r.t, filepath.Join(r.root, "lazarr_mount", f.hash, f.rel), readlinkOr(f.link), "evicted file's link must be back on FUSE")
		}
		if ev.reason == "pressure" {
			pressure = append(pressure, ev)
		}
	}
	// Pressure evictions are in policy order...
	for i := 1; i < len(pressure); i++ {
		a, b := pressure[i-1], pressure[i]
		ra, rb := classRank(a.class), classRank(b.class)
		assert.True(r.t, ra > rb || (ra == rb && !anchor(pre[a.hash]).After(anchor(pre[b.hash]))),
			"pressure order: %s(%s) before %s(%s)", a.hash, a.class, b.hash, b.class)
	}
	// ...and nothing more evictable (or older in the same class) was left behind.
	for _, ev := range pressure {
		for h, p := range post {
			cls := classRank(p.row.Class)
			if h == skip || cls == ClassWanted || (cls == ClassWatching && pinned) {
				continue
			}
			er := classRank(ev.class)
			assert.False(r.t, cls > er, "%s (%s) survived while %s (%s) was evicted for pressure", h, p.row.Class, ev.hash, ev.class)
			if cls == er {
				assert.False(r.t, anchor(pre[h]).Before(anchor(pre[ev.hash])), "%s is older than evicted %s in class %s", h, ev.hash, ev.class)
			}
		}
	}
}

func readlinkOr(p string) string {
	t, err := os.Readlink(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return t
}

// partFiles lists .lazarr-part files anywhere under the cache dir.
func (r *rig) partFiles() []string {
	var out []string
	_ = filepath.WalkDir(r.cfg.Dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, partSuffix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// verifyLocal checks a complete file's bytes on disk and that the decorator serves
// them without touching the inner engine (playback is local).
func (r *rig) verifyLocal(f *pfile) {
	r.t.Helper()
	lf, err := r.store.GetLocalFile(f.hash, 1)
	require.NoError(r.t, err, "%s should be local", short(f.hash))
	require.Equal(r.t, catalog.LocalComplete, lf.State)
	assert.Equal(r.t, filepath.Join(r.cfg.Dir, f.hash, f.rel), readlinkOr(f.link), "link on the cache copy")
	before := r.inner.reads.Load()
	buf := make([]byte, 64<<10)
	for _, off := range []int64{0, f.size / 2, f.size - int64(len(buf))} {
		n, err := r.eng.ReadAt(f.hash, 1, buf, off)
		require.NoError(r.t, err)
		require.Equal(r.t, len(buf), n)
		for i := 0; i < n; i += 4093 {
			require.Equal(r.t, byte((off+int64(i))%251), buf[i], "byte at %d of %s", off+int64(i), short(f.hash))
		}
	}
	assert.Equal(r.t, before, r.inner.reads.Load(), "local file must be served from disk")
}

// verifyStreams checks a non-local file still plays through the inner engine.
func (r *rig) verifyStreams(f *pfile) {
	r.t.Helper()
	before := r.inner.reads.Load()
	buf := make([]byte, 4096)
	n, err := r.eng.ReadAt(f.hash, 1, buf, f.size/3)
	require.NoError(r.t, err)
	require.Equal(r.t, 4096, n)
	assert.Equal(r.t, byte((f.size/3)%251), buf[0])
	assert.Greater(r.t, r.inner.reads.Load(), before, "evicted/unpulled file streams via the inner engine")
}

// ballast is "another process" filling the same filesystem.
type ballast struct {
	t    *testing.T
	path string
	size int64
}

func (r *rig) newBallast() *ballast {
	return &ballast{t: r.t, path: filepath.Join(r.fsDir, "ballast.bin")}
}

// setFree grows or shrinks the ballast so the volume has ~free bytes available.
func (b *ballast) setFree(r *rig, free uint64) {
	b.t.Helper()
	cur := r.disk().Free
	switch {
	case cur > free:
		b.grow(int64(cur - free)) //nolint:gosec // bounded by the fs size
	case cur < free:
		shrink := int64(free - cur) //nolint:gosec
		if shrink > b.size {
			shrink = b.size
		}
		b.size -= shrink
		require.NoError(b.t, os.Truncate(b.path, b.size))
	}
}

func (b *ballast) grow(n int64) {
	b.t.Helper()
	f, err := os.OpenFile(b.path, os.O_WRONLY|os.O_CREATE, 0o600)
	require.NoError(b.t, err)
	defer func() { _ = f.Close() }()
	_, err = f.Seek(b.size, 0)
	require.NoError(b.t, err)
	chunk := make([]byte, 4<<20)
	for i := range chunk {
		chunk[i] = 0xa5
	}
	for left := n; left > 0; {
		w := min(left, int64(len(chunk)))
		k, err := f.Write(chunk[:w])
		b.size += int64(k)
		require.NoError(b.t, err)
		left -= int64(k)
	}
	require.NoError(b.t, f.Sync())
}

// evictions tallies every eviction the engine logged (reaper and make-room passes).
func (r *rig) evictions() (byKey map[string]int, order []string) {
	byKey = map[string]int{}
	for _, ev := range evictionsIn(r.logs.since(0)) {
		byKey[ev.class+"/"+ev.reason]++
		order = append(order, ev.class)
	}
	return byKey, order
}

func (r *rig) report(name string) {
	byKey, _ := r.evictions()
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ev []string
	for _, k := range keys {
		ev = append(ev, fmt.Sprintf("%s=%d", k, byKey[k]))
	}
	r.t.Logf("RESULT %s: fs=%dMiB pulls=%d refused=%d failed=%d sweeps=%d stuck_sweeps=%d make_room_passes=%d min_free_sampled=%.2f%% min_free_after_pull=%.2f%% min_free_after_sweep=%.2f%% evictions[%s]",
		name, r.total>>20, r.stats.pulls, r.stats.refused, r.stats.failed, r.stats.sweeps, r.stats.stuckSweeps, r.stats.makeRoomPasses,
		r.minFreePct(), r.stats.minAfterPullPct, r.stats.minPostSweepPct, strings.Join(ev, " "))
}

// ---- scenarios -------------------------------------------------------------------

// (a) Sustained growth: far more bytes than the volume holds, mixed classes, one
// puller, reaper every simulated minute.
func TestPressureSustainedGrowth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned bool
	}{{"watching_pinned_live", true}, {"watching_unpinned_ttl_10d", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(c *config.LocalCache) {
				if !tc.pinned {
					c.TTL.Watching = config.Duration(240 * time.Hour)
				}
			})
			rng := rand.New(rand.NewPCG(42, 7)) //nolint:gosec // deterministic test data
			cycle := []Class{ClassFinished, ClassUntouched, ClassDefault, ClassNewMovie, ClassUntouched, ClassFinished, ClassDefault}
			plan := func(i int) Class {
				switch i {
				case 0:
					return ClassWanted
				case 3, 17:
					return ClassWatching
				}
				return cycle[i%len(cycle)]
			}
			const n = 60
			var all []*pfile
			var total int64
			for i := 0; i < n; i++ {
				size := int64(40+rng.IntN(41)) << 20
				f := r.addFile(i, plan(i), size)
				all = append(all, f)
				total += size
				if r.pull(f) == outcomePulled {
					r.signal(f)
				}
				r.advance(time.Minute)
				r.poll()
				r.sweep()
				assert.Empty(t, r.partFiles(), "no .part left behind after pull %d", i)
			}
			r.report("a/" + tc.name)
			t.Logf("RESULT a/%s: offered %d MiB to a %d MiB volume (%.1fx)", tc.name, total>>20, r.total>>20, float64(total)/float64(r.total))

			// Hard floor: the cache never took the volume below reserve_pct.
			assert.GreaterOrEqual(t, r.minFreePct(), float64(r.cfg.ReservePct), "reserve breached")
			// Criterion 4: never below free_low_pct because of the cache.
			assert.Greater(t, r.minFreePct(), float64(r.cfg.FreeLowPct), "free space dropped below low-water because of the cache")
			assert.Greater(t, float64(total), 2*float64(r.total), "the scenario must offer well past capacity")
			byKey, _ := r.evictions()
			assert.Positive(t, byKey["finished/pressure"], "finished evicted under pressure")
			assert.Positive(t, byKey["untouched/pressure"], "untouched evicted under pressure")
			assert.Zero(t, r.stats.refused, "evictable files always made room")

			// The wanted file and (when pinned) the watching files survived all of it.
			for _, f := range all {
				switch {
				case f.plan == ClassWanted:
					r.verifyLocal(f)
				case f.plan == ClassWatching && tc.pinned:
					require.True(t, r.pulled[short(f.hash)], "watching file %d was pulled", f.idx)
					r.verifyLocal(f)
				}
			}
			// Everything evicted still plays (through the inner engine).
			for _, f := range all {
				if _, err := r.store.GetLocalFile(f.hash, 1); errors.Is(err, catalog.ErrNotFound) {
					r.verifyStreams(f)
				}
			}
		})
	}
}

// (b) Reserve: a pull that would leave less than reserve_pct free is refused with no
// .part on disk, playback keeps streaming, and the planner's next poll pulls it once
// space frees.
func TestPressureReserveRefusesAndRetries(t *testing.T) {
	r := newRig(t, nil)
	b := r.newBallast()
	reserve := r.total / 100 * uint64(r.cfg.ReservePct)
	const size = 64 << 20
	// Free = size + reserve - 4 MiB: the pull would end 4 MiB inside the reserve.
	b.setFree(r, size+reserve-4<<20)
	freeBefore := r.disk().Free

	f := r.addFile(0, ClassWanted, size)
	require.Equal(t, outcomeRefused, r.pull(f))
	assert.Empty(t, r.partFiles(), "a refused pull leaves no .part")
	_, err := r.store.GetLocalFile(f.hash, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound, "a refused pull leaves no row")
	assert.InDelta(t, float64(freeBefore), float64(r.disk().Free), 64<<10, "a refused pull writes nothing")
	assert.Equal(t, filepath.Join(r.root, "lazarr_mount", f.hash, f.rel), readlinkOr(f.link), "link stays on FUSE")
	r.verifyStreams(f)

	// Still full on the next poll: refused again (the planner keeps asking).
	r.advance(30 * time.Second)
	r.poll()
	assert.Equal(t, 2, r.stats.refused)
	assert.Empty(t, r.partFiles())

	// Another process frees 40 MiB: the next poll pulls it.
	b.setFree(r, r.disk().Free+40<<20)
	r.advance(30 * time.Second)
	r.poll()
	assert.Equal(t, 1, r.stats.pulls)
	r.verifyLocal(f)
	assert.GreaterOrEqual(t, r.minFreePct(), float64(r.cfg.ReservePct))
	r.report("b/reserve")
}

// (b2) Refusal that only eviction can resolve: the volume sits between the low-water
// mark and size+reserve, so the reaper sees no pressure and the pull is refused,
// while an evictable finished file holds the space the wanted file needs.
func TestPressureReserveMakesRoomFromEvictable(t *testing.T) {
	r := newRig(t, nil)
	b := r.newBallast()
	old := r.addFile(0, ClassFinished, 60<<20)
	require.Equal(t, outcomePulled, r.pull(old))
	r.signal(old)
	r.advance(time.Minute)
	r.sweep()

	reserve := r.total / 100 * uint64(r.cfg.ReservePct)
	const size = 64 << 20
	b.setFree(r, size+reserve-4<<20) // ~13% free: above low-water, below size+reserve
	require.Greater(t, r.freePct(), float64(r.cfg.FreeLowPct))
	f := r.addFile(1, ClassWanted, size)
	out := r.pull(f)
	r.advance(30 * time.Second)
	r.sweep() // the regular reaper pass: no pressure at ~13% free
	r.poll()
	r.sweep()
	r.report("b2/make_room")
	t.Logf("RESULT b2: first attempt=%s", out)
	r.verifyLocal(f)
	_, err := r.store.GetLocalFile(old.hash, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound, "the finished file made room")
	r.verifyStreams(old)
	assert.Greater(t, r.minFreePct(), float64(r.cfg.FreeLowPct))
}

// (c) Another process fills the disk: the reaper evicts down to just above the
// low-water mark in policy order; once only wanted/pinned files remain it logs and
// stops, and playback keeps working.
func TestPressureBallastFill(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pinned bool
		order  []string
	}{
		{"watching_pinned_live", true, []string{"finished", "finished", "untouched", "untouched", "default", "new_movie"}},
		{"watching_unpinned_ttl_10d", false, []string{"finished", "finished", "untouched", "untouched", "default", "new_movie", "watching"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(c *config.LocalCache) {
				if !tc.pinned {
					c.TTL.Watching = config.Duration(240 * time.Hour)
				}
			})
			ballastFill(t, r, tc.name, tc.pinned, tc.order)
		})
	}
}

func ballastFill(t *testing.T, r *rig, name string, pinned bool, order []string) {
	b := r.newBallast()
	plans := []struct {
		c    Class
		size int64
	}{
		{ClassWanted, 60}, {ClassWatching, 50}, {ClassFinished, 45}, {ClassUntouched, 40},
		{ClassDefault, 45}, {ClassNewMovie, 50}, {ClassFinished, 55}, {ClassUntouched, 50},
	}
	var all []*pfile
	for i, p := range plans {
		f := r.addFile(i, p.c, p.size<<20)
		require.Equal(t, outcomePulled, r.pull(f))
		r.signal(f)
		all = append(all, f)
		r.advance(time.Minute)
		r.poll()
	}
	ev, _ := r.sweep()
	require.Empty(t, ev, "plenty of room: nothing evicted")
	t.Logf("RESULT c/%s: cache populated, free %.1f%%", name, r.freePct())

	low := r.total / 100 * uint64(r.cfg.FreeLowPct)
	var rounds []string
	for round := 0; round < 12; round++ {
		b.setFree(r, low*6/10) // another process pushes free to ~3%
		pushed := r.freePct()
		mark := r.logs.mark()
		evicted, stuck := r.sweep()
		var cls []string
		for _, e := range evicted {
			cls = append(cls, e.class)
		}
		rounds = append(rounds, fmt.Sprintf("round %d: pushed to %.2f%% → evicted [%s] → %.2f%% free (stuck=%v)",
			round, pushed, strings.Join(cls, ","), r.freePct(), stuck))
		if !stuck {
			assert.Greater(t, r.freePct(), float64(r.cfg.FreeLowPct), "reaper clears the low-water mark")
			continue
		}
		require.Empty(t, evicted, "stuck means nothing evictable is left")
		warned := false
		for _, l := range r.logs.since(mark) {
			if l.level >= slog.LevelWarn && strings.Contains(l.msg, "low-water") {
				warned = true
			}
		}
		assert.True(t, warned, "the sweep logs a WARN when only wanted/pinned files are left below low-water")
		break
	}
	for _, s := range rounds {
		t.Logf("RESULT c/%s: %s", name, s)
	}
	r.report("c/" + name)
	assert.Equal(t, 1, r.stats.stuckSweeps, "the fill ends with only wanted/pinned files left")
	// Wanted (+ pinned watching) survive and play locally; everything else streams.
	for _, f := range all {
		if f.plan == ClassWanted || (f.plan == ClassWatching && pinned) {
			r.verifyLocal(f)
		} else {
			r.verifyStreams(f)
		}
	}
	_, got := r.evictions()
	assert.Equal(t, order, got, "class order across the fill")
}

// (d) Scale: TTLs shrink linearly from free_high_pct to free_low_pct, measured on the
// real statfs, and a finished file is TTL-evicted once its scaled TTL drops under its age.
func TestPressureScale(t *testing.T) {
	r := newRig(t, nil)
	b := r.newBallast()
	f := r.addFile(0, ClassFinished, 40<<20)
	require.Equal(t, outcomePulled, r.pull(f))
	r.signal(f)
	r.advance(25 * time.Hour) // finished TTL 48h: survives until scale < 25/48 ≈ 0.52

	evictedAt := -1.0
	prev := 2.0
	for _, target := range []float64{40, 25, 20, 17.5, 15, 12.5, 10, 7.5, 5, 3} {
		b.setFree(r, uint64(target/100*float64(r.total)))
		d := r.disk()
		got := Scale(d, r.cfg)
		free := d.FreePct()
		want := 1.0
		switch {
		case free <= 5:
			want = MinScale
		case free < 20:
			want = MinScale + (1-MinScale)*(free-5)/15
		}
		assert.InDelta(t, want, got, 1e-9, "scale at %.2f%% free", free)
		assert.LessOrEqual(t, got, prev, "scale never grows as free drops")
		prev = got
		line := fmt.Sprintf("free %5.2f%% scale %.3f ttl finished %6.1fh untouched %5.1fd default %5.1fd new_movie %6.1fd",
			free, got, got*r.cfg.TTL.Finished.D().Hours(), got*r.cfg.TTL.Untouched.D().Hours()/24,
			got*r.cfg.TTL.Default.D().Hours()/24, got*r.cfg.TTL.NewMovie.D().Hours()/24)
		if evictedAt < 0 {
			ev, _ := r.sweep()
			if len(ev) > 0 {
				require.Equal(t, "ttl", ev[0].reason, "evicted by its shrunken TTL, not by pressure")
				evictedAt = free
				line += "  ← 25h-old finished file TTL-evicted"
			}
		}
		t.Logf("RESULT d: %s", line)
	}
	r.report("d/scale")
	// 48h × scale < 25h ⇔ scale < 0.5208 ⇔ free < ~12.0%.
	assert.InDelta(t, 10, evictedAt, 0.5, "first evicted at the 10%% step")
}
