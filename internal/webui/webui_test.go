package webui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/localcache"
	"github.com/your-username/lazarr/internal/logging"
	"github.com/your-username/lazarr/internal/materialize"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/repair"
	"github.com/your-username/lazarr/internal/webui"
)

// fakeProvider is a test implementation of webui.Provider.
type fakeProvider struct {
	status         webui.StatusSnapshot
	releases       []*catalog.Release
	relTotal       int
	matSet         []webui.MaterializedItem
	summary        *metrics.Summary
	summaryErr     error
	releaseErr     error
	auditErr       error
	releasedHashes []string
	auditCalled    bool
	cfg            webui.SafeConfig

	settings      webui.Settings
	savedSettings *webui.Settings
	saveRestart   bool
	saveErr       error
	logEntries    []logging.Entry
	restartCalled bool

	cacheSnap  *localcache.Snapshot
	cacheFiles []localcache.LocalFileView
	pulled     []string
	evicted    []string
	polled     bool

	slowView      webui.SlowLaneView
	slowAdded     []string
	slowTargets   []*catalog.SlowLaneTarget
	slowAddErr    error
	slowAddExists bool
	slowCancelled []string
	slowCancelErr error
	slowTorrents  []string                  // "<data>|<category>" per SlowLaneAddTorrent call
	slowTorTgts   []*catalog.SlowLaneTarget // target per SlowLaneAddTorrent call
}

func (f *fakeProvider) Status() webui.StatusSnapshot { return f.status }
func (f *fakeProvider) ListReleases(_ catalog.ReleaseFilter) ([]*catalog.Release, int, error) {
	return f.releases, f.relTotal, nil
}
func (f *fakeProvider) MaterializedSet() []webui.MaterializedItem { return f.matSet }
func (f *fakeProvider) MetricsSummary() (*metrics.Summary, error) {
	return f.summary, f.summaryErr
}
func (f *fakeProvider) ForceRelease(hash string) error {
	if f.releaseErr != nil {
		return f.releaseErr
	}
	f.releasedHashes = append(f.releasedHashes, hash)
	return nil
}
func (f *fakeProvider) TriggerAudit() error {
	f.auditCalled = true
	return f.auditErr
}
func (f *fakeProvider) TriggerRepairScan(_ context.Context) ([]materialize.RepairEntry, error) {
	return nil, nil
}
func (f *fakeProvider) ListEvicted() ([]*catalog.Release, error) { return nil, nil }
func (f *fakeProvider) Dangling() repair.DanglingReport          { return repair.DanglingReport{} }
func (f *fakeProvider) Orphans() materialize.OrphanReport {
	return materialize.OrphanReport{At: 1, Items: []materialize.AccountOrphan{{TorBoxID: 4000105, Name: "Sample.Show.B.S01E05"}}}
}
func (f *fakeProvider) ForgetRelease(_ string) error { return nil }
func (f *fakeProvider) SafeConfig() webui.SafeConfig { return f.cfg }
func (f *fakeProvider) GetSettings() webui.Settings  { return f.settings }
func (f *fakeProvider) SaveSettings(s webui.Settings) (bool, error) {
	if f.saveErr != nil {
		return false, f.saveErr
	}
	f.savedSettings = &s
	return f.saveRestart, nil
}
func (f *fakeProvider) Logs(_ string, _ int) []logging.Entry { return f.logEntries }
func (f *fakeProvider) CacheSnapshot() *localcache.Snapshot {
	if f.cacheSnap == nil {
		return nil
	}
	return f.cacheSnap
}
func (f *fakeProvider) CacheFiles() []localcache.LocalFileView { return f.cacheFiles }
func (f *fakeProvider) CachePull(hash string) (int, error) {
	f.pulled = append(f.pulled, hash)
	return 2, nil
}
func (f *fakeProvider) CacheEvict(hash string) error {
	f.evicted = append(f.evicted, hash)
	return nil
}
func (f *fakeProvider) CachePoll(context.Context) error    { f.polled = true; return nil }
func (f *fakeProvider) CacheAnalyze(string) (int, error)   { return 1, nil }
func (f *fakeProvider) CacheAnalyzeBackfill() (int, error) { return 2, nil }
func (f *fakeProvider) Restart() error {
	f.restartCalled = true
	return nil
}

