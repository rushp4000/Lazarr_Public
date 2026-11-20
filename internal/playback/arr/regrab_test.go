package arr

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const regrabHash = "6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b"

// fakeRegrabArr serves history + blocklist and records DELETEs and command POSTs.
type fakeRegrabArr struct {
	mu        sync.Mutex
	history   string // JSON records array
	blocklist string // JSON records array
	deletes   []string
	commands  []string
	cmdStatus int
	histQuery string
}

func (f *fakeRegrabArr) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/history":
		f.histQuery = r.URL.Query().Get("downloadId")
		_, _ = io.WriteString(w, `{"records":`+f.history+`}`)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/blocklist":
		_, _ = io.WriteString(w, `{"records":`+f.blocklist+`}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v3/blocklist/"):
		f.deletes = append(f.deletes, strings.TrimPrefix(r.URL.Path, "/api/v3/blocklist/"))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
		b, _ := io.ReadAll(r.Body)
		f.commands = append(f.commands, string(b))
		if f.cmdStatus != 0 {
			w.WriteHeader(f.cmdStatus)
			return
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRegrab(t *testing.T) {
	const title = "Lindy Mercer S01 720p"
	sonarrHistory := `[
		{"eventType":"grabbed","downloadId":"` + strings.ToUpper(regrabHash) + `","seriesId":91,"episodeId":1001,"sourceTitle":"` + title + `"},
		{"eventType":"grabbed","downloadId":"` + strings.ToUpper(regrabHash) + `","seriesId":91,"episodeId":1002,"sourceTitle":"` + title + `"},
		{"eventType":"downloadFailed","downloadId":"` + strings.ToUpper(regrabHash) + `","seriesId":91,"episodeId":1001,"sourceTitle":"` + title + `"},
		{"eventType":"grabbed","downloadId":"OTHERHASH","seriesId":91,"episodeId":1003,"sourceTitle":"other"}]`
	cases := []struct {
		name        string
		kind        string
		history     string
		blocklist   string
		cmdStatus   int
		wantErr     error
		wantAnyErr  bool
		wantDeletes []string
		wantCmd     string
		wantSearch  []int
	}{
		{
			name: "sonarr: clears matching entry, searches every grabbed episode", kind: KindSonarr,
			history: sonarrHistory,
			blocklist: `[{"id":5,"sourceTitle":"` + title + `","seriesId":91},
				{"id":6,"sourceTitle":"` + title + `","seriesId":12},
				{"id":7,"sourceTitle":"Something else","seriesId":91}]`,
			wantDeletes: []string{"5"}, wantCmd: `"EpisodeSearch"`, wantSearch: []int{1001, 1002},
		},
		{
			name: "radarr: movie search", kind: KindRadarr,
			history:     `[{"eventType":"grabbed","downloadId":"` + regrabHash + `","movieId":201,"sourceTitle":"Spiral Arrow 2001"}]`,
			blocklist:   `[{"id":9,"sourceTitle":"Spiral Arrow 2001","movieId":201}]`,
			wantDeletes: []string{"9"}, wantCmd: `"MoviesSearch"`, wantSearch: []int{201},
		},
		{
			name: "no grab in history", kind: KindSonarr,
			history: `[{"eventType":"downloadFailed","downloadId":"` + regrabHash + `","seriesId":91,"episodeId":1001}]`,
			wantErr: ErrNoGrab,
		},
		{
			name: "nothing blocklisted still searches", kind: KindSonarr,
			history: sonarrHistory, blocklist: `[]`, wantCmd: `"EpisodeSearch"`, wantSearch: []int{1001, 1002},
		},
		{
			name: "search command fails", kind: KindSonarr,
			history: sonarrHistory, blocklist: `[]`, cmdStatus: http.StatusInternalServerError, wantAnyErr: true,
			wantCmd: `"EpisodeSearch"`, wantSearch: []int{1001, 1002},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRegrabArr{history: tc.history, blocklist: tc.blocklist, cmdStatus: tc.cmdStatus}
			if f.blocklist == "" {
				f.blocklist = "[]"
			}
			srv := httptest.NewServer(http.HandlerFunc(f.handler))
			defer srv.Close()
			c := New(tc.kind, srv.URL, "k", nil)

			res, err := c.Regrab(context.Background(), regrabHash)
			switch {
			case tc.wantErr != nil:
				require.True(t, errors.Is(err, tc.wantErr), "err = %v", err)
				assert.Empty(t, f.commands, "no search without a grab")
				return
			case tc.wantAnyErr:
				require.Error(t, err)
			default:
				require.NoError(t, err)
			}
			assert.Equal(t, strings.ToUpper(regrabHash), f.histQuery, "history is queried by the upper-cased hash")
			assert.Equal(t, tc.wantDeletes, f.deletes)
			assert.Equal(t, len(tc.wantDeletes), res.BlocklistCleared)
			assert.Equal(t, tc.wantSearch, res.Searched)
			require.Len(t, f.commands, 1)
			assert.Contains(t, f.commands[0], tc.wantCmd)
		})
	}
}
