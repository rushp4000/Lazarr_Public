package qbit_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/qbit"
)

// Workstream G: the grab hook fires after a successful add (release + symlinks in
// place) and the forget hook fires on delete before the catalog row goes.
func TestGrabAndForgetHooks(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.DownloadDir = "/data/symlinks"
	cfg.Paths.FuseMount = "/data/torbox"
	cfg.Categories = []string{"radarr_hq"}

	st := newFakeStore()
	var grabbed []string
	var grabbedFiles int
	var forgotten []string
	h := qbit.New(qbit.Deps{
		Config: cfg, Store: st, TorBox: &fakeTorBox{}, Symlink: &fakeSymlink{},
		GrabHook: func(rel *catalog.Release, files []catalog.File) {
			grabbed = append(grabbed, rel.Hash)
			grabbedFiles = len(files)
			assert.Equal(t, "radarr_hq", rel.Category)
			assert.True(t, rel.Cached)
		},
		ForgetHook: func(hash string) {
			forgotten = append(forgotten, hash)
			_, ok := st.releases[hash]
			assert.True(t, ok, "forget hook must run BEFORE the row is deleted")
		},
	})
	e := &testEnv{store: st, handler: h, cfg: cfg}

	body, ct := formBody("urls", magnetURI(cachedHash, "Big+Buck+Bunny"), "category", "radarr_hq")
	rec := e.do("POST", "/api/v2/torrents/add", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{cachedHash}, grabbed)
	assert.Greater(t, grabbedFiles, 0)

	body, ct = formBody("hashes", cachedHash)
	rec = e.do("POST", "/api/v2/torrents/delete", body, ct)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{cachedHash}, forgotten)
	assert.Contains(t, st.deleted, cachedHash)

	// An uncached / errored grab never fires the grab hook.
	grabbed = nil
	body, ct = formBody("urls", magnetURI(uncachedHash, "Nope"), "category", "radarr_hq")
	e.do("POST", "/api/v2/torrents/add", body, ct)
	assert.Empty(t, grabbed)
}