func (f *fakeProvider) SlowLane() (webui.SlowLaneView, error) { return f.slowView, nil }
func (f *fakeProvider) SlowLaneAdd(input, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error) {
	f.slowAdded = append(f.slowAdded, input+"|"+category)
	f.slowTargets = append(f.slowTargets, target)
	it := &catalog.SlowLaneItem{Hash: "aa", Category: category, State: catalog.SlowQueued}
	if f.slowAddErr != nil {
		if f.slowAddExists {
			return it, f.slowAddErr
		}
		return nil, f.slowAddErr
	}
	return it, nil
}

// SlowLaneAddTorrent fakes the qbit side by content: "bad…" is refused, "dup…" is
// already active, anything else is queued under a hash named after the content.
func (f *fakeProvider) SlowLaneAddTorrent(torrent []byte, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error) {
	f.slowTorrents = append(f.slowTorrents, string(torrent)+"|"+category)
	f.slowTorTgts = append(f.slowTorTgts, target)
	it := &catalog.SlowLaneItem{Hash: string(torrent), Category: category, State: catalog.SlowQueued}
	switch {
	case category == "":
		return nil, errors.New("a category is required (the arr that should import it)")
	case strings.HasPrefix(string(torrent), "bad"):
		return nil, errors.New("not a valid .torrent file: torrent: info dict not found")
	case strings.HasPrefix(string(torrent), "dup"):
		return it, errors.New("already in the slow lane")
	}
	return it, nil
}
func (f *fakeProvider) SlowLaneCancel(hash string) error {
	f.slowCancelled = append(f.slowCancelled, hash)
	return f.slowCancelErr
}

func newHandler(t *testing.T, prov webui.Provider) http.Handler {
	t.Helper()
	h, err := webui.New(prov, "", "")
	require.NoError(t, err)
	return h
}

func newHandlerAuth(t *testing.T, prov webui.Provider, user, pass string) http.Handler {
	t.Helper()
	h, err := webui.New(prov, user, pass)
	require.NoError(t, err)
	return h
}

// ─── index ────────────────────────────────────────────────────────────────────

func TestIndex_ServesHTML(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "Lazarr")
}

// ─── /api/status ─────────────────────────────────────────────────────────────

func TestAPIStatus_Shape(t *testing.T) {
	prov := &fakeProvider{
		status: webui.StatusSnapshot{
			Version:       "v1.0.0",
			UptimeSeconds: 120,
			Mounted:       true,
			SlotsInUse:    1,
			SlotsTotal:    3,
			LastAuditUnix: time.Now().Unix(),
		},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got webui.StatusSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "v1.0.0", got.Version)
	assert.True(t, got.Mounted)
	assert.Equal(t, 1, got.SlotsInUse)
	assert.Equal(t, 3, got.SlotsTotal)
}

// ─── /api/releases ────────────────────────────────────────────────────────────

func TestAPIReleases_Shape(t *testing.T) {
	prov := &fakeProvider{
		releases: []*catalog.Release{
			{Hash: "aabbccddeeff00112233445566778899aabbccdd", Name: "Big Buck Bunny", Category: "radarr_hq", State: catalog.StateVirtual},
		},
		relTotal: 1,
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/releases", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got struct {
		Releases []*catalog.Release `json:"releases"`
		Total    int                `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Releases, 1)
	assert.Equal(t, "Big Buck Bunny", got.Releases[0].Name)
}

func TestAPIReleases_EmptyIsArray(t *testing.T) {
	h := newHandler(t, &fakeProvider{releases: nil, relTotal: 0})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/releases", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"releases":[]`)
}

// ─── /api/materialized ────────────────────────────────────────────────────────

