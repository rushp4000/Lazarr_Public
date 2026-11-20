package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/arrroots"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback/arr"
)

func TestReplacementSearcherOffByDefault(t *testing.T) {
	st, err := catalog.OpenSQLite(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c := config.Default()
	c.Playback.Arrs = []config.Arr{{Category: "sonarr_main", URL: "http://sonarr_main:8989", APIKey: "k"}}
	assert.Nil(t, replacementSearcher(c, st), "off unless repair.search_replacement.enabled")

	c.Repair.SearchReplacement.Enabled = true
	rs := replacementSearcher(c, st)
	require.NotNil(t, rs)
	assert.Equal(t, 10, rs.MaxPerHour)
	assert.Equal(t, 72*time.Hour, rs.Cooldown)
	assert.Contains(t, rs.Arrs, "sonarr_main")

	c.Playback.Arrs[0].APIKey = ""
	assert.Nil(t, replacementSearcher(c, st), "no arr with an api key: stays off")
}

// fakeSonarr logs every request so the test can check the order: the episode files
// must be read BEFORE the rescan command deletes them.
type fakeSonarr struct {
	mu        sync.Mutex
	log       []string
	rescanErr bool
}

func (f *fakeSonarr) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		f.log = append(f.log, "POST "+string(b))
		if f.rescanErr && strings.Contains(string(b), "RescanSeries") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"id":1}`)
		return
	}
	f.log = append(f.log, "GET "+r.URL.Path)
	switch r.URL.Path {
	case "/api/v3/rootfolder":
		_, _ = io.WriteString(w, `[{"path":"/tv"}]`)
	case "/api/v3/series":
		_, _ = io.WriteString(w, `[{"id":412,"path":"/tv/Charter House"}]`)
	case "/api/v3/episodefile":
		_, _ = io.WriteString(w, `[{"id":51,"path":"/tv/Charter House/Season 04/S04E07.mkv"}]`)
	case "/api/v3/episode":
		_, _ = io.WriteString(w, `[{"id":9007,"episodeFileId":51}]`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRemovedLinkRescannerQueuesAfterAcceptedRescan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		rescanErr bool
		wantQueue bool
	}{
		{"enabled: files read before the rescan, item queued", true, false, true},
		{"enabled but the rescan failed: nothing queued", true, true, false},
		{"disabled: plain rescan", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSonarr{rescanErr: tc.rescanErr}
			srv := httptest.NewServer(http.HandlerFunc(f.handler))
			defer srv.Close()

			st, err := catalog.OpenSQLite(filepath.Join(t.TempDir(), "db.sqlite"))
			require.NoError(t, err)
			defer func() { _ = st.Close() }()

			c := config.Default()
			c.Policy.RepairUnlinkDead = true
			c.Repair.SearchReplacement.Enabled = tc.enabled
			c.Playback.Arrs = []config.Arr{{Category: "sonarr_main", URL: srv.URL, APIKey: "k", LibraryRoot: "/d/TV Shows/sonarr_main"}}
			res := &arrroots.Resolver{Arrs: []arrroots.Arr{{Category: "sonarr_main", LibraryRoot: "/d/TV Shows/sonarr_main",
				Client: arr.New(arr.KindSonarr, srv.URL, "k", nil)}}}

			removedLinkRescanner(res, replacementSearcher(c, st))(context.Background(),
				[]string{"/d/TV Shows/sonarr_main/Charter House/Season 04/S04E07.mkv"})

			files, rescan := -1, -1
			for i, l := range f.log {
				switch {
				case l == "GET /api/v3/episodefile":
					files = i
				case strings.Contains(l, "RescanSeries"):
					rescan = i
				}
			}
			require.GreaterOrEqual(t, rescan, 0, "the series is rescanned: %v", f.log)
			if tc.enabled {
				require.GreaterOrEqual(t, files, 0, "episode files are read: %v", f.log)
				assert.Less(t, files, rescan, "files are read before the rescan")
			} else {
				assert.Equal(t, -1, files, "no extra arr calls when disabled")
			}
			for _, l := range f.log {
				assert.NotContains(t, l, "EpisodeSearch", "no search before the rescan settles")
			}
			_, err = st.(catalog.ReplacementStore).GetReplacement("sonarr_main", 9007)
			if tc.wantQueue {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, catalog.ErrNotFound)
			}
		})
	}
}
