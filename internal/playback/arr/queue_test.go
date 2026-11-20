package arr

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQueue serves /api/v3/queue in pages of the requested size from items, and
// records DELETE /api/v3/queue/{id} calls.
type fakeQueue struct {
	mu        sync.Mutex
	items     []queueItem
	pages     []string // page query values seen
	deletes   []string // path?query of each DELETE
	keys      []string // X-Api-Key of every request
	unknown   []string // includeUnknown* param values seen
	deleteErr int      // non-zero: DELETE answers with this status
	listErr   int      // non-zero: GET queue answers with this status
}

func (f *fakeQueue) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, r.Header.Get("X-Api-Key"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
		if f.listErr != 0 {
			w.WriteHeader(f.listErr)
			return
		}
		q := r.URL.Query()
		f.pages = append(f.pages, q.Get("page"))
		f.unknown = append(f.unknown, q.Get("includeUnknownSeriesItems")+q.Get("includeUnknownMovieItems"))
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("pageSize"))
		lo := min((page-1)*size, len(f.items))
		hi := min(lo+size, len(f.items))
		var b strings.Builder
		for i, it := range f.items[lo:hi] {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"downloadId":%q,"title":%q}`, it.ID, it.DownloadID, it.Title)
		}
		fmt.Fprintf(w, `{"page":%d,"pageSize":%d,"totalRecords":%d,"records":[%s]}`, page, size, len(f.items), b.String())
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/queue/"):
		f.deletes = append(f.deletes, r.URL.Path+"?"+r.URL.RawQuery)
		if f.deleteErr != 0 {
			w.WriteHeader(f.deleteErr)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeQueue(t *testing.T, items []queueItem) (*fakeQueue, *httptest.Server) {
	t.Helper()
	f := &fakeQueue{items: items}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, srv
}

func fillerItems(n, startID int) []queueItem {
	out := make([]queueItem, n)
	for i := range out {
		out[i] = queueItem{ID: startID + i, DownloadID: fmt.Sprintf("%040X", startID+i), Title: "other"}
	}
	return out
}

func TestFailDownload_MatchCaseInsensitive(t *testing.T) {
	f, srv := newFakeQueue(t, append(fillerItems(3, 1),
		queueItem{ID: 77, DownloadID: strings.ToUpper(hash), Title: "Show S01E01"},
		queueItem{ID: 78, DownloadID: strings.ToUpper(hash), Title: "Show S01E02"}))
	c := New(KindSonarr, srv.URL, "sk", nil)

	require.NoError(t, c.FailDownload(context.Background(), hash))

	require.Len(t, f.deletes, 1, "one queue item fails the whole download")
	assert.Equal(t, "/api/v3/queue/77?blocklist=true&removeFromClient=true&skipRedownload=false", f.deletes[0])
	assert.Equal(t, []string{"false"}, f.unknown, "sonarr: includeUnknownSeriesItems=false")
	for _, k := range f.keys {
		assert.Equal(t, "sk", k, "api key goes in the header")
	}
}

func TestFindQueueItem_Pagination(t *testing.T) {
	items := append(fillerItems(queuePageSize+5, 1000), queueItem{ID: 9, DownloadID: strings.ToUpper(hash)})
	f, srv := newFakeQueue(t, items)
	c := New(KindRadarr, srv.URL, "rk", nil)

	id, err := c.FindQueueItem(context.Background(), hash)
	require.NoError(t, err)
	assert.Equal(t, 9, id)
	assert.Equal(t, []string{"1", "2"}, f.pages)
	assert.Equal(t, []string{"false", "false"}, f.unknown, "radarr: includeUnknownMovieItems=false")
}

func TestFindQueueItem_NoMatchStopsAtLastPage(t *testing.T) {
	f, srv := newFakeQueue(t, fillerItems(queuePageSize, 1)) // exactly one full page
	c := New(KindRadarr, srv.URL, "rk", nil)

	err := c.FailDownload(context.Background(), hash)
	require.ErrorIs(t, err, ErrNotInQueue)
	assert.Equal(t, []string{"1"}, f.pages, "totalRecords reached: no second page")
	assert.Empty(t, f.deletes)
}

func TestFailDownload_EmptyQueue(t *testing.T) {
	f, srv := newFakeQueue(t, nil)
	c := New(KindRadarr, srv.URL, "rk", nil)
	require.ErrorIs(t, c.FailDownload(context.Background(), hash), ErrNotInQueue)
	assert.Empty(t, f.deletes)
}

func TestFailDownload_Delete404IsNotInQueue(t *testing.T) {
	f, srv := newFakeQueue(t, []queueItem{{ID: 5, DownloadID: strings.ToUpper(hash)}})
	f.deleteErr = http.StatusNotFound
	c := New(KindRadarr, srv.URL, "rk", nil)
	require.ErrorIs(t, c.FailDownload(context.Background(), hash), ErrNotInQueue)
}

func TestFailDownload_Errors(t *testing.T) {
	f, srv := newFakeQueue(t, []queueItem{{ID: 5, DownloadID: strings.ToUpper(hash)}})
	c := New(KindRadarr, srv.URL, "secret-key", nil)

	f.deleteErr = http.StatusInternalServerError
	err := c.FailDownload(context.Background(), hash)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotInQueue)
	assert.Contains(t, err.Error(), "HTTP 500")
	assert.NotContains(t, err.Error(), "secret-key")
	assert.NotContains(t, err.Error(), "blocklist=", "query string is redacted from errors")

	f.listErr = http.StatusUnauthorized
	err = c.FailDownload(context.Background(), hash)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotInQueue)
	assert.Contains(t, err.Error(), "HTTP 401")
}
