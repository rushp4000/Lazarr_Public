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

func TestLibraryItemsAndRescan(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/series":
			_, _ = w.Write([]byte(`[{"id":412,"path":"/debrid_symlinks/TV Shows/Charter House","title":"Charter House"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(KindSonarr, srv.URL, "k", nil)
	items, err := c.LibraryItems(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, 412, items[0].ID)
	require.NoError(t, c.Rescan(context.Background(), 412))
	assert.JSONEq(t, `{"name":"RescanSeries","seriesId":412}`, body)
}
