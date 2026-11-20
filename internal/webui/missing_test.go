package webui_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/missing"
	"github.com/your-username/lazarr/internal/webui"
)

// fakeMissingProvider adds the optional Missing tab contract to fakeProvider.
type fakeMissingProvider struct {
	fakeProvider
	snap      missing.Snapshot
	err       error
	refreshes []bool
	searches  []missing.SearchRequest
	searchErr error
}

func (f *fakeMissingProvider) Missing(_ context.Context, refresh bool) (missing.Snapshot, error) {
	f.refreshes = append(f.refreshes, refresh)
	return f.snap, f.err
}

func (f *fakeMissingProvider) MissingSearch(_ context.Context, req missing.SearchRequest) (missing.SearchResult, error) {
	f.searches = append(f.searches, req)
	if f.searchErr != nil {
		return missing.SearchResult{}, f.searchErr
	}
	return missing.SearchResult{Category: req.Category, Command: "SeasonSearch", CommandID: 42}, nil
}

func missingSnap() missing.Snapshot {
	return missing.Snapshot{Enabled: true, RefreshedAt: 1760000000, RefreshInterval: "30m0s", Arrs: []missing.ArrMissing{{
		Category: "sonarr_main", Kind: "sonarr", TotalMissing: 3, Queued: 1, RefreshedAt: 1760000000,
		Items: []missing.Item{{Type: "season", Title: "Lindy Mercer", Year: 2001, SeriesID: 9, Season: 2,
			Episodes: []int{1, 2, 5}, EpisodeIDs: []int{91, 92, 95}, EpisodeRanges: "E1–2, E5", Count: 3, Queued: 1,
			SearchText: "Lindy Mercer S02", TargetTitle: "Lindy Mercer S02"}},
	}, {Category: "radarr_main", Kind: "radarr", Error: "HTTP 503", Items: []missing.Item{}}}}
}

func TestAPIMissing_Shape(t *testing.T) {
	prov := &fakeMissingProvider{snap: missingSnap()}
	h := newHandler(t, prov)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/missing", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, []bool{false}, prov.refreshes)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.Equal(t, true, raw["enabled"])
	assert.EqualValues(t, 1760000000, raw["refreshed_at"])
	arrs := raw["arrs"].([]any)
	require.Len(t, arrs, 2)
	son := arrs[0].(map[string]any)
	assert.Equal(t, "sonarr_main", son["category"])
	assert.EqualValues(t, 3, son["total_missing"])
	it := son["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "E1–2, E5", it["episode_ranges"])
	assert.EqualValues(t, 9, it["series_id"])
	assert.EqualValues(t, 2, it["season"])
	assert.Equal(t, "Lindy Mercer S02", it["target_title"])
	assert.Equal(t, "HTTP 503", arrs[1].(map[string]any)["error"])
}

func TestAPIMissing_RefreshFlagAndRateLimit(t *testing.T) {
	prov := &fakeMissingProvider{snap: missingSnap()}
	h := newHandler(t, prov)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/missing?refresh=1", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, []bool{true}, prov.refreshes)

	prov.err = &missing.RateLimitedError{What: "refresh", Wait: 41500 * time.Millisecond}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/missing?refresh=1", nil))
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, "42", rr.Header().Get("Retry-After"))
	assert.Contains(t, rr.Body.String(), "rate-limited")
}

func TestAPIMissing_Disabled(t *testing.T) {
	// A provider without the optional interface, and one that reports it disabled.
	for _, prov := range []webui.Provider{&fakeProvider{}, &fakeMissingProvider{err: webui.ErrMissingDisabled, searchErr: webui.ErrMissingDisabled}} {
		h := newHandler(t, prov)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/missing", nil))
		require.Equal(t, http.StatusOK, rr.Code)
		var snap missing.Snapshot
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &snap))
		assert.False(t, snap.Enabled)
		assert.NotNil(t, snap.Arrs)
		assert.Contains(t, rr.Body.String(), `"arrs":[]`)

		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/missing/search", strings.NewReader(`{"category":"radarr_main","movie_id":1}`)))
		assert.Equal(t, http.StatusBadRequest, rr.Code)
	}
}

func TestAPIMissingSearch(t *testing.T) {
	prov := &fakeMissingProvider{}
	h := newHandler(t, prov)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/missing/search",
		strings.NewReader(`{"category":"sonarr_main","series_id":9,"season":0}`)))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var res missing.SearchResult
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	assert.Equal(t, missing.SearchResult{Category: "sonarr_main", Command: "SeasonSearch", CommandID: 42}, res)
	require.Len(t, prov.searches, 1)
	require.NotNil(t, prov.searches[0].Season, "season 0 is kept")
	assert.Equal(t, 0, *prov.searches[0].Season)
	assert.Equal(t, 9, prov.searches[0].SeriesID)

	for _, tc := range []struct {
		err  error
		code int
	}{
		{fmtInvalid(), http.StatusBadRequest},
		{&missing.RateLimitedError{What: "search", Wait: time.Second}, http.StatusTooManyRequests},
		{errors.New("sonarr: POST /api/v3/command: HTTP 500"), http.StatusBadGateway},
	} {
		prov.searchErr = tc.err
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/missing/search", strings.NewReader(`{"category":"x","movie_id":1}`)))
		assert.Equal(t, tc.code, rr.Code, tc.err.Error())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/missing/search", strings.NewReader(`not json`)))
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/missing/search", nil))
	assert.NotEqual(t, http.StatusOK, rr.Code, "search is POST-only")
}

func fmtInvalid() error { return errors.Join(missing.ErrInvalid, errors.New("unknown arr category")) }

func TestIndex_HasMissingTabAndSlowLaneTarget(t *testing.T) {
	h := newHandler(t, &fakeProvider{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	for _, s := range []string{`id="section-missing"`, `data-tab="missing"`, `id="slow-target"`, `/api/missing/search`, `body.target = slowTarget.target`, `function missSlowState`} {
		assert.Contains(t, body, s)
	}
}
