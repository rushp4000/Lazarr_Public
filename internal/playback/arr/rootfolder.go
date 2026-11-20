package arr

import "context"

// RootFolders lists the arr's root folders (GET /api/v3/rootfolder), in the arr's own
// path namespace (its container's mount points, e.g. "/tv").
func (c *Client) RootFolders(ctx context.Context) ([]string, error) {
	var out []struct {
		Path string `json:"path"`
	}
	if err := c.get(ctx, "/api/v3/rootfolder", nil, &out); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(out))
	for _, r := range out {
		if r.Path != "" {
			paths = append(paths, r.Path)
		}
	}
	return paths, nil
}

// CreatesEmptyFolders reports the arr's Media Management "Create empty series folders"
// (Sonarr) / "Create empty movie folders" (Radarr) setting. With it on, the arr's disk
// scan recreates every missing series/movie folder.
func (c *Client) CreatesEmptyFolders(ctx context.Context) (bool, error) {
	var mm struct {
		Series bool `json:"createEmptySeriesFolders"`
		Movie  bool `json:"createEmptyMovieFolders"`
	}
	if err := c.get(ctx, "/api/v3/config/mediamanagement", nil, &mm); err != nil {
		return false, err
	}
	if c.kind == KindSonarr {
		return mm.Series, nil
	}
	return mm.Movie, nil
}
