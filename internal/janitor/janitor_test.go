package janitor_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/janitor"
	"github.com/your-username/lazarr/internal/playback/arr"
)

const cfHash = "7c1a3e5b9d2f4a6c8e0b1d3f5a7c9e2b4d6f8a0c"

var clients = []string{"Lazarr", "Lazarr Seerr"}

func loadQueue(t *testing.T) []arr.QueueRecord {
	t.Helper()
	b, err := os.ReadFile("../../testdata/arr/sonarr_queue_charter_house_s02_partial.json")
	require.NoError(t, err)
	var page struct {
		Records []arr.QueueRecord `json:"records"`
	}
	require.NoError(t, json.Unmarshal(b, &page))
	return page.Records
}

// Only the Charter House S02 partial pack qualifies: one candidate (not 22, one per
// episode record), lowest queue id, 17 rejected files.
func TestCandidates_CharterHouseS02(t *testing.T) {
	cands := janitor.Candidates(loadQueue(t), clients)
	require.Len(t, cands, 1)
	assert.Equal(t, cfHash, cands[0].Hash)
	assert.Equal(t, 880101, cands[0].QueueID)
	assert.Equal(t, 17, cands[0].Rejections)
}

func TestCandidates_RulesOut(t *testing.T) {
	recs := loadQueue(t)
	// A different client name list: nothing qualifies.
	assert.Empty(t, janitor.Candidates(recs, []string{"OtherClient2"}))

	// One episode record of the pack no longer importBlocked: the download is not
	// uniformly blocked, leave it.
	recs2 := append([]arr.QueueRecord(nil), recs...)
	recs2[3].TrackedDownloadState = "importing"
	assert.Empty(t, janitor.Candidates(recs2, clients))

	// A blocked item with only the header message (no per-file rejection) is unknown.
	hdr := []arr.QueueRecord{{ID: 1, DownloadID: "ab", DownloadClient: "Lazarr", TrackedDownloadState: "importBlocked",
		StatusMessages: []arr.StatusMessage{{Title: "One or more episodes expected", Messages: nil}}}}
	assert.Empty(t, janitor.Candidates(hdr, clients))
}

