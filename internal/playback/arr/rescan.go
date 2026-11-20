package arr

import (
	"context"
	"fmt"
)

// LibraryItem is a series (Sonarr) or movie (Radarr) and its folder.
type LibraryItem struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
}

// LibraryItems lists every series / movie with its folder (GET /api/v3/series or
// /api/v3/movie). Used to map a library file path back to the item that owns it.
func (c *Client) LibraryItems(ctx context.Context) ([]LibraryItem, error) {
	path := "/api/v3/movie"
	if c.kind == KindSonarr {
		path = "/api/v3/series"
	}
	var out []LibraryItem
	if err := c.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Rescan asks the arr to rescan one series / movie folder from disk
// (POST /api/v3/command {"name":"RescanSeries","seriesId":N} or RescanMovie/movieId).
// A file whose library symlink was removed then becomes missing, and the arr's missing
// search can grab it again.
func (c *Client) Rescan(ctx context.Context, id int) error {
	body := map[string]any{"name": "RescanMovie", "movieId": id}
	if c.kind == KindSonarr {
		body = map[string]any{"name": "RescanSeries", "seriesId": id}
	}
	if err := c.postJSON(ctx, "/api/v3/command", body); err != nil {
		return fmt.Errorf("%s: rescan %d: %w", c.kind, id, err)
	}
	return nil
}
