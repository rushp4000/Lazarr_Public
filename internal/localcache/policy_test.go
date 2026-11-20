package localcache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
)

func testCfg() config.LocalCache {
	c := config.Default().LocalCache // balanced: finished 48h, untouched 14d, new_movie 180d, default 30d, 20/5
	c.Enabled = true
	return c
}

var now = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

func cand(hash string, fileID int, size int64, pulledAgo time.Duration) Candidate {
	return Candidate{
		File:           catalog.LocalFile{Hash: hash, FileID: fileID, State: catalog.LocalComplete, Size: size, PulledAt: now.Add(-pulledAgo).Unix()},
		Release:        catalog.Release{Hash: hash, Source: catalog.SourceTorrent},
		Links:          1,
		LinksCheckedAt: now,
	}
}

func watch(user string, ago time.Duration, completed bool) catalog.Watch {
	return catalog.Watch{Server: "jellyfin", UserID: user, LastSeen: now.Add(-ago).Unix(), Completed: completed}
}

func TestScale(t *testing.T) {
	cfg := testCfg() // high 20, low 5
	assert.Equal(t, 1.0, Scale(DiskStat{Free: 50, Total: 100}, cfg))
	assert.Equal(t, 1.0, Scale(DiskStat{Free: 20, Total: 100}, cfg))
	assert.InDelta(t, 0.55, Scale(DiskStat{Free: 125, Total: 1000}, cfg), 1e-9) // 12.5% → halfway between 0.1 and 1
	assert.Equal(t, MinScale, Scale(DiskStat{Free: 5, Total: 100}, cfg))
	assert.Equal(t, MinScale, Scale(DiskStat{Free: 1, Total: 100}, cfg))
	assert.Equal(t, 1.0, Scale(DiskStat{}, cfg), "unknown disk = no pressure")
}

func TestClassify(t *testing.T) {
	cfg := testCfg()
	cases := []struct {
		name string
		c    Candidate
		want Class
	}{
		{"wanted wins over everything", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.Wanted = true
			c.Links = 0
			c.LinksCheckedAt = now.Add(-2 * time.Hour)
			return c
		}(), ClassWanted},
		{"unreferenced after grace", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.Links = 0
			c.LinksCheckedAt = now.Add(-2 * time.Hour) // grace 1h
			return c
		}(), ClassUnreferenced},
		{"unreferenced inside grace is not yet", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.Links = 0
			c.LinksCheckedAt = now.Add(-10 * time.Minute)
			return c
		}(), ClassUntouched},
		{"watching: viewer mid-file", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.Watch = []catalog.Watch{watch("u1", 30*time.Minute, false)}
			return c
		}(), ClassWatching},
		{"watching: series active by another viewer", func() Candidate {
			c := cand("a", 1, 10, 3*24*time.Hour)
			c.Watch = []catalog.Watch{watch("u1", 5*24*time.Hour, true)}
			c.SeriesActive = true
			return c
		}(), ClassWatching},
		{"finished: everyone completed", func() Candidate {
			c := cand("a", 1, 10, 3*24*time.Hour)
			c.Watch = []catalog.Watch{watch("u1", 2*24*time.Hour, true), watch("u2", 24*time.Hour, true)}
			return c
		}(), ClassFinished},
		{"not finished while one viewer is mid-file but stale → default", func() Candidate {
			c := cand("a", 1, 10, 30*24*time.Hour)
			c.Watch = []catalog.Watch{watch("u1", 10*24*time.Hour, false)} // beyond active_window
			return c
		}(), ClassDefault},
		{"new movie", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.IsMovie = true
			c.ReleaseDate = now.Add(-30 * 24 * time.Hour)
			c.Newness = cfg.NewReleaseWindow.D()
			return c
		}(), ClassNewMovie},
		{"old movie never played → untouched", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.IsMovie = true
			c.ReleaseDate = now.Add(-5 * 365 * 24 * time.Hour)
			c.Newness = cfg.NewReleaseWindow.D()
			return c
		}(), ClassUntouched},
		{"read locally but never observed by a server → default", func() Candidate {
			c := cand("a", 1, 10, time.Hour)
			c.File.LastRead = now.Unix()
			return c
		}(), ClassDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.c, now, cfg), "got %s", Classify(tc.c, now, cfg))
		})
	}
}

