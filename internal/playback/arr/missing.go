package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/your-username/lazarr/internal/playback"
)

// Read side of the Web UI "Missing" tab (internal/missing): what each arr still wants,
// plus the one write it offers, an explicit button-triggered search command.

// wantedPageSize is the page size for /api/v3/wanted/missing. Each Sonarr record
// carries its full series resource (includeSeries), so a page is a few MiB at most.
const wantedPageSize = 500

// wantedMaxPages bounds the scan (500 x 40 = 20000 records) so an arr that misreports
// totalRecords cannot keep us paging forever.
const wantedMaxPages = 40

// MissingEpisode is one monitored, file-less episode from Sonarr's wanted/missing list.
type MissingEpisode struct {
	ID            int
	SeriesID      int
	SeasonNumber  int
	EpisodeNumber int
	AirDateUtc    time.Time // zero when the arr has no air date (TBA)
	Series        MissingSeries
}

// MissingSeries is the series resource embedded in a wanted/missing record.
type MissingSeries struct {
	ID               int
	Title            string
	Year             int
	TVDBID           int
	IMDBID           string
	QualityProfileID int
	// SeasonMonitored maps season number -> monitored (series.seasons[]).
	SeasonMonitored map[int]bool
}

// MissingMovie is one monitored movie without a file. Available reports Radarr's
// isAvailable (past the movie's minimum availability).
type MissingMovie struct {
	ID               int
	Title            string
	Year             int
	TMDBID           int
	IMDBID           string
	QualityProfileID int
	Available        bool
}

type wantedEpisode struct {
	ID            int    `json:"id"`
	SeriesID      int    `json:"seriesId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	AirDateUtc    string `json:"airDateUtc"`
	HasFile       bool   `json:"hasFile"`
	Monitored     bool   `json:"monitored"`
	Series        *struct {
		ID               int    `json:"id"`
		Title            string `json:"title"`
		Year             int    `json:"year"`
		TvdbID           int    `json:"tvdbId"`
		ImdbID           string `json:"imdbId"`
		QualityProfileID int    `json:"qualityProfileId"`
		Seasons          []struct {
			SeasonNumber int  `json:"seasonNumber"`
			Monitored    bool `json:"monitored"`
		} `json:"seasons"`
	} `json:"series"`
}

type wantedMovie struct {
	ID               int    `json:"id"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	TmdbID           int    `json:"tmdbId"`
	ImdbID           string `json:"imdbId"`
	QualityProfileID int    `json:"qualityProfileId"`
	Monitored        bool   `json:"monitored"`
	HasFile          bool   `json:"hasFile"`
	IsAvailable      bool   `json:"isAvailable"`
}

// wantedPages pages GET /api/v3/wanted/missing (monitored only) and hands each page's
// raw records to add. Stops on a short page, at totalRecords, or at wantedMaxPages.
func (c *Client) wantedPages(ctx context.Context, extra url.Values, add func(json.RawMessage) error) error {
	seen := 0
	for page := 1; page <= wantedMaxPages; page++ {
		q := url.Values{
			"monitored": {"true"},
			"page":      {strconv.Itoa(page)},
			"pageSize":  {strconv.Itoa(wantedPageSize)},
		}
		for k, v := range extra {
			q[k] = v
		}
		var wp struct {
			TotalRecords int               `json:"totalRecords"`
			Records      []json.RawMessage `json:"records"`
		}
		if err := c.get(ctx, "/api/v3/wanted/missing", q, &wp); err != nil {
			return err
		}
		for _, r := range wp.Records {
			if err := add(r); err != nil {
				return err
			}
		}
		seen += len(wp.Records)
		if len(wp.Records) < wantedPageSize || (wp.TotalRecords > 0 && seen >= wp.TotalRecords) {
			break
		}
	}
	return nil
}

