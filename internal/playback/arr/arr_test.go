package arr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hash = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

func radarrServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery+"|key="+r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history":
			_, _ = w.Write([]byte(`{"page":1,"records":[
			  {"movieId":0,"downloadId":"OTHER","eventType":"grabbed"},
			  {"movieId":42,"downloadId":"5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A","eventType":"grabbed"}]}`))
		case "/api/v3/movie/42":
			_, _ = w.Write([]byte(`{"title":"Sintel","year":2010,"tmdbId":9004,"inCinemas":"2010-09-27T00:00:00Z",
			  "digitalRelease":"2011-02-01T00:00:00Z","physicalRelease":"2010-12-15T00:00:00Z","added":"2026-09-01T10:00:00Z"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestRadarrLookup(t *testing.T) {
	srv, seen := radarrServer(t)
	c := New(KindRadarr, srv.URL, "rk", nil)
	info, err := c.LookupGrab(context.Background(), hash)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, KindRadarr, info.Kind)
	assert.Equal(t, "Sintel", info.Title)
	assert.Equal(t, 2010, info.Year)
	assert.Equal(t, 9004, info.TMDBID)
	assert.Equal(t, "2010-12-15", info.ReleaseDate.Format("2006-01-02"), "earliest of digital/physical")
	assert.Equal(t, "2010-09-27", info.InCinemas.Format("2006-01-02"))
	assert.Contains(t, (*seen)[0], "downloadId=5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A")
	assert.Contains(t, (*seen)[0], "key=rk")
}

func TestSonarrLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history":
			_, _ = w.Write([]byte(`{"records":[
			  {"seriesId":7,"episodeId":101,"downloadId":"5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A","eventType":"grabbed"},
			  {"seriesId":7,"episodeId":102,"downloadId":"5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A5A","eventType":"grabbed"},
			  {"seriesId":7,"episodeId":999,"downloadId":"ZZZ","eventType":"grabbed"}]}`))
		case "/api/v3/series/7":
			_, _ = w.Write([]byte(`{"title":"Hard Lessons","year":2001,"tvdbId":75,"status":"continuing","added":"2026-01-01T00:00:00Z"}`))
		case "/api/v3/episode":
			assert.Equal(t, []string{"101", "102"}, r.URL.Query()["episodeIds"])
			_, _ = w.Write([]byte(`[{"seasonNumber":21,"episodeNumber":1,"airDateUtc":"2026-08-05T01:00:00Z"},
			  {"seasonNumber":21,"episodeNumber":2,"airDateUtc":"2026-08-12T01:00:00Z"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(KindSonarr, srv.URL, "sk", nil)
	info, err := c.LookupGrab(context.Background(), hash)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "Hard Lessons", info.Title)
	assert.Equal(t, "continuing", info.SeriesStatus)
	assert.Equal(t, 21, info.Season)
	assert.Equal(t, []int{1, 2}, info.Episodes)
	assert.Equal(t, "2026-08-12", info.AirDate.Format("2006-01-02"), "latest air date wins")
}

func TestUnknownGrab(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records":[]}`))
	}))
	defer srv.Close()
	c := New(KindRadarr, srv.URL, "k", nil)
	info, err := c.LookupGrab(context.Background(), hash)
	require.NoError(t, err)
	assert.Nil(t, info)
}
