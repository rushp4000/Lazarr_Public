package localcache

import (
	"sort"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
)

// Class is the retention classification of one cached file (docs/28 §7.1). Lower
// values are "more protected": eviction under disk pressure walks the classes from
// the bottom of this list upward.
type Class int

const (
	ClassWanted       Class = iota // live session on it, or inside a viewer's prefetch window
	ClassWatching                  // its series/movie has recent activity by someone who is not done
	ClassNewMovie                  // movie (or fresh episode) inside the new-release window
	ClassDefault                   // nothing special
	ClassUntouched                 // pulled, never played by anyone
	ClassFinished                  // every viewer completed it; nobody mid-series
	ClassUnreferenced              // no library link references it any more
)

func (c Class) String() string {
	switch c {
	case ClassWanted:
		return "wanted"
	case ClassWatching:
		return "watching"
	case ClassNewMovie:
		return "new_movie"
	case ClassUntouched:
		return "untouched"
	case ClassFinished:
		return "finished"
	case ClassUnreferenced:
		return "unreferenced"
	default:
		return "default"
	}
}

// Candidate is everything the policy needs to know about one cached file. Built by
// the reaper from the catalog; kept plain so the policy is a pure function.
type Candidate struct {
	File    catalog.LocalFile
	Release catalog.Release
	Watch   []catalog.Watch
	// Wanted: a live session is on this file or it sits inside a viewer's prefetch
	// window (computed by the planner from the last poll).
	Wanted bool
	// SeriesActive: some viewer has a non-completed watch row on the same series
	// (or on this movie) within active_window.
	SeriesActive bool
	// Links is the number of library symlinks referencing this file (Lazarr's own
	// download_dir links excluded).
	Links int
	// LinksCheckedAt, when Links is 0, is when the file was first seen with no
	// library link: unreferenced_grace runs from here. Zero = never unreferenced.
	LinksCheckedAt time.Time
	// ReleaseDate is the best-known release/air date (zero = unknown).
	ReleaseDate time.Time
	// IsMovie says the release came from a movie category (or the arr said so).
	IsMovie bool
	// Newness is the effective new-release window for this item (movies:
	// new_release_window; episodes of continuing series: active_window*7).
	Newness time.Duration
	// AnalysisHold: a media-server analysis of this file is pending (bounded by
	// playback.analysis.max_hold). Blocks TTL eviction; disk pressure still wins.
	AnalysisHold bool
}

// DiskStat is the cache volume's headroom.
type DiskStat struct {
	Free  uint64
	Total uint64
	// Pending is bytes a pull is about to write (the puller's make-room pass): the
	// policy sees the volume as if they had already landed. It may exceed Free.
	Pending uint64
}

// FreePct is the free share after Pending lands: <= 100, negative when Pending
// exceeds Free.
func (d DiskStat) FreePct() float64 {
	if d.Total == 0 {
		return 100
	}
	return (float64(d.Free) - float64(d.Pending)) / float64(d.Total) * 100
}

// MinScale is the floor of the TTL multiplier at/below free_low_pct. TTLs never go
// all the way to zero: below the low-water mark the ordered pressure pass (finished →
// untouched → default …) decides what goes, not a blanket "everything expired".
const MinScale = 0.1

// Scale maps free space to a TTL multiplier: 1.0 at/above free_high_pct, MinScale
// at/below free_low_pct, linear between. A pure function so it can be table-tested.
func Scale(disk DiskStat, cfg config.LocalCache) float64 {
	high, low := float64(cfg.FreeHighPct), float64(cfg.FreeLowPct)
	if high <= low {
		return 1
	}
	free := disk.FreePct()
	switch {
	case free >= high:
		return 1
	case free <= low:
		return MinScale
	default:
		return MinScale + (1-MinScale)*(free-low)/(high-low)
	}
}

// Classify assigns the retention class (docs/28 §7.1, priority order).
func Classify(c Candidate, now time.Time, cfg config.LocalCache) Class {
	if c.Wanted {
		return ClassWanted
	}
	if c.Links == 0 && !c.LinksCheckedAt.IsZero() &&
		now.Sub(c.LinksCheckedAt) >= 0 && now.Sub(c.LinksCheckedAt) >= cfg.UnreferencedGrace.D() {
		return ClassUnreferenced
	}
	active := cfg.ActiveWindow.D()
	var (
		anyWatch     bool
		allCompleted = true
		lastSeen     time.Time
	)
	for _, w := range c.Watch {
		anyWatch = true
		if !w.Completed {
			allCompleted = false
		}
		if t := time.Unix(w.LastSeen, 0); t.After(lastSeen) {
			lastSeen = t
		}
	}
	if anyWatch && !allCompleted && now.Sub(lastSeen) <= active {
		return ClassWatching
	}
	if c.SeriesActive {
		return ClassWatching
	}
	if anyWatch && allCompleted {
		return ClassFinished
	}
	if !c.ReleaseDate.IsZero() && c.Newness > 0 && now.Sub(c.ReleaseDate) <= c.Newness {
		return ClassNewMovie
	}
	if !anyWatch && c.File.LastRead == 0 {
		return ClassUntouched
	}
	return ClassDefault
}

