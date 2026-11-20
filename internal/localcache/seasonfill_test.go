package localcache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback"
)

func hashN(n int) string { return fmt.Sprintf("%040x", n) }

// seasonHarness: series "s1" season 1 E1..E6 + S02E01, each its own release with a
// library link; a viewer is on E3. Nothing is started, so the queue can be inspected.
func seasonHarness(t *testing.T, completed int, mutate func(h *harness)) (*harness, map[int]string) {
	h := newHarness(t, func(h *harness) {
		h.cfg.SeasonFill = config.SeasonFill{Enabled: true, AfterCompleted: 2, MaxEpisodes: 30}
		h.cfg.SpeculativeGBPerDay = 250
		h.cfg.PrefetchAhead = 0
		h.cfg.PullCurrent = false
		if mutate != nil {
			mutate(h)
		}
	})
	links := map[int]string{}
	for ep := 1; ep <= 7; ep++ {
		name := fmt.Sprintf("S01E%02d", ep)
		if ep == 7 {
			name = "S02E01"
		}
		links[ep] = h.addRelease(hashN(ep), "sonarr_main", "Show", name+"/"+name+".mkv", 1<<20, catalog.SourceTorrent)
	}
	var next []playback.Item
	for ep := 4; ep <= 7; ep++ {
		season, num := 1, ep
		if ep == 7 {
			season, num = 2, 1
		}
		next = append(next, playback.Item{ItemID: fmt.Sprint(ep), Season: season, Episode: num, Path: links[ep]})
	}
	h.src.next["s1"] = next
	h.src.sessions = []playback.Session{{Server: "jellyfin", UserID: "u1", Kind: playback.KindEpisode, SeriesKey: "s1",
		Season: 1, Episode: 3, Path: links[3], Position: time.Minute, Duration: 40 * time.Minute}}
	for ep := 1; ep <= completed; ep++ {
		require.NoError(t, h.store.UpsertWatch(&catalog.Watch{Server: "jellyfin", UserID: "u1", Hash: hashN(ep), FileID: 1,
			SeriesKey: "s1", Season: 1, Episode: ep, ProgressPct: 98, Completed: true, FirstSeen: h.now.Unix(), LastSeen: h.now.Unix()}))
	}
	return h, links
}

func queued(h *harness) []string {
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	var out []string
	for _, r := range h.eng.queue {
		out = append(out, fmt.Sprintf("%s:%s", r.key.hash[38:], r.reason))
	}
	return out
}

func TestSeasonFillAfterCommitment(t *testing.T) {
	h, _ := seasonHarness(t, 2, nil)
	h.eng.PollOnce(context.Background())
	assert.Equal(t, []string{"04:season", "05:season", "06:season"}, queued(h),
		"rest of THIS season only, never the next season")

	// A second poll does not re-query/requeue (recheck interval).
	h.eng.PollOnce(context.Background())
	assert.Len(t, queued(h), 3)
}

func TestSeasonFillWaitsForCommitment(t *testing.T) {
	h, _ := seasonHarness(t, 1, nil)
	h.eng.PollOnce(context.Background())
	assert.Empty(t, queued(h), "one finished episode is not enough with after_completed=2")
}

func TestSeasonFillOffByDefaultAndWithZeroBudget(t *testing.T) {
	assert.False(t, config.Default().LocalCache.SeasonFill.Enabled, "season fill ships off")
	h, _ := seasonHarness(t, 2, func(h *harness) { h.cfg.SpeculativeGBPerDay = 0 })
	h.eng.PollOnce(context.Background())
	assert.Empty(t, queued(h))
}

func TestLiveSessionPullsJumpAheadAndPromote(t *testing.T) {
	h, _ := seasonHarness(t, 2, func(h *harness) { h.cfg.PrefetchAhead = 1 })
	h.eng.PollOnce(context.Background())
	// prefetch E4 queued first as a live-session pull; E5, E6 as season fill.
	assert.Equal(t, []string{"04:prefetch", "05:season", "06:season"}, queued(h))

	// A manual pull of E6 promotes it ahead of the speculative tail.
	require.False(t, h.eng.Enqueue(hashN(6), 1, ReasonManual), "already queued")
	assert.Equal(t, []string{"04:prefetch", "06:manual", "05:season"}, queued(h))
}

func TestSpeculativeBudgetRollingDay(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.cfg.SpeculativeGBPerDay = 1 })
	now := h.now
	assert.True(t, h.eng.specBudgetOK(now, 600<<20))
	assert.False(t, h.eng.specBudgetOK(now, 600<<20), "would exceed 1 GB in 24 h")
	assert.True(t, h.eng.specBudgetOK(now.Add(25*time.Hour), 600<<20), "window rolled")
}

func TestFinishedMovieKeptLongerThanEpisode(t *testing.T) {
	cfg := config.Default().LocalCache
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	mk := func(hash string, movie bool) Candidate {
		return Candidate{
			File:  catalog.LocalFile{Hash: hash, FileID: 1, State: catalog.LocalComplete, Size: 1, PulledAt: now.Add(-72 * time.Hour).Unix()},
			Watch: []catalog.Watch{{Completed: true, LastSeen: now.Add(-72 * time.Hour).Unix()}},
			Links: 1, LinksCheckedAt: now, IsMovie: movie,
		}
	}
	ev, _ := Decide([]Candidate{mk(hashA, true), mk(hashB, false)}, DiskStat{Free: 90, Total: 100}, now, cfg)
	require.Len(t, ev, 1)
	assert.Equal(t, hashB, ev[0].Hash, "finished episode (48h) goes; finished movie (30d) stays")
}
