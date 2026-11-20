package localcache

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/playback/arr"
)

// releaseMeta caches one arr lookup per release.
type releaseMeta struct {
	fetched time.Time
	info    *arr.GrabInfo
	failed  bool
}

const (
	metaRefresh = 24 * time.Hour
	metaRetry   = time.Hour
	// episodeNewnessMultiplier: a freshly aired episode of a continuing series keeps
	// the new_movie TTL for active_window × this (docs/28 §7.4).
	episodeNewnessMultiplier = 7
	// theatricalFallback: when only the cinema date is known, digital availability is
	// assumed this long after it.
	theatricalFallback = 90 * 24 * time.Hour
)

var yearRE = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// newness returns the best-known release date, whether the item is a movie, and the
// window during which it counts as new (docs/28 §7.4, inputs in precision order:
// arr → media server → release-name year).
func (e *Engine) newness(rel *catalog.Release, now time.Time) (date time.Time, isMovie bool, window time.Duration) {
	isMovie = e.movieCat[rel.Category]
	if _, known := e.movieCat[rel.Category]; !known {
		isMovie = true
	}
	window = e.cfg.NewReleaseWindow.D()

	// 1. Arr (precise) when a client is configured for the category.
	if info := e.arrInfo(rel, now); info != nil {
		switch info.Kind {
		case arr.KindRadarr:
			isMovie = true
			switch {
			case !info.ReleaseDate.IsZero():
				return info.ReleaseDate, true, window
			case !info.InCinemas.IsZero():
				return info.InCinemas.Add(theatricalFallback), true, window
			}
		case arr.KindSonarr:
			isMovie = false
			if !info.AirDate.IsZero() {
				if info.SeriesStatus == "continuing" || info.SeriesStatus == "upcoming" {
					return info.AirDate, false, e.cfg.ActiveWindow.D() * episodeNewnessMultiplier
				}
				return info.AirDate, false, 0 // ended series: air date is not "new"
			}
		}
	}

	// 2. Media-server date seen in a session.
	e.mu.Lock()
	obs, ok := e.obsDate[rel.Hash]
	e.mu.Unlock()
	if ok && !obs.IsZero() {
		if isMovie {
			return obs.Add(theatricalFallback), true, window
		}
		return obs, false, e.cfg.ActiveWindow.D() * episodeNewnessMultiplier
	}

	// 3. Year token in the release name (movies only): assume mid-year.
	if isMovie {
		if y := yearFromName(rel.Name, now.Year()); y > 0 {
			return time.Date(y, 7, 1, 0, 0, 0, 0, time.UTC), true, window
		}
	}
	return time.Time{}, isMovie, 0
}

// yearFromName returns the last plausible 19xx/20xx token that is <= maxYear, or 0.
func yearFromName(name string, maxYear int) int {
	best := 0
	for _, m := range yearRE.FindAllString(name, -1) {
		y, err := strconv.Atoi(m)
		if err == nil && y >= 1900 && y <= maxYear {
			best = y
		}
	}
	return best
}

// arrInfo returns the cached arr lookup for a release, fetching (bounded) on a miss.
func (e *Engine) arrInfo(rel *catalog.Release, now time.Time) *arr.GrabInfo {
	client := e.arrs[rel.Category]
	if client == nil {
		return nil
	}
	e.mu.Lock()
	m := e.meta[rel.Hash]
	if m != nil {
		fresh := now.Sub(m.fetched) < metaRefresh
		if m.failed {
			fresh = now.Sub(m.fetched) < metaRetry
		}
		if fresh {
			e.mu.Unlock()
			return m.info
		}
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	info, err := client.LookupGrab(ctx, rel.Hash)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.log.Debug("localcache: arr lookup", "category", rel.Category, "hash", short(rel.Hash), "err", err)
		e.meta[rel.Hash] = &releaseMeta{fetched: now, failed: true, info: nil}
		if m != nil {
			e.meta[rel.Hash].info = m.info // keep the last good answer
		}
		return e.meta[rel.Hash].info
	}
	e.meta[rel.Hash] = &releaseMeta{fetched: now, info: info}
	return info
}
