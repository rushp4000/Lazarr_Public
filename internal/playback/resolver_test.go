package playback

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/config"
)

const h = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"

func TestResolveReadlinkJoin(t *testing.T) {
	root := t.TempDir()
	fuse := filepath.Join(root, "lazarr_mount")
	cache := filepath.Join(root, "lazarr_cache")
	lib := filepath.Join(root, "TV Shows", "sonarr_main", "Show")
	require.NoError(t, os.MkdirAll(lib, 0o755))

	r := NewResolver(fuse, cache, nil)

	fuseTarget := filepath.Join(fuse, h, "Show.S01E01/Show.S01E01.mkv")
	link := filepath.Join(lib, "Show - S01E01.mkv")
	require.NoError(t, os.Symlink(fuseTarget, link))

	ref, err := r.Resolve(link)
	require.NoError(t, err)
	assert.Equal(t, h, ref.Hash)
	assert.Equal(t, "Show.S01E01/Show.S01E01.mkv", ref.RelPath)
	assert.Equal(t, TargetFuse, ref.Kind)
	assert.Equal(t, link, ref.LinkPath)

	// Cache-side target resolves too, with the other kind.
	link2 := filepath.Join(lib, "Show - S01E02.mkv")
	require.NoError(t, os.Symlink(filepath.Join(cache, h, "e2.mkv"), link2))
	ref, err = r.Resolve(link2)
	require.NoError(t, err)
	assert.Equal(t, TargetCache, ref.Kind)
	assert.Equal(t, "e2.mkv", ref.RelPath)

	// A real file is not ours.
	real := filepath.Join(lib, "real.mkv")
	require.NoError(t, os.WriteFile(real, []byte("x"), 0o644))
	_, err = r.Resolve(real)
	assert.ErrorIs(t, err, ErrNotLazarr)

	// A symlink elsewhere (another tool's tree) is not ours.
	link3 := filepath.Join(lib, "other.mkv")
	require.NoError(t, os.Symlink(filepath.Join(root, "debrid_mount", "x.mkv"), link3))
	_, err = r.Resolve(link3)
	assert.ErrorIs(t, err, ErrNotLazarr)

	// Malformed hash under the fuse root is rejected.
	link4 := filepath.Join(lib, "bad.mkv")
	require.NoError(t, os.Symlink(filepath.Join(fuse, "..", "escape.mkv"), link4))
	_, err = r.Resolve(link4)
	assert.ErrorIs(t, err, ErrNotLazarr)

	// Missing path is an error but not ErrNotLazarr.
	_, err = r.Resolve(filepath.Join(lib, "missing.mkv"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotLazarr)

	// TargetFor builds the twin.
	assert.Equal(t, filepath.Join(cache, h, "a/b.mkv"), r.TargetFor(TargetCache, h, "a/b.mkv"))
	assert.Equal(t, filepath.Join(fuse, h, "a/b.mkv"), r.TargetFor(TargetFuse, h, "a/b.mkv"))
}

func TestParseTargetRejectsTraversal(t *testing.T) {
	r := NewResolver("/d/lazarr_mount", "/d/lazarr_cache", nil)
	cases := []string{
		"/d/lazarr_mount/" + h,             // no rel path
		"/d/lazarr_mount/ABC/x.mkv",        // bad hash
		"/d/lazarr_mount/" + h + "/../x",   // cleaned away → bad hash
		"/d/lazarr_mountx/" + h + "/x.mkv", // sibling prefix trick
		"/elsewhere/" + h + "/x.mkv",
	}
	for _, c := range cases {
		_, _, _, ok := r.ParseTarget(c)
		assert.False(t, ok, c)
	}
	kind, hash, rel, ok := r.ParseTarget("/d/lazarr_cache/" + h + "/Season 1/e.mkv")
	require.True(t, ok)
	assert.Equal(t, TargetCache, kind)
	assert.Equal(t, h, hash)
	assert.Equal(t, "Season 1/e.mkv", rel)
}

func TestMapPath(t *testing.T) {
	r := NewResolver("/d/lazarr_mount", "", []config.PathMap{{From: "/media", To: "/d"}})
	assert.Equal(t, "/d/TV/x.mkv", r.MapPath("/media/TV/x.mkv"))
	assert.Equal(t, "/other/x.mkv", r.MapPath("/other/x.mkv"))
	assert.Equal(t, "/mediax/x.mkv", r.MapPath("/mediax/x.mkv"), "prefix must match a whole component")
}

func TestSeriesOrder(t *testing.T) {
	s := Session{Season: 1, Episode: 13}
	assert.True(t, After(s, 2, 1))
	assert.True(t, After(s, 1, 14))
	assert.False(t, After(s, 1, 13))
	assert.False(t, After(s, 1, 2))
	assert.True(t, SeriesLess(2, 1, 0, 5), "specials sort last")
	assert.InDelta(t, 45.0, Session{Position: 45, Duration: 100}.ProgressPct(), 0.001)
	assert.Equal(t, 0.0, Session{Position: 45}.ProgressPct())
}
