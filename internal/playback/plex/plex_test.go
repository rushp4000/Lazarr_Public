package plex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/playback"
)

const sessionsJSON = `{"MediaContainer":{"size":2,"Metadata":[
 {"ratingKey":"1234","type":"episode","title":"The Wheel","grandparentRatingKey":"100","grandparentTitle":"Map Makers",
  "parentRatingKey":"110","parentIndex":1,"index":13,"duration":2867904,"viewOffset":1402059,"sessionKey":"7",
  "originallyAvailableAt":"2007-10-18","year":2007,
  "Media":[{"Part":[{"file":"/debrid_symlinks/TV Shows/sonarr_main/Map Makers/Season 1/Map Makers - S01E13 - The Wheel.mkv","decision":"directplay"}]}],
  "User":{"id":1,"title":"owner"},"Player":{"state":"playing","machineIdentifier":"m1"}},
 {"ratingKey":"9","type":"track","title":"song","Media":[{"Part":[{"file":"/music/a.flac"}]}],"User":{"id":"2","title":"kid"},"Player":{"state":"paused"}}
]}}`

const sessionsXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1">
<Video ratingKey="55" type="movie" title="Sintel" duration="888000" viewOffset="100000" sessionKey="3" year="2010" originallyAvailableAt="2010-09-27">
 <Media><Part file="/debrid_symlinks/Movies/radarr_hq/Sintel (2010)/Sintel (2010).mkv"/></Media>
 <User id="1" title="owner"/><Player state="paused" machineIdentifier="m2"/>
</Video>
</MediaContainer>`

const leavesJSON = `{"MediaContainer":{"Metadata":[
 {"ratingKey":"1201","type":"episode","title":"S1E12","parentIndex":1,"index":12,"Media":[{"Part":[{"file":"/lib/s01e12.mkv"}]}]},
 {"ratingKey":"1234","type":"episode","title":"The Wheel","parentIndex":1,"index":13,"Media":[{"Part":[{"file":"/lib/s01e13.mkv"}]}]},
 {"ratingKey":"2002","type":"episode","title":"S2E2","parentIndex":2,"index":2,"Media":[{"Part":[{"file":"/lib/s02e02.mkv"}]}]},
 {"ratingKey":"2001","type":"episode","title":"S2E1","parentIndex":2,"index":1,"Media":[{"Part":[{"file":"/lib/s02e01.mkv"}]}]},
 {"ratingKey":"3","type":"episode","title":"Special","parentIndex":0,"index":1,"Media":[{"Part":[{"file":"/lib/special.mkv"}]}]},
 {"ratingKey":"2003","type":"episode","title":"S2E3","parentIndex":2,"index":3,"Media":[{"Part":[{"file":"/lib/s02e03.mkv"}]}]}
]}}`

func newServer(t *testing.T, xmlSessions bool) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+"|tok="+r.Header.Get("X-Plex-Token")+"|q="+r.URL.RawQuery)
		switch {
		case r.URL.Path == "/status/sessions" && xmlSessions:
			w.Header().Set("Content-Type", "text/xml;charset=utf-8")
			_, _ = w.Write([]byte(sessionsXML))
		case r.URL.Path == "/status/sessions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sessionsJSON))
		case r.URL.Path == "/library/metadata/100/allLeaves":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(leavesJSON))
		case r.URL.Path == "/library/metadata/1234/refresh" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestSessionsJSON(t *testing.T) {
	srv, seen := newServer(t, false)
	c := New(srv.URL, "tok-secret", nil)
	ss, err := c.Sessions(context.Background())
	require.NoError(t, err)
	require.Len(t, ss, 1, "the audio track session is skipped")
	s := ss[0]
	assert.Equal(t, "plex", s.Server)
	assert.Equal(t, playback.KindEpisode, s.Kind)
	assert.Equal(t, "1", s.UserID)
	assert.Equal(t, "owner", s.UserName)
	assert.Equal(t, "100", s.SeriesKey)
	assert.Equal(t, 1, s.Season)
	assert.Equal(t, 13, s.Episode)
	assert.Equal(t, "/debrid_symlinks/TV Shows/sonarr_main/Map Makers/Season 1/Map Makers - S01E13 - The Wheel.mkv", s.Path)
	assert.InDelta(t, 48.9, s.ProgressPct(), 0.1)
	assert.Equal(t, playback.StatePlaying, s.State)
	assert.Equal(t, 2007, s.ReleaseDate.Year())
	assert.Contains(t, (*seen)[0], "tok=tok-secret", "token travels as a header")
	assert.NotContains(t, (*seen)[0], "q=X-Plex-Token", "never in the query string")
}

func TestSessionsXMLFallback(t *testing.T) {
	srv, _ := newServer(t, true)
	c := New(srv.URL, "t", nil)
	ss, err := c.Sessions(context.Background())
	require.NoError(t, err)
	require.Len(t, ss, 1)
	assert.Equal(t, playback.KindMovie, ss[0].Kind)
	assert.Equal(t, playback.StatePaused, ss[0].State)
	assert.Equal(t, "55", ss[0].ItemID)
	assert.Equal(t, 2010, ss[0].Year)
	assert.InDelta(t, 11.26, ss[0].ProgressPct(), 0.1)
	assert.Equal(t, "/debrid_symlinks/Movies/radarr_hq/Sintel (2010)/Sintel (2010).mkv", ss[0].Path)
}

func TestNextEpisodesCrossesSeasonsAndSkipsSpecials(t *testing.T) {
	srv, _ := newServer(t, false)
	c := New(srv.URL, "t", nil)
	s := playback.Session{Kind: playback.KindEpisode, SeriesKey: "100", Season: 1, Episode: 13}
	eps, err := c.NextEpisodes(context.Background(), s, 2)
	require.NoError(t, err)
	require.Len(t, eps, 2)
	assert.Equal(t, "/lib/s02e01.mkv", eps[0].Path)
	assert.Equal(t, "/lib/s02e02.mkv", eps[1].Path)

	all, err := c.NextEpisodes(context.Background(), s, 10)
	require.NoError(t, err)
	assert.Len(t, all, 3, "specials never count as next")

	none, err := c.NextEpisodes(context.Background(), playback.Session{Kind: playback.KindMovie}, 2)
	require.NoError(t, err)
	assert.Nil(t, none)

	_, err = c.NextEpisodes(context.Background(), playback.Session{Kind: playback.KindEpisode, SeriesKey: "../x"}, 2)
	require.Error(t, err, "non-numeric keys are refused before any request")
}

func TestRefresh(t *testing.T) {
	srv, seen := newServer(t, false)
	c := New(srv.URL, "t", nil)
	require.NoError(t, c.Refresh(context.Background(), "1234"))
	assert.Contains(t, (*seen)[0], "PUT /library/metadata/1234/refresh")
	require.Error(t, c.Refresh(context.Background(), "x/y"))
}

func TestHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := New(srv.URL, "bad", nil)
	_, err := c.Sessions(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
	assert.NotContains(t, err.Error(), "bad", "token never appears in errors")
}