func TestAPIMaterialized_Shape(t *testing.T) {
	prov := &fakeProvider{
		matSet: []webui.MaterializedItem{
			{Hash: "aabbcc001122334455667788990011223344556677", TorBoxID: 42, Refs: 1},
		},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/materialized", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got []webui.MaterializedItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, int64(42), got[0].TorBoxID)
}

func TestAPIMaterialized_EmptyIsArray(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/materialized", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "[]")
}

// ─── /api/metrics-summary ─────────────────────────────────────────────────────

func TestAPIMetricsSummary_Shape(t *testing.T) {
	prov := &fakeProvider{
		summary: &metrics.Summary{GrabsTotal: 5, MaterializesTotal: 3, ReleasesTotal: 2},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/metrics-summary", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var got metrics.Summary
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, float64(5), got.GrabsTotal)
}

func TestAPIMetricsSummary_Error(t *testing.T) {
	prov := &fakeProvider{summaryErr: errors.New("gather failed")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/metrics-summary", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── /api/config ─────────────────────────────────────────────────────────────

func TestAPIConfig_RedactsSecrets(t *testing.T) {
	prov := &fakeProvider{
		cfg: webui.SafeConfig{
			TorBoxAPIBase: "https://api.torbox.app/v1/api",
			Categories:    []string{"radarr_hq"},
		},
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/config", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	// api_key must not appear in the body
	body := rec.Body.String()
	assert.NotContains(t, body, "api_key")
	assert.Contains(t, body, "torbox_api_base")
}

// ─── POST /api/releases/{hash}/release ────────────────────────────────────────

func TestForceRelease_CallsProvider(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/releases/aabbcc001122334455667788990011223344556677/release", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, prov.releasedHashes, 1)
	assert.Equal(t, "aabbcc001122334455667788990011223344556677", prov.releasedHashes[0])
}

func TestForceRelease_ProviderError(t *testing.T) {
	prov := &fakeProvider{releaseErr: errors.New("engine busy")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/releases/abc/release", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── POST /api/audit/run ──────────────────────────────────────────────────────

func TestAuditRun_CallsProvider(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit/run", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, prov.auditCalled)
}

func TestAuditRun_ProviderError(t *testing.T) {
	prov := &fakeProvider{auditErr: errors.New("torbox down")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/audit/run", nil))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ─── auth middleware ──────────────────────────────────────────────────────────

func TestAuth_NoCredsReturns401(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_WrongPasswordReturns401(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "admin", "secret")
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("admin", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAuth_CorrectCredsPass(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "admin", "secret")
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAuth_DisabledWhenNoCreds(t *testing.T) {
	h := newHandlerAuth(t, &fakeProvider{}, "", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ─── disabled when listen empty ────────────────────────────────────────────────

func TestWebUI_DisabledWhenListenEmpty(t *testing.T) {
	// If listen is empty, main.go skips creating the server entirely; this test
	// confirms New() still works and handler still responds (the listen-empty guard
	// is in main.go, not in the handler).
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// ─── /api/settings ────────────────────────────────────────────────────────────

func TestSettingsGet_NeverLeaksSecrets(t *testing.T) {
	prov := &fakeProvider{settings: webui.Settings{
		// A misbehaving provider that DOES return secrets — the handler must blank them.
		TorBoxAPIKey:    "sk-LEAK",
		TorBoxAPIKeySet: true,
		WebUIPassword:   "pw-LEAK",
		QBitUsername:    "lazarr",
		Categories:      []string{"radarr_hq"},
		IdleTTL:         "168h0m0s",
	}}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/settings", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "LEAK")
	assert.Contains(t, body, `"torbox_api_key_set":true`)
	assert.Contains(t, body, "radarr_hq")
}

func TestSettingsSave_RoundTrip(t *testing.T) {
	prov := &fakeProvider{saveRestart: true}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	body := `{"log_level":"debug","categories":["radarr_hq","sonarr_hq"],"idle_ttl":"168h","max_hold":"720h"}`
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, prov.savedSettings)
	assert.Equal(t, "debug", prov.savedSettings.LogLevel)
	assert.Equal(t, []string{"radarr_hq", "sonarr_hq"}, prov.savedSettings.Categories)
	assert.Contains(t, rec.Body.String(), `"restart_required":true`)
}

func TestSettingsSave_ValidationErrorIs400(t *testing.T) {
	prov := &fakeProvider{saveErr: errors.New("idle_ttl must be strictly less than max_hold")}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "idle_ttl")
}

func TestSettingsSave_BadJSONIs400(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{nope`)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ─── /api/logs ────────────────────────────────────────────────────────────────

func TestLogs_Shape(t *testing.T) {
	prov := &fakeProvider{logEntries: []logging.Entry{
		{TimeUnixMs: 1781000000000, Level: "INFO", Msg: "materialized", Attrs: "hash=abc"},
	}}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs?level=info&limit=50", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "materialized")
	assert.Contains(t, rec.Body.String(), `"count":1`)
}

func TestLogs_EmptyIsArray(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"entries":[]`)
}

// ─── /api/restart ─────────────────────────────────────────────────────────────

func TestRestart_CallsProvider(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/restart", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, prov.restartCalled)
}

func TestRestart_GETNotAllowed(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/restart", nil))
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.False(t, prov.restartCalled)
}

// ─── releases wire format ─────────────────────────────────────────────────────

func TestAPIReleases_SnakeCaseWireFormat(t *testing.T) {
	// Guards the v1.0.0 bug where catalog.Release had no JSON tags, so the API
	// emitted PascalCase keys the dashboard JS could not read (names showed empty).
	prov := &fakeProvider{
		releases: []*catalog.Release{{
			Hash: "aabbccddeeff00112233445566778899aabbccdd", Name: "Big Buck Bunny",
			Category: "radarr_hq", State: catalog.StateMaterialized, TotalSize: 123,
			Magnet: "magnet:?xt=urn:btih:secret-trackers",
		}},
		relTotal: 1,
	}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/releases", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	for _, key := range []string{`"hash"`, `"name"`, `"category"`, `"state"`, `"total_size"`, `"last_access"`, `"cache_status"`} {
		assert.Contains(t, body, key)
	}
	assert.NotContains(t, body, `"Hash"`)
	assert.NotContains(t, body, "magnet:?", "magnet must not be exposed on the wire")
}

func TestAPIMaterialized_IncludesNames(t *testing.T) {
	prov := &fakeProvider{matSet: []webui.MaterializedItem{{
		Hash: "aabbcc", Name: "Sintel", Category: "radarr_hq", TotalSize: 99, TorBoxID: 7,
	}}}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/materialized", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"Sintel"`)
	assert.Contains(t, rec.Body.String(), `"category":"radarr_hq"`)
}

// ---- Workstream G: cache endpoints ----

func TestCacheEndpoints(t *testing.T) {
	fp := &fakeProvider{}
	h, err := webui.New(fp, "", "")
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Disabled → {"enabled": false}.
	resp, err := http.Get(srv.URL + "/api/cache")
	require.NoError(t, err)
	var off map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&off))
	resp.Body.Close()
	assert.Equal(t, false, off["enabled"])

	fp.cacheSnap = &localcache.Snapshot{Enabled: true, Dir: "/cache", FreePct: 42, Files: map[string]int{"complete": 3}}
	fp.cacheFiles = []localcache.LocalFileView{{Hash: "abc", FileID: 1, Name: "Show", State: "complete"}}
	resp, err = http.Get(srv.URL + "/api/cache")
	require.NoError(t, err)
	var snap localcache.Snapshot
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	resp.Body.Close()
	assert.Equal(t, 3, snap.Files["complete"])

	resp, err = http.Get(srv.URL + "/api/cache/files")
	require.NoError(t, err)
	var files struct {
		Files []localcache.LocalFileView `json:"files"`
		Count int                        `json:"count"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&files))
	resp.Body.Close()
	assert.Equal(t, 1, files.Count)

	resp, err = http.Post(srv.URL+"/api/cache/abc/pull", "", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, []string{"abc"}, fp.pulled)

	resp, err = http.Post(srv.URL+"/api/cache/abc/evict", "", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, []string{"abc"}, fp.evicted)

	resp, err = http.Post(srv.URL+"/api/cache/poll", "", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.True(t, fp.polled)

	// GET on a mutating route is not found (never falls through to HTML).
	resp, err = http.Get(srv.URL + "/api/cache/abc/pull")
	require.NoError(t, err)
	resp.Body.Close()
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
}

// ─── /api/slowlane ───────────────────────────────────────────────────────────

func TestAPISlowLane_ListShape(t *testing.T) {
	prov := &fakeProvider{slowView: webui.SlowLaneView{Enabled: true, MaxConcurrent: 1, PollInterval: "10m0s",
		Items: []*catalog.SlowLaneItem{{Hash: "aa", Name: "Show", State: catalog.SlowDownloading, Magnet: "magnet:?secret-trackers"}}}}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/slowlane", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "magnet:", "the magnet is internal plumbing")
	var got webui.SlowLaneView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.True(t, got.Enabled)
	require.Len(t, got.Items, 1)
	assert.Equal(t, catalog.SlowDownloading, got.Items[0].State)

	// An empty list is [] (the dashboard iterates it), never null.
	rec = httptest.NewRecorder()
	newHandler(t, &fakeProvider{}).ServeHTTP(rec, httptest.NewRequest("GET", "/api/slowlane", nil))
	assert.Contains(t, rec.Body.String(), `"items":[]`)
}

func TestAPISlowLane_Add(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		err        error
		exists     bool
		wantStatus int
	}{
		{"ok", `{"magnet":"magnet:?xt=urn:btih:aa","category":"sonarr_main"}`, nil, false, http.StatusOK},
		{"bad json", `{`, nil, false, http.StatusBadRequest},
		{"rejected", `{"magnet":"x","category":"sonarr_main"}`, errors.New("not a magnet link or infohash"), false, http.StatusBadRequest},
		{"already queued", `{"magnet":"aa","category":"sonarr_main"}`, errors.New("already in the slow lane"), true, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{slowAddErr: tc.err, slowAddExists: tc.exists}
			rec := httptest.NewRecorder()
			newHandler(t, prov).ServeHTTP(rec, httptest.NewRequest("POST", "/api/slowlane", strings.NewReader(tc.body)))
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
		})
	}
}

func TestAPISlowLane_CancelIsPostOnly(t *testing.T) {
	prov := &fakeProvider{}
	h := newHandler(t, prov)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/slowlane/aa/cancel", nil))
	assert.NotEqual(t, http.StatusOK, rec.Code)
	assert.Empty(t, prov.slowCancelled)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/slowlane/aa/cancel", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"aa"}, prov.slowCancelled)

	prov.slowCancelErr = errors.New("item is already failed")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/slowlane/aa/cancel", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// part is one multipart part: a text field, or (file != "") a `torrent` file part.
type part struct{ name, file, value string }

func field(name, value string) part         { return part{name: name, value: value} }
func torrentFile(file, content string) part { return part{name: "torrent", file: file, value: content} }

// slowUpload builds a multipart POST /api/slowlane with the parts in the given order.
func slowUpload(t *testing.T, parts ...part) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.file == "" {
			require.NoError(t, mw.WriteField(p.name, p.value))
			continue
		}
		fw, err := mw.CreateFormFile(p.name, p.file)
		require.NoError(t, err)
		_, err = fw.Write([]byte(p.value))
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	req := httptest.NewRequest("POST", "/api/slowlane", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

type slowUploadResp struct {
	Queued  int    `json:"queued"`
	Error   string `json:"error"`
	Results []struct {
		Source string                `json:"source"`
		Item   *catalog.SlowLaneItem `json:"item"`
		Exists bool                  `json:"exists"`
		Error  string                `json:"error"`
	} `json:"results"`
}

func decodeUpload(t *testing.T, rec *httptest.ResponseRecorder) slowUploadResp {
	t.Helper()
	var got slowUploadResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
	return got
}

func TestAPISlowLane_UploadMultipleWithOneBad(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	// Category as the LAST part: the handler must not depend on part order.
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t,
		torrentFile("one.torrent", "t1"), torrentFile("broken.torrent", "bad"),
		part{name: "torrents", file: "two.torrent", value: "t2"}, // qBittorrent's part name works too
		field("category", "sonarr_main")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeUpload(t, rec)
	assert.Equal(t, 2, got.Queued)
	assert.Empty(t, got.Error)
	require.Len(t, got.Results, 3)
	assert.Equal(t, "one.torrent", got.Results[0].Source)
	require.NotNil(t, got.Results[0].Item)
	assert.Equal(t, "t1", got.Results[0].Item.Hash)
	assert.Equal(t, "broken.torrent", got.Results[1].Source)
	assert.Contains(t, got.Results[1].Error, "not a valid .torrent")
	assert.Nil(t, got.Results[1].Item)
	assert.Empty(t, got.Results[2].Error)
	assert.Equal(t, []string{"t1|sonarr_main", "bad|sonarr_main", "t2|sonarr_main"}, prov.slowTorrents)
}

func TestAPISlowLane_UploadStatuses(t *testing.T) {
	cases := []struct {
		name       string
		parts      []part
		wantStatus int
		wantErr    string
	}{
		{"single bad file", []part{field("category", "sonarr_main"), torrentFile("x.torrent", "bad")},
			http.StatusBadRequest, "not a valid .torrent"},
		{"all already active", []part{field("category", "sonarr_main"), torrentFile("a.torrent", "dup1"), torrentFile("b.torrent", "dup2")},
			http.StatusConflict, "already in the slow lane"},
		{"active + bad is 400", []part{field("category", "sonarr_main"), torrentFile("a.torrent", "dup1"), torrentFile("b.torrent", "bad")},
			http.StatusBadRequest, "b.torrent: not a valid"},
		{"missing category", []part{torrentFile("a.torrent", "t1")}, http.StatusBadRequest, "category is required"},
		{"nothing to add", []part{field("category", "sonarr_main")}, http.StatusBadRequest, "nothing to add"},
		{"magnet field only", []part{field("category", "sonarr_main"), field("magnet", "magnet:?xt=urn:btih:aa")},
			http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newHandler(t, &fakeProvider{}).ServeHTTP(rec, slowUpload(t, tc.parts...))
			assert.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			if tc.wantErr != "" {
				assert.Contains(t, rec.Body.String(), tc.wantErr)
			}
		})
	}
}

func TestAPISlowLane_UploadMagnetAndFiles(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, field("category", "radarr_4k"),
		field("magnet", "  magnet:?xt=urn:btih:aa  "), torrentFile("one.torrent", "t1")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeUpload(t, rec)
	assert.Equal(t, 2, got.Queued)
	require.Len(t, got.Results, 2)
	assert.Equal(t, "magnet", got.Results[0].Source)
	assert.Equal(t, []string{"magnet:?xt=urn:btih:aa|radarr_4k"}, prov.slowAdded, "the magnet field takes the JSON path's provider call")
	assert.Equal(t, []string{"t1|radarr_4k"}, prov.slowTorrents)
}

func TestAPISlowLane_UploadOversizeFileRefused(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	big := strings.Repeat("x", 2<<20+1)
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, field("category", "sonarr_main"),
		torrentFile("huge.torrent", big), torrentFile("ok.torrent", "t1")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeUpload(t, rec)
	assert.Equal(t, 1, got.Queued)
	require.Len(t, got.Results, 2)
	assert.Contains(t, got.Results[0].Error, "file too large")
	assert.Equal(t, []string{"t1|sonarr_main"}, prov.slowTorrents, "the oversize file never reaches the provider")
}

func TestAPISlowLane_UploadRequestTooLarge(t *testing.T) {
	prov := &fakeProvider{}
	parts := []part{field("category", "sonarr_main")}
	chunk := strings.Repeat("x", 2<<20) // each at the per-file cap; together over the request cap
	for i := 0; i < 9; i++ {
		parts = append(parts, torrentFile(fmt.Sprintf("f%d.torrent", i), chunk))
	}
	rec := httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, parts...))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Empty(t, prov.slowTorrents, "nothing is queued from a truncated upload")
}

func TestAPISlowLane_UploadTooManyFiles(t *testing.T) {
	prov := &fakeProvider{}
	parts := []part{field("category", "sonarr_main")}
	for i := 0; i < 33; i++ {
		parts = append(parts, torrentFile(fmt.Sprintf("f%d.torrent", i), fmt.Sprintf("t%d", i)))
	}
	rec := httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, parts...))
	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeUpload(t, rec)
	assert.Equal(t, 32, got.Queued)
	assert.Contains(t, got.Results[32].Error, "too many files")
}

func TestAPISlowLane_JSONPathUnchanged(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/slowlane", strings.NewReader(`{"magnet":"magnet:?xt=urn:btih:aa","category":"sonarr_main"}`))
	req.Header.Set("Content-Type", "application/json")
	newHandler(t, prov).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var it catalog.SlowLaneItem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &it), "the JSON path still answers with the bare item")
	assert.Equal(t, "aa", it.Hash)
	assert.Equal(t, []string{"magnet:?xt=urn:btih:aa|sonarr_main"}, prov.slowAdded)
	assert.Empty(t, prov.slowTorrents)
}

// The optional target object is decoded and passed through (the Missing tab POSTs it).
func TestAPISlowLane_AddWithTarget(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	body := `{"magnet":"aa","category":"sonarr_main","target":{"series_id":91,"season":2,"title":"Lindy Mercer"}}`
	newHandler(t, prov).ServeHTTP(rec, httptest.NewRequest("POST", "/api/slowlane", strings.NewReader(body)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, prov.slowTargets, 1)
	assert.Equal(t, &catalog.SlowLaneTarget{SeriesID: 91, Season: 2, Title: "Lindy Mercer"}, prov.slowTargets[0])

	rec = httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, httptest.NewRequest("POST", "/api/slowlane", strings.NewReader(`{"magnet":"aa","category":"radarr_main"}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, prov.slowTargets[1], "no target = nil")
}

// An upload from the Missing tab's "Add torrent…" carries the row's target as a
// multipart `target` field (the JSON body's object); it applies to every input.
func TestAPISlowLane_UploadWithTarget(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, field("category", "sonarr_main"),
		field("target", `{"series_id":91,"season":2,"title":"Lindy Mercer S02"}`),
		field("magnet", "magnet:?xt=urn:btih:aa"), torrentFile("one.torrent", "t1")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	want := &catalog.SlowLaneTarget{SeriesID: 91, Season: 2, Title: "Lindy Mercer S02"}
	require.Len(t, prov.slowTorTgts, 1)
	assert.Equal(t, want, prov.slowTorTgts[0])
	require.Len(t, prov.slowTargets, 1)
	assert.Equal(t, want, prov.slowTargets[0], "the magnet field gets it too")

	// No (or an empty) target field = nil, as before.
	prov = &fakeProvider{}
	rec = httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, field("category", "sonarr_main"), field("target", " "),
		torrentFile("one.torrent", "t1")))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []*catalog.SlowLaneTarget{nil}, prov.slowTorTgts)
}

func TestAPISlowLane_UploadBadTargetRefused(t *testing.T) {
	prov := &fakeProvider{}
	rec := httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, slowUpload(t, field("category", "sonarr_main"),
		field("target", `{"series_id":"x"}`), torrentFile("one.torrent", "t1")))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid target")
	assert.Empty(t, prov.slowTorrents, "nothing is queued with an unreadable target")
}

// The kind map for the UI's category warning is passed through as is.
func TestAPISlowLane_CategoryKinds(t *testing.T) {
	prov := &fakeProvider{slowView: webui.SlowLaneView{Enabled: true, Categories: []string{"sonarr_main", "radarr_main"},
		CategoryKinds: map[string]string{"sonarr_main": "sonarr", "radarr_main": "radarr"}}}
	rec := httptest.NewRecorder()
	newHandler(t, prov).ServeHTTP(rec, httptest.NewRequest("GET", "/api/slowlane", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		CategoryKinds map[string]string `json:"category_kinds"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, map[string]string{"sonarr_main": "sonarr", "radarr_main": "radarr"}, got.CategoryKinds)
}
