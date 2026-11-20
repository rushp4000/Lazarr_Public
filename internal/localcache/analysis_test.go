package localcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback"
)

// fakeAnalyzer is a media server that can analyze on demand. It records whether the
// library link pointed at the cache tree at the moment each request was made — the
// invariant that keeps analysis off the CDN.
type fakeAnalyzer struct {
	*fakeSource
	mu        sync.Mutex
	link      string // the library path it has "indexed" ("" = not indexed yet)
	cacheRoot string
	started   []string
	localAt   []bool
	busyPolls int      // AnalysisStatus reports busy this many times after a start
	season    []string // SeasonPaths result (nil = a movie)
	actions   [][]string
	gateOpen  bool
	gateLog   []string
}

func (f *fakeAnalyzer) SeasonPaths(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.season...), nil
}

func (f *fakeAnalyzer) OpenGates(context.Context, []string) (func(context.Context) error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateOpen = true
	f.gateLog = append(f.gateLog, "open")
	return func(context.Context) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gateOpen = false
		f.gateLog = append(f.gateLog, "close")
		return nil
	}, nil
}

func (f *fakeAnalyzer) ResetGates(context.Context, []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateLog = append(f.gateLog, "reset")
	return nil
}

func (f *fakeAnalyzer) FindItems(_ context.Context, path string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.link != "" && path == f.link {
		return []string{"7"}, nil
	}
	return nil, nil
}

func (f *fakeAnalyzer) Preflight(context.Context, []string) ([]string, error) { return nil, nil }

func (f *fakeAnalyzer) StartAnalysis(_ context.Context, id string, actions []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	tgt, _ := os.Readlink(f.link)
	f.started = append(f.started, id)
	f.actions = append(f.actions, append([]string(nil), actions...))
	if f.gateLog != nil && !f.gateOpen {
		f.localAt = append(f.localAt, false) // requested with gates closed: counts as a failure below
	}
	f.localAt = append(f.localAt, strings.HasPrefix(tgt, f.cacheRoot+string(filepath.Separator)))
	return nil
}

func (f *fakeAnalyzer) AnalysisStatus(context.Context, string) (playback.AnalysisStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busyPolls > 0 {
		f.busyPolls--
		return playback.AnalysisStatus{Busy: true}, nil
	}
	return playback.AnalysisStatus{Intro: 1, Credits: 1}, nil
}

func (f *fakeAnalyzer) calls() ([]string, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]bool(nil), f.localAt...)
}

func analysisHarness(t *testing.T) (*harness, *fakeAnalyzer) {
	fa := &fakeAnalyzer{fakeSource: &fakeSource{name: "plex", next: map[string][]playback.Item{}}, busyPolls: 2}
	h := newHarness(t, func(h *harness) {
		h.pb.Analysis = config.Analysis{
			Enabled: true, Actions: []string{"intro", "credits"},
			Timeout: config.Duration(time.Hour), MaxHold: config.Duration(6 * time.Hour),
			ScanWait: config.Duration(30 * time.Minute), PollInterval: config.Duration(5 * time.Millisecond),
		}
		h.source = fa
	})
	fa.cacheRoot = h.cfg.Dir
	return h, fa
}