// fakeArr serves the recorded queue + history through a real *arr.Client.
func fakeArr(t *testing.T, imported bool) (*arr.Client, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var deletes []string
	q, err := os.ReadFile("../../testdata/arr/sonarr_queue_charter_house_s02_partial.json")
	require.NoError(t, err)
	h, err := os.ReadFile("../../testdata/arr/sonarr_history_charter_house_s02.json")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/queue":
			_, _ = w.Write(q)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/history":
			if imported && strings.EqualFold(r.URL.Query().Get("downloadId"), cfHash) {
				_, _ = w.Write(h)
				return
			}
			_, _ = w.Write([]byte(`{"records":[]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/queue/"):
			mu.Lock()
			deletes = append(deletes, r.URL.Path+"?"+r.URL.RawQuery)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return arr.New(arr.KindSonarr, srv.URL, "k", nil), &deletes
}

// The pack is removed exactly once with blocklist=false&skipRedownload=true.
func TestRunOnce_RemovesWithoutBlocklist(t *testing.T) {
	c, deletes := fakeArr(t, true)
	j := &janitor.Janitor{Arrs: map[string]janitor.Arr{"sonarr_main": c}, Clients: clients,
		Known: func(h string) bool { return h == cfHash }}
	res := j.RunOnce(context.Background())
	require.Equal(t, []string{"sonarr_main:" + cfHash}, res.Removed)
	require.Len(t, *deletes, 1)
	d := (*deletes)[0]
	assert.True(t, strings.HasPrefix(d, "/api/v3/queue/880101?"), d)
	assert.Contains(t, d, "blocklist=false")
	assert.Contains(t, d, "removeFromClient=true")
	assert.Contains(t, d, "skipRedownload=true")
	assert.NotContains(t, d, "bulk")
}

// Nothing imported from it (history) or not a Lazarr catalog release: left alone.
func TestRunOnce_RequiresImportAndCatalog(t *testing.T) {
	c, deletes := fakeArr(t, false)
	j := &janitor.Janitor{Arrs: map[string]janitor.Arr{"sonarr_main": c}, Clients: clients}
	assert.Empty(t, j.RunOnce(context.Background()).Removed)
	assert.Empty(t, *deletes)

	c2, deletes2 := fakeArr(t, true)
	j2 := &janitor.Janitor{Arrs: map[string]janitor.Arr{"sonarr_main": c2}, Clients: clients,
		Known: func(string) bool { return false }}
	assert.Empty(t, j2.RunOnce(context.Background()).Removed)
	assert.Empty(t, *deletes2)
}

func pendingRecs(state string) []arr.QueueRecord {
	msg := []arr.StatusMessage{{Title: "Show.S01E02.mkv", Messages: []string{"Not an upgrade for existing episode file(s). Existing quality: WEBDL-1080p. New Quality HDTV-1080p."}}}
	rev := []arr.StatusMessage{{Title: "Show.S01E03.mkv", Messages: []string{"Not a quality revision upgrade for existing episode file(s)"}}}
	return []arr.QueueRecord{
		{ID: 7, DownloadID: cfHash, DownloadClient: "Lazarr", Title: "Show.S01.1080p", TrackedDownloadState: state, StatusMessages: msg},
		{ID: 8, DownloadID: cfHash, DownloadClient: "Lazarr", Title: "Show.S01.1080p", TrackedDownloadState: "importBlocked", StatusMessages: rev},
	}
}

// importPending leftovers qualify (flagged Pending); "importing" still does not.
func TestCandidates_ImportPending(t *testing.T) {
	cands := janitor.Candidates(pendingRecs("importPending"), clients)
	require.Len(t, cands, 1)
	assert.True(t, cands[0].Pending)
	assert.Equal(t, 7, cands[0].QueueID)
	assert.Equal(t, 2, cands[0].Rejections)

	blocked := janitor.Candidates(pendingRecs("importBlocked"), clients)
	require.Len(t, blocked, 1)
	assert.False(t, blocked[0].Pending)

	assert.Empty(t, janitor.Candidates(pendingRecs("importing"), clients))
}

type stubArr struct {
	recs    []arr.QueueRecord
	removed []int
}

func (s *stubArr) Queue(context.Context) ([]arr.QueueRecord, error)           { return s.recs, nil }
func (s *stubArr) ImportedFromDownload(context.Context, string) (bool, error) { return true, nil }
func (s *stubArr) RemoveQueueItemKeep(_ context.Context, id int) error {
	s.removed = append(s.removed, id)
	return nil
}

// An importPending candidate is removed only on its second consecutive sighting; a pass
// without it resets the count.
func TestRunOnce_ImportPendingNeedsTwoPasses(t *testing.T) {
	a := &stubArr{recs: pendingRecs("importPending")}
	j := &janitor.Janitor{Arrs: map[string]janitor.Arr{"sonarr_main": a}, Clients: clients}
	ctx := context.Background()

	assert.Empty(t, j.RunOnce(ctx).Removed, "first sighting waits")
	assert.Empty(t, a.removed)

	saved := a.recs
	a.recs = nil
	j.RunOnce(ctx) // gone for a pass
	a.recs = saved
	assert.Empty(t, j.RunOnce(ctx).Removed, "count restarts after a pass without it")

	assert.Equal(t, []string{"sonarr_main:" + cfHash}, j.RunOnce(ctx).Removed)
	assert.Equal(t, []int{7}, a.removed)
}

// importBlocked keeps the old behaviour: removed on the first pass.
func TestRunOnce_ImportBlockedFirstPass(t *testing.T) {
	a := &stubArr{recs: pendingRecs("importBlocked")}
	j := &janitor.Janitor{Arrs: map[string]janitor.Arr{"sonarr_main": a}, Clients: clients}
	assert.Equal(t, []string{"sonarr_main:" + cfHash}, j.RunOnce(context.Background()).Removed)
}
