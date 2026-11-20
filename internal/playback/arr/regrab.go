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
	"strings"

	"github.com/your-username/lazarr/internal/playback"
)

// ErrNoGrab means the arr's history has no grab for the download id: it never grabbed
// that hash (a manual slow-lane add) or its history was cleared.
var ErrNoGrab = errors.New("arr: no grab in history for this download")

// blocklistPageSize bounds the blocklist scan: the newest entries come first and a
// slow-lane hand-off looks for an entry the arr wrote when the wait download failed.
const blocklistPageSize = 250

// RegrabResult is what Regrab did.
type RegrabResult struct {
	SourceTitle      string // the release title the arr grabbed
	BlocklistCleared int    // blocklist entries removed for that release
	Searched         []int  // episode ids (Sonarr) or the movie id (Radarr) searched again
}

type grabRecord struct {
	historyRecord
	SourceTitle string `json:"sourceTitle"`
}

type blocklistPage struct {
	Records []blocklistItem `json:"records"`
}

type blocklistItem struct {
	ID          int    `json:"id"`
	SourceTitle string `json:"sourceTitle"`
	SeriesID    int    `json:"seriesId"`
	MovieID     int    `json:"movieId"`
}

// Regrab gives a download the arr already failed back to it, for the slow lane
// (v1.2.9). Sonarr/Radarr take a download's state from the LATEST history event for its
// download id, so a hash the arr marked failed can never import again by itself. Instead:
//
//  1. history (by download id): the grab's release title and its episodes / movie;
//  2. blocklist: remove the entries for that title on that series/movie (the arr
//     blocklisted it when the wait download failed; the API exposes no hash, so the
//     match is title + series/movie);
//  3. command EpisodeSearch / MoviesSearch for those items.
//
// The arr then grabs whatever it now ranks best. The slow-lane release is cached by
// then, so a re-grab of it completes instantly; if the arr already found another copy,
// the search finds nothing missing and nothing happens. Returns ErrNoGrab when the
// history has no grab for the hash.
func (c *Client) Regrab(ctx context.Context, hash string) (RegrabResult, error) {
	var res RegrabResult
	q := url.Values{
		"downloadId": {strings.ToUpper(hash)},
		"pageSize":   {"100"},
		"page":       {"1"},
	}
	var hp struct {
		Records []grabRecord `json:"records"`
	}
	if err := c.get(ctx, "/api/v3/history", q, &hp); err != nil {
		return res, err
	}
	seriesID, movieID := 0, 0
	seen := map[int]bool{}
	for _, r := range hp.Records {
		if !strings.EqualFold(r.DownloadID, hash) || !strings.EqualFold(r.EventType, "grabbed") {
			continue
		}
		if res.SourceTitle == "" {
			res.SourceTitle = r.SourceTitle
		}
		switch {
		case c.kind == KindSonarr && r.EpisodeID > 0:
			seriesID = r.SeriesID
			if !seen[r.EpisodeID] {
				seen[r.EpisodeID] = true
				res.Searched = append(res.Searched, r.EpisodeID)
			}
		case c.kind != KindSonarr && r.MovieID > 0:
			movieID = r.MovieID
		}
	}
	if c.kind != KindSonarr && movieID > 0 {
		res.Searched = []int{movieID}
	}
	if len(res.Searched) == 0 {
		return res, ErrNoGrab
	}

	if res.SourceTitle != "" {
		n, err := c.clearBlocklist(ctx, res.SourceTitle, seriesID, movieID)
		res.BlocklistCleared = n
		if err != nil {
			return res, err
		}
	}

	body := map[string]any{"name": "MoviesSearch", "movieIds": res.Searched}
	if c.kind == KindSonarr {
		body = map[string]any{"name": "EpisodeSearch", "episodeIds": res.Searched}
	}
	if err := c.postJSON(ctx, "/api/v3/command", body); err != nil {
		return res, fmt.Errorf("%s: search: %w", c.kind, err)
	}
	return res, nil
}

// clearBlocklist deletes the newest blocklist entries whose title matches and that
// belong to the same series (Sonarr) / movie (Radarr). Returns how many were removed.
func (c *Client) clearBlocklist(ctx context.Context, title string, seriesID, movieID int) (int, error) {
	q := url.Values{
		"page":          {"1"},
		"pageSize":      {strconv.Itoa(blocklistPageSize)},
		"sortKey":       {"date"},
		"sortDirection": {"descending"},
	}
	var bp blocklistPage
	if err := c.get(ctx, "/api/v3/blocklist", q, &bp); err != nil {
		return 0, err
	}
	n := 0
	for _, b := range bp.Records {
		if b.ID == 0 || !strings.EqualFold(strings.TrimSpace(b.SourceTitle), strings.TrimSpace(title)) {
			continue
		}
		if (c.kind == KindSonarr && b.SeriesID != seriesID) || (c.kind != KindSonarr && b.MovieID != movieID) {
			continue
		}
		_, err := c.send(ctx, http.MethodDelete, "/api/v3/blocklist/"+strconv.Itoa(b.ID), nil)
		var se *playback.StatusError
		gone := errors.As(err, &se) && se.Code == http.StatusNotFound // removed meanwhile
		if err != nil && !gone {
			return n, fmt.Errorf("%s: delete blocklist %d: %w", c.kind, b.ID, err)
		}
		n++
	}
	return n, nil
}

// postJSON sends a JSON body and discards the 2xx response.
func (c *Client) postJSON(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, playback.JoinURL(c.base, path, nil), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	_, _, err = playback.Do(ctx, c.hc, req)
	return err
}
