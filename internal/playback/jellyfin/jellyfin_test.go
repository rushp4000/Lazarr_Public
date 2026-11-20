package jellyfin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/playback"
)

// Shapes as observed live on Jellyfin 12.0.0 (docs/28 §3.1).
const sessionsJSON = `[
 {"Id":"sess1","UserId":"0d3c5b7a9e1f4a2b8c6d0e9f1a2b3c4d","UserName":"admin","Client":"Jellyfin Web",
  "NowPlayingItem":{"Id":"a7840000000000000000000000000001","Name":"An Heir Apparent","Type":"Episode",
    "SeriesName":"The Emissary","SeriesId":"5f0c2a7e9d1b4c3a8e6f2b1d0a9c8e7f","SeasonId":"7ba3",
    "ParentIndexNumber":2,"IndexNumber":1,
    "Path":"/debrid_symlinks/TV Shows/sonarr_main/The Emissary/Season 2/The Emissary - S02E01.mkv",
    "PremiereDate":"2024-11-22T05:00:00.0000000Z","RunTimeTicks":28679040000,
    "MediaSources":[{"Id":"a784","Path":"/debrid_symlinks/TV Shows/sonarr_main/The Emissary/Season 2/The Emissary - S02E01.mkv","Protocol":"File"}]},
  "PlayState":{"PositionTicks":14020590000,"IsPaused":true,"MediaSourceId":"a784"}},
 {"Id":"idle","UserId":"u2","UserName":"kid"},
 {"Id":"music","UserId":"u3","UserName":"x","NowPlayingItem":{"Id":"m","Type":"Audio","Name":"song"}}
]`

const episodesJSON = `{"Items":[
 {"Id":"e0","Type":"Episode","Name":"Special","ParentIndexNumber":0,"IndexNumber":1,"Path":"/lib/special.mkv"},
 {"Id":"e1","Type":"Episode","Name":"S2E1","ParentIndexNumber":2,"IndexNumber":1,"Path":"/lib/s02e01.mkv"},
 {"Id":"e2","Type":"Episode","Name":"S2E2","ParentIndexNumber":2,"IndexNumber":2,"Path":"/lib/s02e02.mkv"},
 {"Id":"e3","Type":"Episode","Name":"S3E1","ParentIndexNumber":3,"IndexNumber":1,"Path":"/lib/s03e01.mkv"},
 {"Id":"e4","Type":"Episode","Name":"S2E3","ParentIndexNumber":2,"IndexNumber":3,"MediaSources":[{"Path":"/lib/s02e03.mkv"}]}
],"TotalRecordCount":5}`

func newServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+"|auth="+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/Sessions":
			_, _ = w.Write([]byte(sessionsJSON))
		case r.URL.Path == "/Shows/5f0c2a7e9d1b4c3a8e6f2b1d0a9c8e7f/Episodes":
			_, _ = w.Write([]byte(episodesJSON))
		case r.URL.Path == "/Items/e1/Refresh" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestSessions(t *testing.T) {
	srv, seen := newServer(t)
	c := New(srv.URL, "jf-secret", nil)
	ss, err := c.Sessions(context.Background())
	require.NoError(t, err)
	require.Len(t, ss, 1, "idle and audio sessions are skipped")
	s := ss[0]
	assert.Equal(t, "jellyfin", s.Server)
	assert.Equal(t, playback.KindEpisode, s.Kind)
	assert.Equal(t, "0d3c5b7a9e1f4a2b8c6d0e9f1a2b3c4d", s.UserID)
	assert.Equal(t, "admin", s.UserName)
	assert.Equal(t, "5f0c2a7e9d1b4c3a8e6f2b1d0a9c8e7f", s.SeriesKey)
	assert.Equal(t, 2, s.Season)
	assert.Equal(t, 1, s.Episode)
	assert.Equal(t, playback.StatePaused, s.State)
	assert.Equal(t, int64(2867), int64(s.Duration.Seconds()))
	assert.Equal(t, int64(1402), int64(s.Position.Seconds()))
	assert.InDelta(t, 48.9, s.ProgressPct(), 0.1)
	assert.Equal(t, 2024, s.ReleaseDate.Year())
	assert.Contains(t, (*seen)[0], `Token="jf-secret"`)
	assert.Contains(t, (*seen)[0], "ActiveWithinSeconds=300")
}

func TestNextEpisodes(t *testing.T) {
	srv, seen := newServer(t)
	c := New(srv.URL, "k", nil)
	s := playback.Session{Kind: playback.KindEpisode, SeriesKey: "5f0c2a7e9d1b4c3a8e6f2b1d0a9c8e7f", UserID: "u1", Season: 2, Episode: 1}
	eps, err := c.NextEpisodes(context.Background(), s, 2)
	require.NoError(t, err)
	require.Len(t, eps, 2)
	assert.Equal(t, "/lib/s02e02.mkv", eps[0].Path)
	assert.Equal(t, "/lib/s02e03.mkv", eps[1].Path, "MediaSources path fallback")
	assert.Contains(t, (*seen)[0], "Fields=Path")
	assert.Contains(t, (*seen)[0], "UserId=u1")

	all, err := c.NextEpisodes(context.Background(), s, 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "/lib/s03e01.mkv", all[2].Path, "crosses the season boundary, skips specials")
}

func TestRefresh(t *testing.T) {
	srv, seen := newServer(t)
	c := New(srv.URL, "k", nil)
	require.NoError(t, c.Refresh(context.Background(), "e1"))
	assert.Contains(t, (*seen)[0], "POST /Items/e1/Refresh")
	require.Error(t, c.Refresh(context.Background(), "a/b"))
}
