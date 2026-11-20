// Package arr talks to the Radarr/Sonarr instance that made a Lazarr grab. One key
// per category; absent keys simply mean "no arr integration" for that category.
//
// Almost everything here is read-only: LookupGrab learns what a grab is (title,
// season/episodes) and how new it is (digital/physical release or air date) for
// the local cache (docs/28 §7.4, §7.6). The one write is FailDownload (queue.go),
// used only when policy.wait_fail_blocklist is on: it removes a failed
// on_cache_miss=wait grab from the arr's queue with blocklist=true, which makes
// the arr blocklist that release and search for a replacement. Nothing in this
// package grabs directly.
package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/playback"
)

// Kind is the arr flavour.
const (
	KindRadarr = "radarr"
	KindSonarr = "sonarr"
)

// GrabInfo is what the arr knows about one grab (by download id = infohash).
type GrabInfo struct {
	Kind   string    `json:"kind"`
	Title  string    `json:"title"`
	Year   int       `json:"year"`
	TMDBID int       `json:"tmdb_id,omitempty"`
	TVDBID int       `json:"tvdb_id,omitempty"`
	Added  time.Time `json:"added"`
	// Movies: earliest of digital/physical release (zero if neither is known) and the
	// theatrical date.
	ReleaseDate time.Time `json:"release_date"`
	InCinemas   time.Time `json:"in_cinemas"`
	// Episodes: the latest air date among the grabbed episodes, and the series status.
	AirDate      time.Time `json:"air_date"`
	SeriesStatus string    `json:"series_status"` // continuing | ended | upcoming | deleted
	Season       int       `json:"season"`
	Episodes     []int     `json:"episodes,omitempty"`
}

// Client is one Radarr/Sonarr instance.
type Client struct {
	base string
	key  string
	kind string
	hc   *http.Client
}

// New builds a client for one instance. hc may be nil.
func New(kind, baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = playback.NewHTTPClient(15 * time.Second)
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), key: apiKey, kind: kind, hc: hc}
}

func (c *Client) Kind() string { return c.kind }

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	body, err := c.send(ctx, http.MethodGet, path, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode %s: %w", c.kind, path, err)
	}
	return nil
}

// send runs one request against the arr and returns the 2xx body.
func (c *Client) send(ctx context.Context, method, path string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, playback.JoinURL(c.base, path, q), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.key) // header, never a query string
	req.Header.Set("Accept", "application/json")
	body, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.kind, err)
	}
	return body, nil
}

// ---- wire shapes (subset) ------------------------------------------------------

type historyPage struct {
	Records []historyRecord `json:"records"`
}

type historyRecord struct {
	MovieID    int    `json:"movieId"`
	SeriesID   int    `json:"seriesId"`
	EpisodeID  int    `json:"episodeId"`
	DownloadID string `json:"downloadId"`
	EventType  string `json:"eventType"`
}

type movie struct {
	Title           string `json:"title"`
	Year            int    `json:"year"`
	TmdbID          int    `json:"tmdbId"`
	InCinemas       string `json:"inCinemas"`
	DigitalRelease  string `json:"digitalRelease"`
	PhysicalRelease string `json:"physicalRelease"`
	Added           string `json:"added"`
}

type series struct {
	Title  string `json:"title"`
	Year   int    `json:"year"`
	TvdbID int    `json:"tvdbId"`
	Status string `json:"status"`
	Added  string `json:"added"`
}

type episode struct {
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	AirDateUtc    string `json:"airDateUtc"`
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func earliest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.IsZero() {
			continue
		}
		if out.IsZero() || t.Before(out) {
			out = t
		}
	}
	return out
}

// LookupGrab finds the grab by download id (infohash) in the arr's history and
// returns what the arr knows. Returns (nil, nil) when the arr has no record of it.
func (c *Client) LookupGrab(ctx context.Context, hash string) (*GrabInfo, error) {
	q := url.Values{
		"downloadId": {strings.ToUpper(hash)}, // arrs store the client's hash upper-cased
		"pageSize":   {"50"},
		"page":       {"1"},
	}
	var hp historyPage
	if err := c.get(ctx, "/api/v3/history", q, &hp); err != nil {
		return nil, err
	}
	var rec *historyRecord
	for i := range hp.Records {
		r := &hp.Records[i]
		if !strings.EqualFold(r.DownloadID, hash) {
			continue // defensive: the filter is honoured, but never trust it blindly
		}
		if r.MovieID > 0 || r.SeriesID > 0 {
			if rec == nil {
				rec = r
			}
			if c.kind == KindSonarr && rec != nil && r.EpisodeID > 0 {
				// keep scanning to collect every episode of a multi-episode grab below
				continue
			}
		}
	}
	if rec == nil {
		return nil, nil
	}
	switch c.kind {
	case KindSonarr:
		return c.lookupSonarr(ctx, hp.Records, hash, rec.SeriesID)
	default:
		return c.lookupRadarr(ctx, rec.MovieID)
	}
}

func (c *Client) lookupRadarr(ctx context.Context, movieID int) (*GrabInfo, error) {
	var m movie
	if err := c.get(ctx, "/api/v3/movie/"+strconv.Itoa(movieID), nil, &m); err != nil {
		return nil, err
	}
	return &GrabInfo{
		Kind:        KindRadarr,
		Title:       m.Title,
		Year:        m.Year,
		TMDBID:      m.TmdbID,
		Added:       parseTime(m.Added),
		ReleaseDate: earliest(parseTime(m.DigitalRelease), parseTime(m.PhysicalRelease)),
		InCinemas:   parseTime(m.InCinemas),
	}, nil
}

func (c *Client) lookupSonarr(ctx context.Context, records []historyRecord, hash string, seriesID int) (*GrabInfo, error) {
	var s series
	if err := c.get(ctx, "/api/v3/series/"+strconv.Itoa(seriesID), nil, &s); err != nil {
		return nil, err
	}
	info := &GrabInfo{
		Kind:         KindSonarr,
		Title:        s.Title,
		Year:         s.Year,
		TVDBID:       s.TvdbID,
		Added:        parseTime(s.Added),
		SeriesStatus: s.Status,
	}
	q := url.Values{}
	seen := map[int]bool{}
	for _, r := range records {
		if strings.EqualFold(r.DownloadID, hash) && r.EpisodeID > 0 && !seen[r.EpisodeID] {
			seen[r.EpisodeID] = true
			q.Add("episodeIds", strconv.Itoa(r.EpisodeID))
		}
	}
	if len(q) == 0 {
		return info, nil
	}
	var eps []episode
	if err := c.get(ctx, "/api/v3/episode", q, &eps); err != nil {
		return nil, err
	}
	for _, e := range eps {
		if info.Season == 0 {
			info.Season = e.SeasonNumber
		}
		info.Episodes = append(info.Episodes, e.EpisodeNumber)
		if t := parseTime(e.AirDateUtc); !t.IsZero() && t.After(info.AirDate) {
			info.AirDate = t
		}
	}
	return info, nil
}