func waitAnalysis(t *testing.T, h *harness, want catalog.AnalysisState) *catalog.LocalAnalysis {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *catalog.LocalAnalysis
	for time.Now().Before(deadline) {
		rows, _ := h.eng.astore.ListLocalAnalysis(hashA, 1)
		if len(rows) == 1 {
			last = rows[0]
			if last.State == want {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("analysis never reached %s (last: %+v)", want, last)
	return nil
}

func TestAnalysisRunsAfterRetargetWhileLocal(t *testing.T) {
	h, fa := analysisHarness(t)
	link := h.addRelease(hashA, "sonarr_main", "Show", "Show.S01/ep1.mkv", 1<<20, catalog.SourceTorrent)
	fa.link = link
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonGrab))
	h.waitComplete(hashA)

	row := waitAnalysis(t, h, catalog.AnalysisDone)
	assert.Equal(t, "7", row.ItemIDs)
	assert.Equal(t, 1, row.Intro)
	assert.Equal(t, 1, row.Credits)
	assert.Equal(t, 1, row.Attempts)
	ids, local := fa.calls()
	assert.Equal(t, []string{"7"}, ids)
	assert.Equal(t, []bool{true}, local, "request only sent while the link points at the cache")

	// Backfill skips files whose analysis is done.
	n, err := h.eng.AnalyzeBackfill()
	require.NoError(t, err)
	assert.Zero(t, n)
	views := h.eng.LocalFiles()
	require.Len(t, views, 1)
	require.Len(t, views[0].Analysis, 1)
	assert.Equal(t, catalog.AnalysisDone, views[0].Analysis[0].State)
}

func TestAnalysisSkippedWhenNotLocal(t *testing.T) {
	h, fa := analysisHarness(t)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	fa.link = link
	// Publish failed: the file completes but its link stays on FUSE.
	h.eng.publish = func(string, string) error { return os.ErrPermission }
	h.eng.cfg.Publish = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)

	n, err := h.eng.AnalyzeRelease(hashA)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	waitAnalysis(t, h, catalog.AnalysisSkipped)
	ids, _ := fa.calls()
	assert.Empty(t, ids, "never analyzed through FUSE")
}

func TestAnalysisUnavailableWhenNeverIndexed(t *testing.T) {
	h, fa := analysisHarness(t)
	link := h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonGrab))
	h.waitComplete(hashA)
	waitLink(t, link, filepath.Join(h.cfg.Dir, hashA, "ep1.mkv"))
	waitAnalysis(t, h, catalog.AnalysisPending)

	h.advance(31 * time.Minute) // past scan_wait; fa.link == "" → never found
	waitAnalysis(t, h, catalog.AnalysisUnavailable)
	ids, _ := fa.calls()
	assert.Empty(t, ids)
}

func TestAnalysisHoldBlocksTTLEvictionUntilMaxHold(t *testing.T) {
	h, _ := analysisHarness(t)
	h.addRelease(hashA, "sonarr_main", "Show", "ep1.mkv", 1<<20, catalog.SourceTorrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Not started: drive the sweep by hand. Complete, finished-by-everyone file.
	require.NoError(t, os.MkdirAll(filepath.Join(h.cfg.Dir, hashA), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.cfg.Dir, hashA, "ep1.mkv"), h.bytesOf(hashA), 0o644))
	lf := &catalog.LocalFile{Hash: hashA, FileID: 1, State: catalog.LocalComplete, Size: 1 << 20, Frontier: 1 << 20, PulledAt: h.now.Unix()}
	require.NoError(t, h.store.UpsertLocalFile(lf))
	h.eng.files[fileKey{hashA, 1}] = &fileState{LocalFile: *lf, rel: "ep1.mkv"}
	require.NoError(t, h.store.UpsertWatch(&catalog.Watch{Server: "plex", UserID: "u", Hash: hashA, FileID: 1, Completed: true, FirstSeen: h.now.Unix(), LastSeen: h.now.Unix()}))
	require.NoError(t, h.eng.astore.UpsertLocalAnalysis(&catalog.LocalAnalysis{Hash: hashA, FileID: 1, Server: "plex", State: catalog.AnalysisRunning, RequestedAt: h.now.Unix()}))

	h.advance(49 * time.Hour) // past the 48h finished TTL, but max_hold is 6h → gives up
	h.eng.Sweep(ctx)
	_, err := h.store.GetLocalFile(hashA, 1)
	assert.ErrorIs(t, err, catalog.ErrNotFound, "evicted once the hold gave up")
	rows, _ := h.eng.astore.ListLocalAnalysis(hashA, 1)
	require.Len(t, rows, 1)
	assert.Equal(t, catalog.AnalysisTimeout, rows[0].State, "a given-up analysis is never re-queued automatically")
}

