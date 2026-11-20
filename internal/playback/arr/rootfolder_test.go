package arr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRootFoldersAndCreatesEmptyFolders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "k", r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/v3/rootfolder":
			_, _ = w.Write([]byte(`[{"id":1,"path":"/tv"},{"id":2,"path":""}]`))
		case "/api/v3/config/mediamanagement":
			_, _ = w.Write([]byte(`{"createEmptySeriesFolders":true,"createEmptyMovieFolders":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	son := New(KindSonarr, srv.URL, "k", nil)
	roots, err := son.RootFolders(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"/tv"}, roots)
	on, err := son.CreatesEmptyFolders(ctx)
	require.NoError(t, err)
	assert.True(t, on, "sonarr reads createEmptySeriesFolders")

	on, err = New(KindRadarr, srv.URL, "k", nil).CreatesEmptyFolders(ctx)
	require.NoError(t, err)
	assert.False(t, on, "radarr reads createEmptyMovieFolders")
}
