package arr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wantedEpisodeJSON renders one Sonarr wanted/missing record with its series embedded
// (trimmed to the fields Lazarr reads, plus a few it must ignore).
func wantedEpisodeJSON(id, seriesID, season, ep int, aired string) string {
	return fmt.Sprintf(`{"id":%d,"seriesId":%d,"tvdbId":999,"seasonNumber":%d,"episodeNumber":%d,
	  "airDateUtc":%q,"hasFile":false,"monitored":true,"overview":"x",
	  "series":{"id":%d,"title":"Show %d","year":2004,"tvdbId":%d,"imdbId":"tt%d","qualityProfileId":13,
	    "images":[{"coverType":"poster"}],"seasons":[{"seasonNumber":0,"monitored":false},{"seasonNumber":%d,"monitored":true}]}}`,
		id, seriesID, season, ep, aired, seriesID, seriesID, 70000+seriesID, seriesID, season)
}

func TestMissingEpisodes_PaginatesAndDecodes(t *testing.T) {
	const total = wantedPageSize + 2 // forces a second page
	var mu sync.Mutex
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/wanted/missing", r.URL.Path)
		assert.Equal(t, "sk", r.Header.Get("X-Api-Key"))
		q := r.URL.Query()
		assert.Equal(t, "true", q.Get("monitored"))
		assert.Equal(t, "true", q.Get("includeSeries"))
		assert.Equal(t, strconv.Itoa(wantedPageSize), q.Get("pageSize"))
		page, _ := strconv.Atoi(q.Get("page"))
		mu.Lock()
		pages = append(pages, q.Get("page"))
		mu.Unlock()
		var recs []string
		for i := (page - 1) * wantedPageSize; i < total && i < page*wantedPageSize; i++ {
			recs = append(recs, wantedEpisodeJSON(1000+i, 7, 1, i+1, "2020-01-02T03:00:00Z"))
		}
		_, _ = fmt.Fprintf(w, `{"page":%d,"pageSize":%d,"totalRecords":%d,"records":[%s]}`, page, wantedPageSize, total, strings.Join(recs, ","))
	}))
	defer srv.Close()

	eps, err := New(KindSonarr, srv.URL, "sk", nil).MissingEpisodes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"1", "2"}, pages, "stops at totalRecords")
	require.Len(t, eps, total)
	e := eps[0]
	assert.Equal(t, 1000, e.ID)
	assert.Equal(t, 7, e.SeriesID)
	assert.Equal(t, 1, e.SeasonNumber)
	assert.Equal(t, 1, e.EpisodeNumber)
	assert.Equal(t, "2020-01-02", e.AirDateUtc.Format("2006-01-02"))
	assert.Equal(t, "Show 7", e.Series.Title)
	assert.Equal(t, 70007, e.Series.TVDBID, "series tvdbId, not the episode's")
	assert.Equal(t, "tt7", e.Series.IMDBID)
	assert.Equal(t, 13, e.Series.QualityProfileID)
	assert.True(t, e.Series.SeasonMonitored[1])
	assert.False(t, e.Series.SeasonMonitored[0])
}