func TestDecideTTLAndScaling(t *testing.T) {
	cfg := testCfg()
	// finished 48h ago at full headroom: expired.
	fin := cand("f", 1, 100, 5*24*time.Hour)
	fin.Watch = []catalog.Watch{watch("u1", 49*time.Hour, true)}
	// finished 30h ago: survives at full headroom, but dies at 50% scale (24h).
	finYoung := cand("g", 1, 100, 5*24*time.Hour)
	finYoung.Watch = []catalog.Watch{watch("u1", 30*time.Hour, true)}
	// untouched pulled 10 days ago: survives at 14d TTL.
	unt := cand("u", 1, 100, 10*24*time.Hour)
	// wanted, ancient: never evicted.
	want := cand("w", 1, 100, 400*24*time.Hour)
	want.Wanted = true

	evict, classes := Decide([]Candidate{fin, finYoung, unt, want}, DiskStat{Free: 50, Total: 100}, now, cfg)
	require.Len(t, evict, 1)
	assert.Equal(t, "f", evict[0].Hash)
	assert.Equal(t, "ttl", evict[0].Reason)
	assert.Equal(t, ClassFinished, classes[[2]any{"f", 1}])
	assert.Equal(t, ClassWanted, classes[[2]any{"w", 1}])

	// Halfway between the water marks (scale 0.55): finished TTL is 26.4h, so the
	// 30h-old finished file goes too; untouched (14d*0.55=7.7d) at 10d goes as well.
	evict, _ = Decide([]Candidate{fin, finYoung, unt, want}, DiskStat{Free: 125, Total: 1000}, now, cfg)
	hashes := map[string]bool{}
	for _, e := range evict {
		hashes[e.Hash] = true
		assert.Equal(t, "ttl", e.Reason)
	}
	assert.True(t, hashes["f"] && hashes["g"] && hashes["u"], "%v", evict)
	assert.False(t, hashes["w"], "wanted is never evicted")
}

func TestDecidePressureOrder(t *testing.T) {
	cfg := testCfg()
	// Everything young; the volume is at 3% free (below low water 5%) with 1000 total.
	fin := cand("fin", 1, 10, time.Hour)
	fin.Watch = []catalog.Watch{watch("u1", time.Hour, true)}
	unt := cand("unt", 1, 10, time.Hour)
	def := cand("def", 1, 10, time.Hour)
	def.File.LastRead = now.Unix()
	watching := cand("wat", 1, 10, time.Hour)
	watching.Watch = []catalog.Watch{watch("u2", time.Minute, false)}
	wanted := cand("want", 1, 10, time.Hour)
	wanted.Wanted = true

	// Need 30 bytes freed to get from 30 to 60 free out of 1000 (6% > 5%).
	evict, _ := Decide([]Candidate{watching, wanted, def, unt, fin}, DiskStat{Free: 30, Total: 1000}, now, cfg)
	var order []string
	for _, e := range evict {
		order = append(order, e.Hash)
		assert.Equal(t, "pressure", e.Reason)
	}
	assert.Equal(t, []string{"fin", "unt", "def"}, order, "finished → untouched → default, stop once above low water")

	// If only pinned/wanted remain, nothing more is evicted (playback falls back to proxy).
	evict, _ = Decide([]Candidate{watching, wanted}, DiskStat{Free: 1, Total: 1000}, now, cfg)
	assert.Empty(t, evict)

	// Unreferenced always goes, even with plenty of room.
	unref := cand("unref", 1, 10, time.Hour)
	unref.Links = 0
	unref.LinksCheckedAt = now.Add(-2 * time.Hour)
	evict, _ = Decide([]Candidate{unref}, DiskStat{Free: 900, Total: 1000}, now, cfg)
	require.Len(t, evict, 1)
	assert.Equal(t, "unreferenced", evict[0].Reason)
}

func TestAnchorUsesMostRecentSignal(t *testing.T) {
	c := cand("a", 1, 10, 10*time.Hour)
	assert.Equal(t, now.Add(-10*time.Hour).Unix(), Anchor(c).Unix())
	c.File.LastRead = now.Add(-5 * time.Hour).Unix()
	assert.Equal(t, now.Add(-5*time.Hour).Unix(), Anchor(c).Unix())
	c.Watch = []catalog.Watch{watch("u", 2*time.Hour, false), watch("v", 7*time.Hour, true)}
	assert.Equal(t, now.Add(-2*time.Hour).Unix(), Anchor(c).Unix())
}

// Pending (bytes a pull is about to write) counts as used, even when it exceeds Free:
// the make-room pass must clear the low-water mark for the volume after the pull lands.
// A saturating "Free - Pending" would floor at 0 and stop one file too early.
func TestDecidePendingCountsBytesAboutToLand(t *testing.T) {
	cfg := testCfg()
	a := cand("a", 1, 100, 3*time.Hour)
	a.Watch = []catalog.Watch{watch("u1", 3*time.Hour, true)}
	b := cand("b", 1, 100, 2*time.Hour)
	b.Watch = []catalog.Watch{watch("u1", 2*time.Hour, true)}
	c := cand("c", 1, 100, time.Hour)
	c.Watch = []catalog.Watch{watch("u1", time.Hour, true)}
	cands := []Candidate{c, b, a}

	// 150 free of 1000 is 15%: no pressure without a pull.
	evict, _ := Decide(cands, DiskStat{Free: 150, Total: 1000}, now, cfg)
	assert.Empty(t, evict)

	// A 250-byte pull would leave -10%: free 150 bytes more than the deficit plus
	// low-water (50) → two 100-byte files, oldest first.
	evict, _ = Decide(cands, DiskStat{Free: 150, Total: 1000, Pending: 250}, now, cfg)
	var got []string
	for _, e := range evict {
		got = append(got, e.Hash)
		assert.Equal(t, "pressure", e.Reason)
	}
	assert.Equal(t, []string{"a", "b"}, got)
	assert.InDelta(t, -10.0, DiskStat{Free: 150, Total: 1000, Pending: 250}.FreePct(), 1e-9)
	assert.Equal(t, MinScale, Scale(DiskStat{Free: 150, Total: 1000, Pending: 250}, cfg))
}
