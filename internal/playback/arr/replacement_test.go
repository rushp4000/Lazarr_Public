package arr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func replacementServer(t *testing.T, routes map[string]string, commands *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v3/command" {
			b, _ := io.ReadAll(r.Body)
			*commands = append(*commands, string(b))
			_, _ = io.WriteString(w, `{"id":7}`)
			return
		}
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		body, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFileOwners(t *testing.T) {
	routes := map[string]string{
		"/api/v3/episodefile?seriesId=412": `[
			{"id":51,"path":"/tv/Charter House/Season 04/S04E07.mkv"},
			{"id":52,"path":"/tv/Charter House/Season 04/S04E08.mkv"},
			{"id":53,"path":"/tv/Charter House/Season 04/S04E09.mkv"}]`,
		"/api/v3/episode?seriesId=412": `[
			{"id":9007,"episodeFileId":51},{"id":9008,"episodeFileId":52},
			{"id":9009,"episodeFileId":53},{"id":9010,"episodeFileId":0}]`,
		"/api/v3/moviefile?movieId=5": `[{"id":1,"path":"/movies/Lexa (1994)/Lexa.mkv"}]`,
	}
	var cmds []string
	srv := replacementServer(t, routes, &cmds)
	ctx := context.Background()

	tests := []struct {
		name  string
		kind  string
		item  int
		paths []string
		want  []int
	}{
		{"sonarr: the removed files' episodes", KindSonarr, 412,
			[]string{"/tv/Charter House/Season 04/S04E08.mkv", "/tv/Charter House/Season 04//S04E07.mkv"}, []int{9007, 9008}},
		{"sonarr: no file record matches", KindSonarr, 412, []string{"/tv/Charter House/other.mkv"}, nil},
		{"radarr: the movie when its file matches", KindRadarr, 5, []string{"/movies/Lexa (1994)/Lexa.mkv"}, []int{5}},
		{"radarr: nothing when another file was removed", KindRadarr, 5, []string{"/movies/Lexa (1994)/old.mkv"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New(tc.kind, srv.URL, "k", nil).FileOwners(ctx, tc.item, tc.paths)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestItemStatesAndSearch(t *testing.T) {
	routes := map[string]string{
		"/api/v3/episode/1": `{"id":1,"seriesId":10,"monitored":true,"hasFile":false}`,
		"/api/v3/episode/2": `{"id":2,"seriesId":10,"monitored":false,"hasFile":false}`,
		"/api/v3/episode/4": `{"id":4,"seriesId":11,"monitored":true,"hasFile":false}`,
		"/api/v3/series/10": `{"id":10,"monitored":true}`,
		"/api/v3/series/11": `{"id":11,"monitored":false}`,
		"/api/v3/movie/5":   `{"id":5,"monitored":true,"hasFile":true}`,
	}
	var cmds []string
	srv := replacementServer(t, routes, &cmds)
	ctx := context.Background()

	son := New(KindSonarr, srv.URL, "k", nil)
	st, err := son.ItemStates(ctx, []int{1, 2, 3, 4})
	require.NoError(t, err)
	assert.Equal(t, map[int]ItemState{1: {Monitored: true}, 2: {}, 4: {}}, st,
		"a 404 id is left out; an episode of an unmonitored series is unmonitored")

	rad := New(KindRadarr, srv.URL, "k", nil)
	st, err = rad.ItemStates(ctx, []int{5})
	require.NoError(t, err)
	assert.Equal(t, map[int]ItemState{5: {Monitored: true, HasFile: true}}, st)

	require.NoError(t, son.SearchItems(ctx, []int{1}))
	require.NoError(t, rad.SearchItems(ctx, []int{5}))
	assert.JSONEq(t, `{"name":"EpisodeSearch","episodeIds":[1]}`, cmds[0])
	assert.JSONEq(t, `{"name":"MoviesSearch","movieIds":[5]}`, cmds[1])
}
