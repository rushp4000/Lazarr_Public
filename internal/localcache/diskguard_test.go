package localcache

import (
	"context"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback"
)

// logCapture is a slog.Handler that records Info+ records for assertions.
type logCapture struct {
	mu   sync.Mutex
	recs []logRec
	echo func(logRec)
}

type logRec struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logCapture) WithGroup(string) slog.Handler            { return l }
func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo {
		return nil
	}
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.String(); return true })
	rec := logRec{r.Level, r.Message, m}
	l.mu.Lock()
	l.recs = append(l.recs, rec)
	l.mu.Unlock()
	if l.echo != nil {
		l.echo(rec)
	}
	return nil
}

func (l *logCapture) mark() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.recs) }

func (l *logCapture) since(i int) []logRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]logRec(nil), l.recs[i:]...)
}

func (l *logCapture) has(level slog.Level, substr string) bool {
	for _, r := range l.since(0) {
		if r.level >= level && strings.Contains(r.msg, substr) {
			return true
		}
	}
	return false
}

// fakeVolume makes the engine see a small volume: total bytes, `other` used by
// somebody else, plus whatever actually sits under the cache dir (complete files and
// parts), so pulls and evictions move the numbers like a real filesystem.
func fakeVolume(t *testing.T, h *harness, total, other uint64) {
	t.Helper()
	h.eng.statDisk = func() (DiskStat, error) {
		var used uint64
		_ = filepath.WalkDir(h.cfg.Dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, ierr := d.Info(); ierr == nil {
					used += uint64(fi.Size()) //nolint:gosec // non-negative
				}
			}
			return nil
		})
		used += other
		if used > total {
			used = total
		}
		return DiskStat{Free: total - used, Total: total}, nil
	}
}

func waitGone(t *testing.T, h *harness, hash string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.store.GetLocalFile(hash, 1); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never evicted", hash[:8])
}

// A pull that would leave the volume at/below free_low_pct makes room first, by the
// pressure order, instead of being refused while evictable files hold the space
// (the wedge the disk-pressure test found: free between low-water and size+reserve).
func TestPullMakesRoomBeforeCrossingLowWater(t *testing.T) {
	logs := &logCapture{}
	h := newHarness(t, nil)
	h.eng.log = slog.New(logs)
	const mib = 1 << 20
	// 10 MiB volume, 2 MiB used by someone else; low-water 5% (0.5 MiB), reserve 2%.
	fakeVolume(t, h, 10*mib, 2*mib)
	h.addRelease(hashA, "sonarr_main", "Old", "old.mkv", 3*mib, catalog.SourceTorrent)
	h.addRelease(hashB, "sonarr_main", "Unt", "unt.mkv", 3*mib, catalog.SourceTorrent)
	const hashC = "dddddddddddddddddddddddddddddddddddddddd"
	h.addRelease(hashC, "sonarr_main", "New", "new.mkv", 3*mib, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()

	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	require.NoError(t, h.store.UpsertWatch(&catalog.Watch{Server: "jellyfin", UserID: "u1", Hash: hashA, FileID: 1, SeriesKey: "a", Completed: true, FirstSeen: h.now.Unix(), LastSeen: h.now.Unix()}))
	h.advance(time.Minute)
	require.True(t, h.eng.Enqueue(hashB, 1, ReasonPrefetch))
	h.waitComplete(hashB)
	h.eng.Sweep(ctx)
	d, _ := h.eng.statDisk()
	require.InDelta(t, 20.0, d.FreePct(), 0.01, "2 MiB free: above low-water, below 3 MiB + reserve")

	// Before the fix this pull was refused forever (reaper: no pressure at 20%).
	require.True(t, h.eng.Enqueue(hashC, 1, ReasonPrefetch))
	h.waitComplete(hashC)
	waitGone(t, h, hashA)
	_, err := h.store.GetLocalFile(hashB, 1)
	assert.NoError(t, err, "only as much as needed: untouched B stays")
	assert.False(t, logs.has(slog.LevelWarn, "not enough free space"))
	d, _ = h.eng.statDisk()
	assert.Greater(t, d.FreePct(), float64(h.cfg.FreeLowPct))
}

// When only wanted files are left the pull is still refused by reserve_pct (no part
// written), and the reaper logs that it is stuck instead of evicting a wanted file.
func TestStuckBelowLowWaterWarnsAndKeepsWanted(t *testing.T) {
	logs := &logCapture{}
	h := newHarness(t, nil)
	h.eng.log = slog.New(logs)
	const mib = 1 << 20
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 3*mib, catalog.SourceTorrent)
	h.addRelease(hashB, "sonarr_main", "Next", "ep2.mkv", 3*mib, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	h.src.sessions = []playback.Session{{Server: "jellyfin", UserID: "u1", Kind: playback.KindEpisode, SeriesKey: "s", Season: 1, Episode: 1, Path: link, Position: time.Minute, Duration: time.Hour}}
	h.eng.PollOnce(ctx)

	// Somebody else fills the volume: 0.3 MiB free of 10 MiB (3%).
	fakeVolume(t, h, 10*mib, 10*mib-3*mib-3*mib/10)
	mark := logs.mark()
	h.eng.Sweep(ctx)
	lf, err := h.store.GetLocalFile(hashA, 1)
	require.NoError(t, err, "wanted file is never evicted")
	assert.Equal(t, "wanted", lf.Class)
	warned := false
	for _, r := range logs.since(mark) {
		if r.level == slog.LevelWarn && strings.Contains(r.msg, "low-water") {
			warned = true
			assert.Equal(t, "1", r.attrs["wanted"])
		}
	}
	assert.True(t, warned, "stuck sweep logs at WARN")
	h.eng.Sweep(ctx)
	n := 0
	for _, r := range logs.since(mark) {
		if strings.Contains(r.msg, "low-water") {
			n++
		}
	}
	assert.Equal(t, 1, n, "the warning is rate-limited")

	// A further pull is refused by the reserve check and writes nothing.
	mark = logs.mark()
	require.True(t, h.eng.Enqueue(hashB, 1, ReasonPrefetch))
	deadline := time.Now().Add(5 * time.Second)
	for !logsHave(logs, mark, "not enough free space") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, logsHave(logs, mark, "not enough free space"))
	_, err = h.store.GetLocalFile(hashB, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound)
	_, err = h.store.GetLocalFile(hashA, 1)
	assert.NoError(t, err, "the make-room pass never evicts a wanted file either")

	// Playback of the non-local file keeps working through the inner engine.
	buf := make([]byte, 16)
	_, err = h.eng.ReadAt(hashB, 1, buf, 0)
	require.NoError(t, err)
}

func logsHave(l *logCapture, mark int, substr string) bool {
	for _, r := range l.since(mark) {
		if strings.Contains(r.msg, substr) {
			return true
		}
	}
	return false
}
