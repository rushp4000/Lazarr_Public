package qbit_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/torbox"
)

// The GoT S01 regression (2026-10): TorBox cached a 10-episode season pack under ONE
// episode's file name. Lazarr reported that name, Sonarr parsed the download folder as
// S01E06 only, imported one episode and rejected nine (MatchesFolderSpecification).
// The folder must carry the release title the arr grabbed.
const gotHash = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"

func gotItem() torbox.CachedItem {
	files := []torbox.CachedFile{}
	for i := 1; i <= 10; i++ {
		files = append(files, torbox.CachedFile{ID: i,
			Name: "Game.of.Towers.S01.1080p.BluRay.x264-GRP/Game.of.Towers.S01E" + twoDigits(i) + ".1080p.BluRay.x264-GRP.mkv",
			Size: 2 << 30})
	}
	return torbox.CachedItem{Hash: gotHash, Name: "Game.of.Towers.S01E06.A.Gilded.Crown.1080p.BluRay.x264-GRP.mkv",
		Size: 20 << 30, Files: files}
}

func twoDigits(i int) string { return string(rune('0'+i/10)) + string(rune('0'+i%10)) }

func TestAdd_UsesArrReleaseName_GoTS01(t *testing.T) {
	e := newTestEnv(false)
	e.torbox.items = map[string]torbox.CachedItem{gotHash: gotItem()}
	arrTitle := "Game.of.Towers.S01.1080p.BluRay.x264-GRP"
	body, ct := formBody("urls", magnetURI(gotHash, arrTitle), "category", "sonarr_main")
	require.Equal(t, "Ok.", e.do("POST", "/api/v2/torrents/add", body, ct).Body.String())

	rel := e.store.releases[gotHash]
	require.NotNil(t, rel)
	assert.Equal(t, arrTitle, rel.Name, "download folder = the arr's release title")
	assert.Equal(t, "Game.of.Towers.S01E06.A.Gilded.Crown.1080p.BluRay.x264-GRP.mkv", rel.TorBoxName,
		"TorBox's name kept for diagnostics")

	rec := e.do("GET", "/api/v2/torrents/info?hashes="+gotHash, nil, "")
	var infos []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &infos))
	require.Len(t, infos, 1)
	assert.Equal(t, arrTitle, infos[0]["name"])
	assert.Equal(t, "/data/symlinks/sonarr_main/"+arrTitle, infos[0]["content_path"])
}

func TestAdd_ArrReleaseNameOffOrUnusable(t *testing.T) {
	// Policy off: TorBox's name, as before.
	e := newTestEnv(false)
	e.cfg.Policy.UseArrReleaseName = false
	e.torbox.items = map[string]torbox.CachedItem{gotHash: gotItem()}
	body, ct := formBody("urls", magnetURI(gotHash, "Game.of.Towers.S01.1080p.BluRay.x264-GRP"), "category", "sonarr_main")
	e.do("POST", "/api/v2/torrents/add", body, ct)
	assert.Equal(t, gotItem().Name, e.store.releases[gotHash].Name)

	// Unusable dn (a path, or just the hash): TorBox's name.
	for _, dn := range []string{"../../etc", "Show/S01", gotHash, "   ", "1234"} {
		e := newTestEnv(false)
		e.torbox.items = map[string]torbox.CachedItem{gotHash: gotItem()}
		body, ct := formBody("urls", magnetURI(gotHash, dn), "category", "sonarr_main")
		e.do("POST", "/api/v2/torrents/add", body, ct)
		assert.Equal(t, gotItem().Name, e.store.releases[gotHash].Name, "dn %q", dn)
	}
}