func TestMissingEpisodes_SkipsFiledAndUnmonitored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"totalRecords":3,"records":[
		  {"id":1,"seriesId":2,"seasonNumber":1,"episodeNumber":1,"hasFile":true,"monitored":true},
		  {"id":2,"seriesId":2,"seasonNumber":1,"episodeNumber":2,"hasFile":false,"monitored":false},
		  {"id":3,"seriesId":2,"seasonNumber":1,"episodeNumber":3,"hasFile":false,"monitored":true}]}`))
	}))
	defer srv.Close()
	eps, err := New(KindSonarr, srv.URL, "k", nil).MissingEpisodes(context.Background())
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, 3, eps[0].ID)
	assert.Equal(t, 2, eps[0].Series.ID, "series id falls back to seriesId without an embedded series")
	assert.True(t, eps[0].AirDateUtc.IsZero())
}

func TestMissingMovies_WantedMissing(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		assert.Empty(t, r.URL.Query().Get("includeSeries"))
		_, _ = w.Write([]byte(`{"totalRecords":2,"records":[
		  {"id":201,"title":"Spiral Arrow","year":2001,"tmdbId":9001,"imdbId":"tt9000001","qualityProfileId":11,"monitored":true,"hasFile":false,"isAvailable":true},
		  {"id":178,"title":"Vesper","year":2026,"tmdbId":9003,"qualityProfileId":11,"monitored":true,"hasFile":false,"isAvailable":false}]}`))
	}))
	defer srv.Close()
	ms, err := New(KindRadarr, srv.URL, "k", nil).MissingMovies(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"/api/v3/wanted/missing"}, paths)
	require.Len(t, ms, 2)
	assert.Equal(t, MissingMovie{ID: 201, Title: "Spiral Arrow", Year: 2001, TMDBID: 9001, IMDBID: "tt9000001", QualityProfileID: 11, Available: true}, ms[0])
	assert.False(t, ms[1].Available)
}

func TestMissingMovies_FallsBackToMovieList(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[
			  {"id":1,"title":"Has File","monitored":true,"hasFile":true,"isAvailable":true},
			  {"id":2,"title":"Unmonitored","monitored":false,"hasFile":false,"isAvailable":true},
			  {"id":3,"title":"Wanted","year":2004,"monitored":true,"hasFile":false,"isAvailable":true}]`))
		default:
			w.WriteHeader(http.StatusNotFound) // an older Radarr without wanted/missing
		}
	}))
	defer srv.Close()
	ms, err := New(KindRadarr, srv.URL, "k", nil).MissingMovies(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"/api/v3/wanted/missing", "/api/v3/movie"}, paths)
	require.Len(t, ms, 1)
	assert.Equal(t, "Wanted", ms[0].Title)
}

func TestMissingMovies_OtherErrorsAreReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(KindRadarr, srv.URL, "k", nil).MissingMovies(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestQualityProfiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v3/qualityprofile", r.URL.Path)
		_, _ = w.Write([]byte(`[{"id":9,"name":"1080p Remux","items":[]},{"id":13,"name":"1080p Classic SD"}]`))
	}))
	defer srv.Close()
	ps, err := New(KindSonarr, srv.URL, "k", nil).QualityProfiles(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[int]string{9: "1080p Remux", 13: "1080p Classic SD"}, ps)
}

func TestQueue_CarriesItemIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"totalRecords":1,"records":[{"id":5,"seriesId":100,"episodeId":8212,"seasonNumber":2,"downloadId":null,"status":"delay"}]}`))
	}))
	defer srv.Close()
	recs, err := New(KindSonarr, srv.URL, "k", nil).Queue(context.Background())
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, 100, recs[0].SeriesID)
	assert.Equal(t, 8212, recs[0].EpisodeID)
	assert.Equal(t, 2, recs[0].SeasonNumber)
}

func TestSearchCommands(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v3/command", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		got = append(got, m)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":%d,"name":%q,"status":"queued"}`, 4000+len(got), m["name"])
	}))
	defer srv.Close()
	ctx := context.Background()
	sc := New(KindSonarr, srv.URL, "k", nil)

	id, err := sc.SearchSeason(ctx, 75, 0)
	require.NoError(t, err)
	assert.Equal(t, 4001, id)
	id, err = sc.SearchEpisodes(ctx, []int{8647, 8648})
	require.NoError(t, err)
	assert.Equal(t, 4002, id)
	id, err = New(KindRadarr, srv.URL, "k", nil).SearchMovies(ctx, []int{201})
	require.NoError(t, err)
	assert.Equal(t, 4003, id)

	assert.Equal(t, []map[string]any{
		{"name": "SeasonSearch", "seriesId": float64(75), "seasonNumber": float64(0)},
		{"name": "EpisodeSearch", "episodeIds": []any{float64(8647), float64(8648)}},
		{"name": "MoviesSearch", "movieIds": []any{float64(201)}},
	}, got)
}

func TestSearchCommand_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := New(KindSonarr, srv.URL, "k", nil).SearchSeason(context.Background(), 1, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SeasonSearch")
}
