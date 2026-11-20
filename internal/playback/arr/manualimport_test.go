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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const miHash = "6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b"

// fakeEp is one episode of the fake Sonarr library.
type fakeEp struct {
	ID, Season, Episode int
	HasFile, Monitored  bool
}

// fakeImportArr is a Sonarr/Radarr serving manualimport, episode/movie state and the
// command API. A ManualImport command gives every listed episode / movie a file.
type fakeImportArr struct {
	mu          sync.Mutex
	kind        string
	items       []map[string]any // the manualimport response
	eps         map[int]*fakeEp  // Sonarr
	movieHas    map[int]bool     // Radarr
	unmonitored map[int]bool     // Radarr
	listQueries []string
	commands    []map[string]any
	cmdPolls    int
	failImport  bool // the command completes but imports nothing
}

func (f *fakeImportArr) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/v3/manualimport" && r.Method == http.MethodGet:
		f.listQueries = append(f.listQueries, r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(f.items)
	case r.URL.Path == "/api/v3/episode":
		var out []map[string]any
		for _, e := range f.eps {
			out = append(out, map[string]any{"id": e.ID, "seasonNumber": e.Season, "episodeNumber": e.Episode,
				"hasFile": e.HasFile, "monitored": e.Monitored})
		}
		_ = json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/movie/"))
		_ = json.NewEncoder(w).Encode(map[string]any{"title": "Sintel", "hasFile": f.movieHas[id], "monitored": !f.unmonitored[id]})
	case r.URL.Path == "/api/v3/command" && r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		var cmd map[string]any
		_ = json.Unmarshal(b, &cmd)
		f.commands = append(f.commands, cmd)
		if !f.failImport {
			files, _ := cmd["files"].([]any)
			for _, fl := range files {
				m := fl.(map[string]any)
				if ids, ok := m["episodeIds"].([]any); ok {
					for _, id := range ids {
						f.eps[int(id.(float64))].HasFile = true
					}
				}
				if mid, ok := m["movieId"].(float64); ok {
					f.movieHas[int(mid)] = true
				}
			}
		}
		_, _ = w.Write([]byte(`{"id":7,"name":"ManualImport","status":"queued"}`))
	case r.URL.Path == "/api/v3/command/7":
		f.cmdPolls++
		_, _ = w.Write([]byte(`{"id":7,"status":"completed","message":"Completed"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeImportArr(t *testing.T, kind string) (*fakeImportArr, *Client) {
	t.Helper()
	commandPollInterval = time.Millisecond
	f := &fakeImportArr{kind: kind, eps: map[int]*fakeEp{}, movieHas: map[int]bool{}, unmonitored: map[int]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return f, New(kind, srv.URL, "k", nil)
}

// lib adds episodes 1..n of a season to the fake library (ids season*100+ep).
func (f *fakeImportArr) lib(season, n int, hasFile bool) {
	for e := 1; e <= n; e++ {
		id := season*100 + e
		f.eps[id] = &fakeEp{ID: id, Season: season, Episode: e, HasFile: hasFile, Monitored: true}
	}
}

// file is a manualimport item for one episode file of series sid.
func epFile(sid, season, ep int, rejections ...string) map[string]any {
	rej := []map[string]any{}
	for _, r := range rejections {
		rej = append(rej, map[string]any{"reason": r, "type": "permanent"})
	}
	return map[string]any{
		"path":         fmt.Sprintf("/dl/sonarr_main/Pack/S%02d/ep%02d.mkv", season, ep),
		"folderName":   fmt.Sprintf("S%02d", season),
		"series":       map[string]any{"id": sid, "title": fmt.Sprintf("Series %d", sid)},
		"seasonNumber": season,
		"episodes":     []map[string]any{{"id": season*100 + ep, "seasonNumber": season, "episodeNumber": ep}},
		"quality":      map[string]any{"quality": map[string]any{"id": 2, "name": "DVD"}, "revision": map[string]any{"version": 1}},
		"languages":    []map[string]any{{"id": 1, "name": "English"}},
		"releaseGroup": "GRP",
		"indexerFlags": 0,
		"releaseType":  "singleEpisode",
		"rejections":   rej,
	}
}

func TestImportMissing_MultiSeasonPackImported(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 2, false)
	f.lib(2, 2, false)
	f.items = []map[string]any{epFile(91, 1, 1), epFile(91, 1, 2), epFile(91, 2, 1), epFile(91, 2, 2)}

	res, err := c.ImportMissing(context.Background(), "/dl/sonarr_main/Lindy Mercer S01-S02", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Equal(t, 4, res.Candidates)
	assert.Equal(t, 4, res.Selected)
	assert.Equal(t, 4, res.Imported)
	assert.Equal(t, 91, res.SeriesID)
	assert.Equal(t, "completed", res.Command)
	assert.Equal(t, "4 file(s) S01E01–E02, S02E01–E02", res.Summary())

	require.Len(t, f.listQueries, 1)
	assert.Contains(t, f.listQueries[0], "folder=%2Fdl%2Fsonarr_main%2FLindy+Mercer+S01-S02")
	assert.Contains(t, f.listQueries[0], "filterExistingFiles=true")
	assert.NotContains(t, f.listQueries[0], "seriesId", "seriesId makes Sonarr list the library folder instead")

	require.Len(t, f.commands, 1)
	cmd := f.commands[0]
	assert.Equal(t, "ManualImport", cmd["name"])
	assert.Equal(t, "move", cmd["importMode"])
	files := cmd["files"].([]any)
	require.Len(t, files, 4)
	first := files[0].(map[string]any)
	assert.Equal(t, strings.ToUpper(miHash), first["downloadId"])
	assert.EqualValues(t, 91, first["seriesId"])
	assert.Equal(t, []any{float64(101)}, first["episodeIds"])
	assert.Equal(t, "GRP", first["releaseGroup"])
	assert.Equal(t, "singleEpisode", first["releaseType"])
	assert.NotNil(t, first["quality"])
	assert.NotNil(t, first["languages"])

	// A re-run is idempotent: everything has a file now, nothing is sent.
	res, err = c.ImportMissing(context.Background(), "/dl/sonarr_main/Lindy Mercer S01-S02", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Zero(t, res.Selected)
	assert.Equal(t, 4, res.Skipped["has file"])
	assert.Len(t, f.commands, 1)
}

func TestImportMissing_MixedSeriesWithoutTargetSkipped(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 2, false)
	f.items = []map[string]any{epFile(91, 1, 1), epFile(92, 1, 2)}

	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.True(t, res.Ambiguous)
	assert.Zero(t, res.Selected)
	assert.Empty(t, f.commands)
}

func TestImportMissing_RejectedFileStillCountsForAmbiguity(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 2, false)
	f.items = []map[string]any{epFile(91, 1, 1), epFile(92, 1, 2, "Unknown Series")}
	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.True(t, res.Ambiguous)
	assert.Empty(t, f.commands)
}

func TestImportMissing_TargetFiltersSeriesAndSeason(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 2, false)
	f.lib(2, 2, false)
	f.items = []map[string]any{epFile(91, 1, 1), epFile(91, 2, 1), epFile(91, 2, 2), epFile(92, 2, 1)}

	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{SeriesID: 91, Season: 2})
	require.NoError(t, err)
	assert.False(t, res.Ambiguous, "a target resolves mixed folders")
	assert.Equal(t, 2, res.Selected)
	assert.Equal(t, 2, res.Imported)
	assert.Equal(t, 1, res.Skipped["other season"])
	assert.Equal(t, 1, res.Skipped["other series"])
	assert.Equal(t, "2 file(s) S02E01–E02", res.Summary())
	assert.False(t, f.eps[101].HasFile, "season 1 untouched")
}

func TestImportMissing_ExistingAndUnmonitoredSkipped(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 3, false)
	f.eps[101].HasFile = true
	f.eps[102].Monitored = false
	f.items = []map[string]any{epFile(91, 1, 1), epFile(91, 1, 2), epFile(91, 1, 3)}

	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Selected)
	assert.Equal(t, 1, res.Skipped["has file"])
	assert.Equal(t, 1, res.Skipped["unmonitored"])
	require.Len(t, f.commands, 1)
	files := f.commands[0]["files"].([]any)
	require.Len(t, files, 1)
	assert.Equal(t, []any{float64(103)}, files[0].(map[string]any)["episodeIds"], "never replaces an existing file")
}

func TestImportMissing_RejectionsAndDuplicatesSkipped(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 3, false)
	noEps := epFile(91, 1, 3)
	noEps["episodes"] = []any{}
	f.items = []map[string]any{
		epFile(91, 1, 1, "Sample"),
		epFile(91, 1, 2), epFile(91, 1, 2), // two files for one episode
		noEps,
	}
	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Zero(t, res.Selected)
	assert.Equal(t, 1, res.Skipped["rejected"])
	assert.Equal(t, 2, res.Skipped["duplicate"])
	assert.Equal(t, 1, res.Skipped["unmatched"])
	assert.Empty(t, f.commands)
	assert.Equal(t, "2 duplicate, 1 rejected, 1 unmatched", res.SkipSummary())
}

func TestImportMissing_CommandImportsNothing(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 1, false)
	f.failImport = true
	f.items = []map[string]any{epFile(91, 1, 1)}
	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Selected)
	assert.Zero(t, res.Imported, "verification reads the episode state, not the command status")
	assert.Equal(t, "completed", res.Command)
}

func TestImportMissing_EmptyFolderAndMismatch(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Zero(t, res.Candidates)

	res, err = c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{MovieID: 5})
	require.NoError(t, err)
	assert.True(t, res.Mismatch)
	assert.Len(t, f.listQueries, 1, "a mismatched target never asks the arr")
}

func movieFile(mid int, path string, rejections ...string) map[string]any {
	rej := []map[string]any{}
	for _, r := range rejections {
		rej = append(rej, map[string]any{"reason": r, "type": "permanent"})
	}
	return map[string]any{
		"path": path, "folderName": "Sintel", "movie": map[string]any{"id": mid, "title": "Sintel"},
		"quality":   map[string]any{"quality": map[string]any{"id": 7, "name": "Bluray-1080p"}},
		"languages": []map[string]any{{"id": 1, "name": "English"}}, "releaseGroup": "GRP", "indexerFlags": 0,
		"rejections": rej,
	}
}

func TestImportMissing_Radarr(t *testing.T) {
	f, c := newFakeImportArr(t, KindRadarr)
	f.items = []map[string]any{movieFile(42, "/dl/radarr_main/Sintel/sintel.mkv"), movieFile(42, "/dl/radarr_main/Sintel/sample.mkv", "Sample")}

	res, err := c.ImportMissing(context.Background(), "/dl/radarr_main/Sintel", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Selected)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 1, res.Skipped["rejected"])
	assert.Equal(t, "1 file(s) Sintel", res.Summary())
	require.Len(t, f.commands, 1)
	file := f.commands[0]["files"].([]any)[0].(map[string]any)
	assert.EqualValues(t, 42, file["movieId"])
	assert.Equal(t, strings.ToUpper(miHash), file["downloadId"])
	assert.NotContains(t, file, "seriesId")

	// The movie has a file now: a re-run sends nothing.
	res, err = c.ImportMissing(context.Background(), "/dl/radarr_main/Sintel", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Zero(t, res.Selected)
	assert.Equal(t, 1, res.Skipped["has file"])
	assert.Len(t, f.commands, 1)
}

func TestImportMissing_RadarrSeveralFilesSkipped(t *testing.T) {
	f, c := newFakeImportArr(t, KindRadarr)
	f.items = []map[string]any{movieFile(42, "/dl/a/cd1.mkv"), movieFile(42, "/dl/a/cd2.mkv")}
	res, err := c.ImportMissing(context.Background(), "/dl/a", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Zero(t, res.Selected)
	assert.Equal(t, 2, res.Skipped["several files"])
	assert.Empty(t, f.commands)

	f.items = []map[string]any{movieFile(42, "/dl/a/m.mkv"), movieFile(43, "/dl/a/n.mkv")}
	res, err = c.ImportMissing(context.Background(), "/dl/a", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.True(t, res.Ambiguous)

	res, err = c.ImportMissing(context.Background(), "/dl/a", miHash, ImportTarget{MovieID: 43})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 1, res.Skipped["other movie"])
}

// A file rejected only for an existing file (not an upgrade, already imported, ...) is
// "has file" (final for the slow lane), any other rejection keeps it "rejected".
func TestImportMissing_ExistingFileRejectionsAreHasFile(t *testing.T) {
	f, c := newFakeImportArr(t, KindSonarr)
	f.lib(1, 4, false)
	f.items = []map[string]any{
		epFile(91, 1, 1, "Not an upgrade for existing episode file(s). Existing quality: WEBDL-1080p. New Quality WEBDL-720p."),
		epFile(91, 1, 2, "Not a quality revision upgrade for existing episode file(s)", "Episode file already imported at 10/14/2026"),
		epFile(91, 1, 3, "NOT A CUSTOM FORMAT UPGRADE for existing episode file(s)"),
		epFile(91, 1, 4, "Not an upgrade for existing episode file(s)", "Sample"),
	}
	res, err := c.ImportMissing(context.Background(), "/dl/x", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Zero(t, res.Selected)
	assert.Equal(t, 3, res.Skipped["has file"])
	assert.Equal(t, 1, res.Skipped["rejected"], "one non-existing-file rejection keeps it retriable")

	f, c = newFakeImportArr(t, KindRadarr)
	f.items = []map[string]any{movieFile(42, "/dl/a/m.mkv", "Not a quality revision upgrade for existing movie file(s)")}
	res, err = c.ImportMissing(context.Background(), "/dl/a", miHash, ImportTarget{})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"has file": 1}, res.Skipped)
	assert.Empty(t, f.commands)
}