// TTL returns the base retention for a class. Wanted is never evicted (returns a
// sentinel of 0 with keep=true); unreferenced has no TTL of its own (already past
// its grace) and evicts immediately.
func TTL(class Class, cfg config.LocalCache) (ttl time.Duration, keep bool) {
	switch class {
	case ClassWanted:
		return 0, true
	case ClassWatching:
		if cfg.TTL.Watching.D() == 0 {
			return 0, true // pinned while active
		}
		return cfg.TTL.Watching.D(), false
	case ClassFinished:
		return cfg.TTL.Finished.D(), false
	case ClassUntouched:
		return cfg.TTL.Untouched.D(), false
	case ClassNewMovie:
		return cfg.TTL.NewMovie.D(), false
	case ClassUnreferenced:
		return 0, false
	default:
		return cfg.TTL.Default.D(), false
	}
}

// Anchor is the moment a candidate's retention clock starts: the most recent of the
// last watch activity (any user), the last local read, and the pull completion.
func Anchor(c Candidate) time.Time {
	t := time.Unix(c.File.PulledAt, 0)
	if c.File.LastRead > c.File.PulledAt {
		t = time.Unix(c.File.LastRead, 0)
	}
	for _, w := range c.Watch {
		if wt := time.Unix(w.LastSeen, 0); wt.After(t) {
			t = wt
		}
	}
	return t
}

// Decision is one file the reaper should evict and why.
type Decision struct {
	Hash   string
	FileID int
	Class  Class
	Reason string // "ttl" | "pressure" | "unreferenced"
}

// Decide is the eviction brain (docs/28 §7.1): (1) everything past its scaled TTL
// goes, except wanted; (2) if still below free_low_pct, evict in class order
// finished → untouched → default → new_movie → watching(if unpinned) by
// least-recently-anchored until the low-water mark is cleared. Wanted files are
// never evicted. classes is filled with every candidate's classification so the
// caller can persist it for the UI.
func Decide(cands []Candidate, disk DiskStat, now time.Time, cfg config.LocalCache) (evict []Decision, classes map[[2]any]Class) {
	scale := Scale(disk, cfg)
	classes = make(map[[2]any]Class, len(cands))
	type scored struct {
		c      Candidate
		class  Class
		anchor time.Time
	}
	var survivors []scored
	var freed uint64
	key := func(c Candidate) [2]any { return [2]any{c.File.Hash, c.File.FileID} }

	for _, c := range cands {
		class := Classify(c, now, cfg)
		classes[key(c)] = class
		if class == ClassWanted {
			continue
		}
		if class == ClassUnreferenced {
			evict = append(evict, Decision{c.File.Hash, c.File.FileID, class, "unreferenced"})
			freed += uint64(c.File.Size) //nolint:gosec // sizes are non-negative
			continue
		}
		ttl, keep := TTL(class, cfg)
		if class == ClassFinished && c.IsMovie && cfg.TTL.FinishedMovie.D() > 0 {
			ttl = cfg.TTL.FinishedMovie.D() // movies are re-watched far more than episodes
		}
		anchor := Anchor(c)
		if !keep && !c.AnalysisHold {
			expires := anchor.Add(time.Duration(float64(ttl) * scale))
			if !now.Before(expires) {
				evict = append(evict, Decision{c.File.Hash, c.File.FileID, class, "ttl"})
				freed += uint64(c.File.Size) //nolint:gosec
				continue
			}
		}
		survivors = append(survivors, scored{c, class, anchor})
	}

	// Pressure pass: still under the low-water mark after TTL evictions?
	if lowWater(disk, freed, cfg) {
		sort.SliceStable(survivors, func(i, j int) bool {
			if survivors[i].class != survivors[j].class {
				return survivors[i].class > survivors[j].class // finished(5) before untouched(4) before default(3)...
			}
			return survivors[i].anchor.Before(survivors[j].anchor)
		})
		for _, s := range survivors {
			if !lowWater(disk, freed, cfg) {
				break
			}
			if s.class == ClassWatching && cfg.TTL.Watching.D() == 0 {
				continue // pinned while active
			}
			evict = append(evict, Decision{s.c.File.Hash, s.c.File.FileID, s.class, "pressure"})
			freed += uint64(s.c.File.Size) //nolint:gosec
		}
	}
	return evict, classes
}

// lowWater reports whether, after freeing `freed` bytes, the volume is still at or
// below free_low_pct.
func lowWater(disk DiskStat, freed uint64, cfg config.LocalCache) bool {
	if disk.Total == 0 {
		return false
	}
	free := (float64(disk.Free+freed) - float64(disk.Pending)) / float64(disk.Total) * 100
	return free <= float64(cfg.FreeLowPct)
}