func TestDecideRespectsAnalysisHold(t *testing.T) {
	cfg := config.Default().LocalCache
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	c := Candidate{
		File:  catalog.LocalFile{Hash: hashA, FileID: 1, State: catalog.LocalComplete, Size: 1, PulledAt: now.Add(-100 * time.Hour).Unix()},
		Watch: []catalog.Watch{{Completed: true, LastSeen: now.Add(-100 * time.Hour).Unix()}},
		Links: 1, LinksCheckedAt: now,
	}
	disk := DiskStat{Free: 90, Total: 100}
	ev, _ := Decide([]Candidate{c}, disk, now, cfg)
	require.Len(t, ev, 1, "finished past TTL is evicted")
	c.AnalysisHold = true
	ev, _ = Decide([]Candidate{c}, disk, now, cfg)
	assert.Empty(t, ev, "held while its analysis is pending")
	ev, _ = Decide([]Candidate{c}, DiskStat{Free: 1, Total: 100}, now, cfg)
	assert.Len(t, ev, 1, "disk pressure still wins over the hold")
}

func runAnalysis(t *testing.T, mutate func(h *harness, fa *fakeAnalyzer)) (*harness, *fakeAnalyzer, string) {
	t.Helper()
	h, fa := analysisHarness(t)
	link := h.addRelease(hashA, "sonarr_main", "Show", "Show.S01/ep1.mkv", 1<<20, catalog.SourceTorrent)
	fa.link = link
	if mutate != nil {
		mutate(h, fa)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.eng.Start(ctx)
	h.eng.walkLinks()
	require.True(t, h.eng.Enqueue(hashA, 1, ReasonWatch))
	h.waitComplete(hashA)
	waitAnalysis(t, h, catalog.AnalysisDone)
	return h, fa, link
}

func TestIntroOnlyWhenWholeSeasonLocal(t *testing.T) {
	// The season has a second episode that is still on FUSE → intro is held back.
	other := ""
	h, fa, _ := runAnalysis(t, func(h *harness, fa *fakeAnalyzer) {
		other = h.addRelease(hashB, "sonarr_main", "Show", "Show.S01/ep2.mkv", 1<<20, catalog.SourceTorrent)
		fa.season = []string{fa.link, other}
	})
	_ = h
	assert.Equal(t, [][]string{{"credits"}}, fa.actions, "intro waits for the whole season")
	rows, _ := h.eng.astore.ListLocalAnalysis(hashA, 1)
	assert.Contains(t, rows[0].Error, "whole season")
}

func TestIntroRequestedWhenSeasonLocal(t *testing.T) {
	_, fa, _ := runAnalysis(t, func(_ *harness, fa *fakeAnalyzer) {
		fa.season = []string{fa.link} // one-episode season, and it is local after the pull
	})
	assert.Equal(t, [][]string{{"intro", "credits"}}, fa.actions)
}

func TestMovieNeverGetsIntro(t *testing.T) {
	_, fa, _ := runAnalysis(t, nil) // season nil → a movie
	assert.Equal(t, [][]string{{"credits"}}, fa.actions)
}

func TestGatesOpenOnlyAroundTheRequest(t *testing.T) {
	_, fa, _ := runAnalysis(t, func(h *harness, fa *fakeAnalyzer) {
		h.eng.pb.Analysis.OpenGates = true
		h.eng.pb.Analysis.AvoidHours = ""
		fa.gateLog = []string{}
	})
	_, local := fa.calls()
	assert.Equal(t, []bool{true}, local, "request made while local and with the gate open")
	fa.mu.Lock()
	defer fa.mu.Unlock()
	assert.Equal(t, []string{"reset", "open", "close"}, fa.gateLog, "reset at start, open for the request, closed after")
	assert.False(t, fa.gateOpen)
}

func TestInAvoidHours(t *testing.T) {
	e := &Engine{}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 10, h, m, 0, 0, time.Local) }
	e.pb.Analysis.AvoidHours = "01:30-05:30"
	assert.True(t, e.inAvoidHours(at(2, 0)))
	assert.False(t, e.inAvoidHours(at(5, 30)))
	assert.False(t, e.inAvoidHours(at(12, 0)))
	e.pb.Analysis.AvoidHours = "23:00-02:00" // wraps midnight
	assert.True(t, e.inAvoidHours(at(23, 30)))
	assert.True(t, e.inAvoidHours(at(1, 0)))
	assert.False(t, e.inAvoidHours(at(3, 0)))
	e.pb.Analysis.AvoidHours = ""
	assert.False(t, e.inAvoidHours(at(2, 0)))
}
