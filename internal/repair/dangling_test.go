package repair_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/playback"
	"github.com/your-username/lazarr/internal/repair"
)

const (
	liveHash = "1111111111111111111111111111111111111111"
	goneHash = "2222222222222222222222222222222222222222"
	cachHash = "3333333333333333333333333333333333333333"
)

type danglingEnv struct {
	root, lib, fuse, cache string
	store                  catalog.Store
	sweeper                *repair.DanglingSweeper
	now                    time.Time
	rescanned              []string
}

func newDanglingEnv(t *testing.T, unlink bool) *danglingEnv {
	t.Helper()
	root := t.TempDir()
	e := &danglingEnv{root: root, lib: filepath.Join(root, "TV Shows"),
		fuse: filepath.Join(root, "lazarr_mount"), cache: filepath.Join(root, "lazarr_cache"),
		now: time.Unix(1_790_000_000, 0)}
	for _, d := range []string{e.lib, e.fuse, e.cache, filepath.Join(root, "lazarr")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	st, err := catalog.OpenSQLite(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	e.store = st
	// liveHash is a catalog release (detached rows count too); goneHash / cachHash are not.
	require.NoError(t, st.UpsertRelease(&catalog.Release{Hash: liveHash, State: catalog.StateVirtual, DetachedAt: 5}, nil))

	link := func(target, p string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.Symlink(target, p))
	}
	link(filepath.Join(e.fuse, liveHash, "e1.mkv"), filepath.Join(e.lib, "Live", "S01E01.mkv"))
	link(filepath.Join(e.fuse, goneHash, "e1.mkv"), filepath.Join(e.lib, "Charter House", "Season 02", "S02E01.mkv"))
	link(filepath.Join(e.fuse, goneHash, "e2.mkv"), filepath.Join(e.lib, "Charter House", "Season 02", "S02E02.mkv"))
	// A rowless cache link whose bytes still exist plays fine: not reported.
	require.NoError(t, os.MkdirAll(filepath.Join(e.cache, cachHash), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(e.cache, cachHash, "m.mkv"), []byte("x"), 0o644))
	link(filepath.Join(e.cache, cachHash, "m.mkv"), filepath.Join(root, "Movies", "M (2020)", "M.mkv"))

	e.sweeper = &repair.DanglingSweeper{
		Scanner: &libscan.Scanner{Roots: []string{root}, DownloadDir: filepath.Join(root, "lazarr"),
			Resolver: playback.NewResolver(e.fuse, e.cache, nil)},
		Store: st, Unlink: unlink, ConfirmGap: time.Hour, MaxHashesPerRun: 20,
		Rescan: func(_ context.Context, paths []string) { e.rescanned = append(e.rescanned, paths...) },
		Now:    func() time.Time { return e.now },
	}
	return e
}

func TestDangling_ReportOnly(t *testing.T) {
	e := newDanglingEnv(t, false)
	rep, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)
	require.Len(t, rep.Hashes, 1)
	assert.Equal(t, goneHash, rep.Hashes[0].Hash)
	assert.Equal(t, 2, rep.Links)
	assert.Zero(t, rep.Removed)
	_, err = os.Lstat(filepath.Join(e.lib, "Charter House", "Season 02", "S02E01.mkv"))
	assert.NoError(t, err, "report-only never removes")
	assert.Equal(t, rep, e.sweeper.Last())
}

func TestDangling_UnlinkAfterConfirmGapAndRescan(t *testing.T) {
	e := newDanglingEnv(t, true)
	rep, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)
	assert.Zero(t, rep.Removed, "first sighting: nothing removed")

	e.now = e.now.Add(61 * time.Minute)
	rep, err = e.sweeper.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Removed)
	assert.True(t, rep.Hashes[0].Unlinked)
	for _, p := range []string{"S02E01.mkv", "S02E02.mkv"} {
		_, err := os.Lstat(filepath.Join(e.lib, "Charter House", "Season 02", p))
		assert.True(t, os.IsNotExist(err), p)
	}
	_, err = os.Lstat(filepath.Join(e.lib, "Live", "S01E01.mkv"))
	assert.NoError(t, err, "a catalogued (even detached) release's link is never touched")
	sort.Strings(e.rescanned)
	assert.Len(t, e.rescanned, 2, "the arr is asked to rescan the owners of the removed links")

	rep, err = e.sweeper.Run(context.Background())
	require.NoError(t, err)
	assert.Empty(t, rep.Hashes)
}

func TestOwnerOf(t *testing.T) {
	folders := map[string]int{"/tv/Charter House": 412, "/tv/Charter": 9, "/tv/Other": 3}
	assert.Equal(t, 412, repair.OwnerOf("/tv/Charter House/Season 02/S02E01.mkv", folders))
	assert.Equal(t, 9, repair.OwnerOf("/tv/Charter/S01E01.mkv", folders))
	assert.Equal(t, -1, repair.OwnerOf("/tv/Charter Med/S01E01.mkv", folders))
}