// MissingEpisodes lists Sonarr's monitored missing episodes with their series
// (GET /api/v3/wanted/missing?monitored=true&includeSeries=true, paged). Air-date
// filtering is the caller's job: the list may hold unaired or TBA episodes.
func (c *Client) MissingEpisodes(ctx context.Context) ([]MissingEpisode, error) {
	var out []MissingEpisode
	err := c.wantedPages(ctx, url.Values{"includeSeries": {"true"}}, func(raw json.RawMessage) error {
		var w wantedEpisode
		if err := json.Unmarshal(raw, &w); err != nil {
			return fmt.Errorf("%s: decode wanted/missing: %w", c.kind, err)
		}
		if w.HasFile || !w.Monitored {
			return nil // defensive: the filter is honoured, but never trust it blindly
		}
		e := MissingEpisode{
			ID:            w.ID,
			SeriesID:      w.SeriesID,
			SeasonNumber:  w.SeasonNumber,
			EpisodeNumber: w.EpisodeNumber,
			AirDateUtc:    parseTime(w.AirDateUtc),
		}
		if s := w.Series; s != nil {
			e.Series = MissingSeries{ID: s.ID, Title: s.Title, Year: s.Year, TVDBID: s.TvdbID, IMDBID: s.ImdbID,
				QualityProfileID: s.QualityProfileID, SeasonMonitored: make(map[int]bool, len(s.Seasons))}
			for _, sn := range s.Seasons {
				e.Series.SeasonMonitored[sn.SeasonNumber] = sn.Monitored
			}
		}
		if e.Series.ID == 0 {
			e.Series.ID = w.SeriesID
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MissingMovies lists Radarr's monitored movies without a file. It uses
// /api/v3/wanted/missing (Radarr v5+, paged) and falls back to filtering the full
// /api/v3/movie list on arrs that do not have it (404). Both include movies not yet
// available; Available carries isAvailable for the caller to filter on.
func (c *Client) MissingMovies(ctx context.Context) ([]MissingMovie, error) {
	var out []MissingMovie
	keep := func(w wantedMovie) {
		if w.Monitored && !w.HasFile {
			out = append(out, MissingMovie{ID: w.ID, Title: w.Title, Year: w.Year, TMDBID: w.TmdbID, IMDBID: w.ImdbID,
				QualityProfileID: w.QualityProfileID, Available: w.IsAvailable})
		}
	}
	err := c.wantedPages(ctx, nil, func(raw json.RawMessage) error {
		var w wantedMovie
		if err := json.Unmarshal(raw, &w); err != nil {
			return fmt.Errorf("%s: decode wanted/missing: %w", c.kind, err)
		}
		keep(w)
		return nil
	})
	var se *playback.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		out = nil
		var all []wantedMovie
		if err := c.get(ctx, "/api/v3/movie", nil, &all); err != nil {
			return nil, err
		}
		for _, w := range all {
			keep(w)
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// QualityProfiles maps quality profile id -> name (GET /api/v3/qualityprofile).
func (c *Client) QualityProfiles(ctx context.Context) (map[int]string, error) {
	var ps []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := c.get(ctx, "/api/v3/qualityprofile", nil, &ps); err != nil {
		return nil, err
	}
	out := make(map[int]string, len(ps))
	for _, p := range ps {
		out[p.ID] = p.Name
	}
	return out, nil
}

// SearchSeason asks Sonarr to search one season (command SeasonSearch). Returns the
// arr's command id.
func (c *Client) SearchSeason(ctx context.Context, seriesID, season int) (int, error) {
	return c.command(ctx, map[string]any{"name": "SeasonSearch", "seriesId": seriesID, "seasonNumber": season})
}

// SearchEpisodes asks Sonarr to search the given episodes (command EpisodeSearch).
func (c *Client) SearchEpisodes(ctx context.Context, episodeIDs []int) (int, error) {
	return c.command(ctx, map[string]any{"name": "EpisodeSearch", "episodeIds": episodeIDs})
}

// SearchMovies asks Radarr to search the given movies (command MoviesSearch).
func (c *Client) SearchMovies(ctx context.Context, movieIDs []int) (int, error) {
	return c.command(ctx, map[string]any{"name": "MoviesSearch", "movieIds": movieIDs})
}

// command POSTs /api/v3/command and returns the created command's id.
func (c *Client) command(ctx context.Context, body map[string]any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, playback.JoinURL(c.base, "/api/v3/command", nil), bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return 0, fmt.Errorf("%s: command %v: %w", c.kind, body["name"], err)
	}
	var cmd struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(resp, &cmd); err != nil {
		return 0, fmt.Errorf("%s: decode command response: %w", c.kind, err)
	}
	return cmd.ID, nil
}
