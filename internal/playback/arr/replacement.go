package arr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"

	"github.com/your-username/lazarr/internal/playback"
)

// ItemState is whether the arr still monitors an episode (Sonarr) / movie (Radarr) and
// whether it has a file for it.
type ItemState struct {
	Monitored bool
	HasFile   bool
}

type fileRecord struct {
	ID   int    `json:"id"`
	Path string `json:"path"`
}

// FileOwners returns the episodes (Sonarr) or the movie (Radarr) whose file record is
// one of arrPaths (paths in the arr's own view) inside series/movie itemID. Used for
// the replacement search (repair.search_replacement): it must run BEFORE the rescan
// that deletes those file records, which is the only time the arr can still say which
// episode a removed library link was.
func (c *Client) FileOwners(ctx context.Context, itemID int, arrPaths []string) ([]int, error) {
	want := make(map[string]bool, len(arrPaths))
	for _, p := range arrPaths {
		want[path.Clean(p)] = true
	}
	if c.kind != KindSonarr {
		var files []fileRecord
		if err := c.get(ctx, "/api/v3/moviefile", url.Values{"movieId": {strconv.Itoa(itemID)}}, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			if want[path.Clean(f.Path)] {
				return []int{itemID}, nil
			}
		}
		return nil, nil
	}
	var files []fileRecord
	if err := c.get(ctx, "/api/v3/episodefile", url.Values{"seriesId": {strconv.Itoa(itemID)}}, &files); err != nil {
		return nil, err
	}
	fileIDs := map[int]bool{}
	for _, f := range files {
		if f.ID > 0 && want[path.Clean(f.Path)] {
			fileIDs[f.ID] = true
		}
	}
	if len(fileIDs) == 0 {
		return nil, nil
	}
	var eps []struct {
		ID            int `json:"id"`
		EpisodeFileID int `json:"episodeFileId"`
	}
	if err := c.get(ctx, "/api/v3/episode", url.Values{"seriesId": {strconv.Itoa(itemID)}}, &eps); err != nil {
		return nil, err
	}
	var out []int
	for _, e := range eps {
		if fileIDs[e.EpisodeFileID] {
			out = append(out, e.ID)
		}
	}
	sort.Ints(out)
	return out, nil
}

// ItemStates reads monitored/hasFile for each episode (Sonarr) or movie (Radarr) id.
// An episode counts as monitored only when its series is monitored too. An id the arr
// no longer knows (404) is left out of the result.
func (c *Client) ItemStates(ctx context.Context, ids []int) (map[int]ItemState, error) {
	base := "/api/v3/movie/"
	if c.kind == KindSonarr {
		base = "/api/v3/episode/"
	}
	seriesMonitored := map[int]bool{}
	out := make(map[int]ItemState, len(ids))
	for _, id := range ids {
		var st struct {
			Monitored bool `json:"monitored"`
			HasFile   bool `json:"hasFile"`
			SeriesID  int  `json:"seriesId"`
		}
		found, err := c.getOptional(ctx, base+strconv.Itoa(id), &st)
		if err != nil {
			return nil, fmt.Errorf("%s: item %d: %w", c.kind, id, err)
		}
		if !found {
			continue
		}
		mon := st.Monitored
		if c.kind == KindSonarr && mon {
			sm, seen := seriesMonitored[st.SeriesID]
			if !seen {
				var s struct {
					Monitored bool `json:"monitored"`
				}
				ok, err := c.getOptional(ctx, "/api/v3/series/"+strconv.Itoa(st.SeriesID), &s)
				if err != nil {
					return nil, fmt.Errorf("%s: series %d: %w", c.kind, st.SeriesID, err)
				}
				sm = ok && s.Monitored
				seriesMonitored[st.SeriesID] = sm
			}
			mon = sm
		}
		out[id] = ItemState{Monitored: mon, HasFile: st.HasFile}
	}
	return out, nil
}

// getOptional is get that reports a 404 as found=false instead of an error.
func (c *Client) getOptional(ctx context.Context, p string, out any) (bool, error) {
	err := c.get(ctx, p, nil, out)
	var se *playback.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// SearchItems asks the arr to search exactly these episodes (EpisodeSearch) or movies
// (MoviesSearch).
func (c *Client) SearchItems(ctx context.Context, ids []int) error {
	var err error
	if c.kind == KindSonarr {
		_, err = c.SearchEpisodes(ctx, ids)
	} else {
		_, err = c.SearchMovies(ctx, ids)
	}
	return err
}
