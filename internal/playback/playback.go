// Package playback turns Plex / Jellyfin "now playing" state into one neutral
// Session shape and joins media-server library paths back to catalog releases by
// readlink. Nothing Plex- or Jellyfin-specific escapes the two client sub-packages;
// the local-cache planner (internal/localcache) only ever sees these types.
// Workstream G; see docs/28 §3.
package playback

import (
	"context"
	"time"
)

// Kind is the media type of a session's item.
type Kind string

const (
	KindMovie   Kind = "movie"
	KindEpisode Kind = "episode"
)

// State is the coarse playback state.
type State string

const (
	StatePlaying   State = "playing"
	StatePaused    State = "paused"
	StateBuffering State = "buffering"
	StateStopped   State = "stopped"
)

// Session is one live playback observation. Position/Duration are wall durations
// (Plex reports milliseconds, Jellyfin 100 ns ticks — both normalized here).
type Session struct {
	Server    string `json:"server"` // "plex" | "jellyfin"
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`

	ItemID      string `json:"item_id"`
	Kind        Kind   `json:"kind"`
	Title       string `json:"title"`
	SeriesKey   string `json:"series_key"` // server-native series id ("" for movies)
	SeriesTitle string `json:"series_title"`
	Season      int    `json:"season"`
	Episode     int    `json:"episode"`
	// Path is the library path exactly as the server reports it (before path_map).
	Path string `json:"path"`

	Position time.Duration `json:"position"`
	Duration time.Duration `json:"duration"`
	State    State         `json:"state"`

	// ReleaseDate is the server's premiere / originally-available date (theatrical for
	// movies, air date for episodes); zero when unknown. Year is the production year.
	ReleaseDate time.Time `json:"release_date"`
	Year        int       `json:"year"`

	Observed time.Time `json:"observed"`
}

// ProgressPct is 0..100 (0 when the duration is unknown).
func (s Session) ProgressPct() float64 {
	if s.Duration <= 0 {
		return 0
	}
	p := float64(s.Position) / float64(s.Duration) * 100
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// Item is one library item with its path — what NextEpisodes returns.
type Item struct {
	ItemID  string `json:"item_id"`
	Title   string `json:"title"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Path    string `json:"path"`
}

// Source is a media server the planner can poll. Implementations must be safe for
// concurrent use and must not block longer than the request context allows.
type Source interface {
	// Name is the stable server label used in watch rows ("plex", "jellyfin").
	Name() string
	// Sessions returns every live video session (movies and episodes only).
	Sessions(ctx context.Context) ([]Session, error)
	// NextEpisodes returns up to n episodes that follow s in series order (crossing
	// season boundaries; specials excluded), nearest first, each with its library path.
	NextEpisodes(ctx context.Context, s Session, n int) ([]Item, error)
	// Refresh asks the server to re-scan one item (used only when
	// playback.refresh_on_retarget is on).
	Refresh(ctx context.Context, itemID string) error
}

// SeriesOrder sorts episodes for "what comes next": by season then episode; season 0
// (specials) sorts last so it never counts as "next" after a regular episode.
func SeriesLess(aSeason, aEp, bSeason, bEp int) bool {
	as, bs := seasonRank(aSeason), seasonRank(bSeason)
	if as != bs {
		return as < bs
	}
	return aEp < bEp
}

func seasonRank(season int) int {
	if season <= 0 {
		return 1 << 30
	}
	return season
}

// After reports whether (season, ep) comes strictly after the session's position.
func After(s Session, season, ep int) bool {
	return SeriesLess(s.Season, s.Episode, season, ep)
}

// Analyzer is optionally implemented by a Source whose server can analyze one item on
// demand (docs/28 §16). Every method must be bounded by ctx.
type Analyzer interface {
	Name() string
	// FindItems returns the ids of the items whose media file is exactly path (several
	// for a multi-episode file; none when the server has not indexed it yet).
	FindItems(ctx context.Context, path string) ([]string, error)
	// Preflight reports, once at start, why requested actions would be ignored
	// (subscription missing, server-wide setting off). Warnings are not errors.
	Preflight(ctx context.Context, actions []string) ([]string, error)
	// StartAnalysis asks the server to run the actions for one item. Asynchronous.
	StartAnalysis(ctx context.Context, itemID string, actions []string) error
	// AnalysisStatus reports whether the server is still busy analyzing and how many
	// intro/credits markers the item has now.
	AnalysisStatus(ctx context.Context, itemID string) (AnalysisStatus, error)
	// SeasonPaths returns the library paths of every episode in the item's season
	// (nil for a movie). Used to request season-wide work (intro detection reads the
	// whole season) only when the whole season is local.
	SeasonPaths(ctx context.Context, itemID string) ([]string, error)
	// OpenGates temporarily enables server-wide settings that would otherwise make
	// the server ignore the actions; the returned func restores them. It must be
	// safe to call restore more than once. ResetGates restores unconditionally (boot).
	OpenGates(ctx context.Context, actions []string) (restore func(context.Context) error, err error)
	ResetGates(ctx context.Context, actions []string) error
}

// AnalysisStatus is one observation of an item's analysis.
type AnalysisStatus struct {
	Busy    bool // the server has analysis work running (server-wide; concurrency is 1)
	Intro   int
	Credits int
}
