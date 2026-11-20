package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePMS mimics the PMS 1.43 responses observed live on 2026-10-09.
type fakePMS struct {
	mu        sync.Mutex
	puts      []string
	busy      bool
	markers   string
	subscribe bool
	introPref string
	prefPuts  []string
}

func (f *fakePMS) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /library/all", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "tok", r.Header.Get("X-Plex-Token"))
		if r.URL.Query().Get("type") != "4" {
			_, _ = w.Write([]byte(`{"MediaContainer":{"size":0}}`))
			return
		}
		// PMS matches `file` as a substring: return an exact and a near match.
		_, _ = fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[
			{"ratingKey":"101","type":"episode","Media":[{"Part":[{"file":%q}]}]},
			{"ratingKey":"102","type":"episode","Media":[{"Part":[{"file":%q}]}]}]}}`,
			r.URL.Query().Get("file"), r.URL.Query().Get("file")+".extra")
	})
	mux.HandleFunc("PUT /library/metadata/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		a := r.PathValue("id") + "/" + r.PathValue("action")
		if q := r.URL.RawQuery; q != "" {
			a += "?" + q
		}
		f.puts = append(f.puts, a)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /activities", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.busy {
			_, _ = w.Write([]byte(`{"MediaContainer":{"Activity":[{"type":"media.generate.credits","title":"Detecting credits"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":0}}`))
	})
	mux.HandleFunc("GET /library/metadata/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		typ := "episode"
		if r.PathValue("id") == "900" {
			typ = "movie"
		}
		_, _ = fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"ratingKey":%q,"type":%q,"parentRatingKey":"50","Marker":[%s]}]}}`, r.PathValue("id"), typ, f.markers)
	})
	mux.HandleFunc("GET /library/metadata/{id}/children", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "50", r.PathValue("id"))
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[
			{"ratingKey":"101","type":"episode","Media":[{"Part":[{"file":"/lib/S01E01.mkv"}]}]},
			{"ratingKey":"102","type":"episode","Media":[{"Part":[{"file":"/lib/S01E02.mkv"}]}]}]}}`))
	})
	mux.HandleFunc("PUT /:/prefs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prefPuts = append(f.prefPuts, r.URL.RawQuery)
		if v := r.URL.Query().Get("GenerateIntroMarkerBehavior"); v != "" {
			f.introPref = v
		}
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"MediaContainer":{"myPlexSubscription":%v}}`, f.subscribe)
	})
	mux.HandleFunc("GET /:/prefs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"MediaContainer":{"Setting":[
			{"id":"GenerateIntroMarkerBehavior","value":%q},
			{"id":"GenerateCreditsMarkerBehavior","value":"scheduled"},
			{"id":"ScheduledLibraryUpdatesEnabled","value":false}]}}`, f.introPref)
	})
	return mux
}

func newFake(t *testing.T, f *fakePMS) *Client {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok", nil)
}

func TestFindItemsExactPathOnly(t *testing.T) {
	c := newFake(t, &fakePMS{})
	ids, err := c.FindItems(context.Background(), "/debrid_symlinks/TV Shows/x/S01E01.mkv")
	require.NoError(t, err)
	assert.Equal(t, []string{"101"}, ids, "substring matches are dropped")
}

func TestStartAnalysisPutsEachAction(t *testing.T) {
	f := &fakePMS{}
	c := newFake(t, f)
	require.NoError(t, c.StartAnalysis(context.Background(), "101", []string{"intro", "credits", "voice_activity", "chapter_thumbs", "index", "analyze"}))
	assert.Equal(t, []string{"101/intro?force=1", "101/credits?force=1&manual=1", "101/voiceActivity", "101/chapterThumbs", "101/index", "101/analyze"}, f.puts,
		"intro/credits carry the flags PMS needs to actually run them")
	assert.Error(t, c.StartAnalysis(context.Background(), "../x", []string{"intro"}))
	assert.Error(t, c.StartAnalysis(context.Background(), "101", []string{"bogus"}))
}

func TestAnalysisStatusBusyAndMarkers(t *testing.T) {
	f := &fakePMS{busy: true}
	c := newFake(t, f)
	st, err := c.AnalysisStatus(context.Background(), "101")
	require.NoError(t, err)
	assert.True(t, st.Busy)
	assert.Zero(t, st.Intro+st.Credits)

	f.mu.Lock()
	f.busy = false
	f.markers = `{"type":"intro","startTimeOffset":1000,"endTimeOffset":2000},{"type":"credits","startTimeOffset":9000,"endTimeOffset":9900,"final":true}`
	f.mu.Unlock()
	st, err = c.AnalysisStatus(context.Background(), "101")
	require.NoError(t, err)
	assert.False(t, st.Busy)
	assert.Equal(t, 1, st.Intro)
	assert.Equal(t, 1, st.Credits)
}

func TestPreflightWarnsOnNeverAndNoPlexPass(t *testing.T) {
	c := newFake(t, &fakePMS{subscribe: false, introPref: "never"})
	warns, err := c.Preflight(context.Background(), []string{"intro", "credits"})
	require.NoError(t, err)
	require.Len(t, warns, 2)
	assert.Contains(t, warns[0], "Plex Pass")
	assert.Contains(t, warns[1], "GenerateIntroMarkerBehavior=never")

	c = newFake(t, &fakePMS{subscribe: true, introPref: "scheduled"})
	warns, err = c.Preflight(context.Background(), []string{"intro", "credits"})
	require.NoError(t, err)
	assert.Empty(t, warns)
}

func TestOpenGatesFlipsOnlyNeverAndRestores(t *testing.T) {
	f := &fakePMS{introPref: "never"} // credits is "scheduled" in the fake already
	c := newFake(t, f)
	restore, err := c.OpenGates(context.Background(), []string{"intro", "credits", "index"})
	require.NoError(t, err)
	assert.Equal(t, []string{"GenerateIntroMarkerBehavior=scheduled"}, f.prefPuts, "only never→scheduled, only intro/credits gates")
	require.NoError(t, restore(context.Background()))
	assert.Equal(t, "GenerateIntroMarkerBehavior=never", f.prefPuts[1])

	f.prefPuts = nil
	require.NoError(t, c.ResetGates(context.Background(), []string{"intro", "credits", "analyze"}))
	require.Len(t, f.prefPuts, 1)
	assert.Contains(t, f.prefPuts[0], "GenerateIntroMarkerBehavior=never")
	assert.Contains(t, f.prefPuts[0], "GenerateCreditsMarkerBehavior=never")
	assert.NotContains(t, f.prefPuts[0], "Loudness")
}

func TestSeasonPaths(t *testing.T) {
	c := newFake(t, &fakePMS{})
	paths, err := c.SeasonPaths(context.Background(), "101")
	require.NoError(t, err)
	assert.Equal(t, []string{"/lib/S01E01.mkv", "/lib/S01E02.mkv"}, paths)
	paths, err = c.SeasonPaths(context.Background(), "900")
	require.NoError(t, err)
	assert.Nil(t, paths, "a movie has no season")
}
