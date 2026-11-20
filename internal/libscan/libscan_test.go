package libscan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/playback"
)

const (
	hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// tree is a typical deploy's layout under one temp root:
//
//	<root>/lazarr/<cat>/...      download_dir (Lazarr's own grab symlinks)
//	<root>/lazarr_mount/<hash>/  FUSE mount
//	<root>/lazarr_cache/<hash>/  local-cache publish tree
//	<root>/TV Shows/...          arr library
type tree struct {
	root, dl, fuse, cache, lib string
}

func newTree(t *testing.T) tree {
	t.Helper()
	root := t.TempDir()
	tr := tree{
		root:  root,
		dl:    filepath.Join(root, "lazarr"),
		fuse:  filepath.Join(root, "lazarr_mount"),
		cache: filepath.Join(root, "lazarr_cache"),
		lib:   filepath.Join(root, "TV Shows"),
	}
	for _, d := range []string{tr.dl, tr.fuse, tr.cache, tr.lib} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	return tr
}

func (tr tree) scanner(store catalog.Store) *libscan.Scanner {
	return &libscan.Scanner{
		Roots:       []string{tr.root},
		DownloadDir: tr.dl,
		Resolver:    playback.NewResolver(tr.fuse, tr.cache, nil),
		Store:       store,
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(target, link))
}

func TestLinksTo_CountsLibraryLinksOnly(t *testing.T) {
	tr := newTree(t)
	// Lazarr's own download-dir symlink for hashA: must NOT count.
	symlink(t, filepath.Join(tr.fuse, hashA, "Show.S01/e1.mkv"),
		filepath.Join(tr.dl, "sonarr_main", "Show.S01", "Show.S01/e1.mkv"))
	// Two library links for hashA (one FUSE, one cache), one for hashB.
	symlink(t, filepath.Join(tr.fuse, hashA, "Show.S01/e2.mkv"),
		filepath.Join(tr.lib, "Show", "Season 01", "Show - S01E02.mkv"))
	symlink(t, filepath.Join(tr.cache, hashA, "Show.S01/e3.mkv"),
		filepath.Join(tr.lib, "Show", "Season 01", "Show - S01E03.mkv"))
	symlink(t, filepath.Join(tr.fuse, hashB, "Movie.mkv"),
		filepath.Join(tr.root, "Movies", "Movie (2020)", "Movie.mkv"))
	// A non-Lazarr link (RD mount) and a regular file are ignored.
	symlink(t, "/mnt/remote/realdebrid/x.mkv", filepath.Join(tr.lib, "Other", "x.mkv"))
	require.NoError(t, os.WriteFile(filepath.Join(tr.lib, "Show", "notes.txt"), []byte("x"), 0o644))

	s := tr.scanner(nil)
	links, err := s.LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	require.Len(t, links, 2)
	kinds := map[string]bool{}
	for _, l := range links {
		assert.Equal(t, hashA, l.Hash)
		assert.False(t, l.Chained)
		kinds[l.Kind] = true
	}
	assert.True(t, kinds[playback.TargetFuse] && kinds[playback.TargetCache])

	links, err = s.LinksTo(context.Background(), hashB)
	require.NoError(t, err)
	assert.Len(t, links, 1)
}

// Only download_dir links exist (the arr has not imported yet, or removed the import):
// the release has no library links. This is the brief's caution: the link index also
// records the download-dir tree, which must never keep a row alive.
func TestLinksTo_DownloadDirOnlyIsUnlinked(t *testing.T) {
	tr := newTree(t)
	symlink(t, filepath.Join(tr.fuse, hashA, "Movie/m.mkv"),
		filepath.Join(tr.dl, "radarr_main", "Movie", "Movie/m.mkv"))
	links, err := tr.scanner(nil).LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	assert.Empty(t, links)
}

// The walk must never descend into the FUSE mount or the cache tree themselves.
func TestWalk_SkipsLazarrTrees(t *testing.T) {
	tr := newTree(t)
	// A symlink planted inside the FUSE dir that points at itself-ish: if the walk
	// descended, it would be counted.
	symlink(t, filepath.Join(tr.fuse, hashA, "f.mkv"), filepath.Join(tr.fuse, "nested", "loop.mkv"))
	symlink(t, filepath.Join(tr.fuse, hashA, "f.mkv"), filepath.Join(tr.cache, "nested", "loop.mkv"))
	links, err := tr.scanner(nil).LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestLinksTo_ChainedThroughDownloadDir(t *testing.T) {
	tr := newTree(t)
	dlLink := filepath.Join(tr.dl, "sonarr_main", "Show.S01", "e1.mkv")
	symlink(t, filepath.Join(tr.fuse, hashA, "e1.mkv"), dlLink)
	symlink(t, dlLink, filepath.Join(tr.lib, "Show", "S01E01.mkv"))
	links, err := tr.scanner(nil).LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.True(t, links[0].Chained)
}

func TestLinksTo_RelativeTarget(t *testing.T) {
	tr := newTree(t)
	link := filepath.Join(tr.lib, "Show", "S01E01.mkv")
	rel, err := filepath.Rel(filepath.Dir(link), filepath.Join(tr.fuse, hashA, "e1.mkv"))
	require.NoError(t, err)
	symlink(t, rel, link)
	links, err := tr.scanner(nil).LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	assert.Len(t, links, 1)
}

func TestLinksTo_CanceledContextIsAnError(t *testing.T) {
	tr := newTree(t)
	symlink(t, filepath.Join(tr.fuse, hashA, "e1.mkv"), filepath.Join(tr.lib, "S01E01.mkv"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tr.scanner(nil).LinksTo(ctx, hashA)
	assert.Error(t, err, "an interrupted walk must report an error (callers then keep the row)")
}

// Fast path: an index row outside download_dir that still resolves to the hash is
// trusted without a walk; download_dir rows and stale rows are ignored.
func TestLinksTo_IndexFastPath(t *testing.T) {
	tr := newTree(t)
	store, err := catalog.OpenSQLite(filepath.Join(t.TempDir(), "db.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.UpsertRelease(&catalog.Release{Hash: hashA, Name: "Show.S01", State: catalog.StateVirtual},
		[]catalog.File{{Hash: hashA, FileID: 1, RelPath: "e1.mkv", Size: 10}}))

	libLink := filepath.Join(tr.lib, "Show", "S01E01.mkv")
	symlink(t, filepath.Join(tr.fuse, hashA, "e1.mkv"), libLink)
	dlLink := filepath.Join(tr.dl, "sonarr_main", "Show.S01", "e1.mkv")
	symlink(t, filepath.Join(tr.fuse, hashA, "e1.mkv"), dlLink)
	for _, p := range []string{libLink, dlLink} {
		require.NoError(t, store.UpsertLibraryLink(&catalog.LibraryLink{LinkPath: p, Hash: hashA, FileID: 1, TargetKind: "fuse"}))
	}

	// Scanner with NO roots: only the index can find the link.
	s := tr.scanner(store)
	s.Roots = nil
	links, err := s.LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, libLink, links[0].Path)

	// The library link is gone: the stale index row is not trusted. With no roots the
	// walk cannot confirm that, so the answer is "unknown"; a complete walk says "none".
	require.NoError(t, os.Remove(libLink))
	_, err = s.LinksTo(context.Background(), hashA)
	require.ErrorIs(t, err, libscan.ErrIncompleteWalk)
	s.Roots = []string{tr.root}
	links, err = s.LinksTo(context.Background(), hashA)
	require.NoError(t, err)
	assert.Empty(t, links)
}

// "No links" must come from a complete walk: anything the walk had to skip makes the
// answer unknown (an error), so torrents/delete and the detached sweep keep the row.
// Walk itself (the dangling sweeper) stays lenient and reports what it could see.
func TestLinksTo_IncompleteWalkIsAnError(t *testing.T) {
	cases := []struct {
		name  string
		roots func(tr tree) []string
		setup func(t *testing.T, tr tree)
	}{
		{
			name:  "no roots configured",
			roots: func(tree) []string { return nil },
		},
		{
			name:  "only empty root entries",
			roots: func(tree) []string { return []string{""} },
		},
		{
			name:  "root missing (library not mounted)",
			roots: func(tr tree) []string { return []string{filepath.Join(tr.root, "gone")} },
		},
		{
			name:  "one of two roots missing",
			roots: func(tr tree) []string { return []string{tr.lib, filepath.Join(tr.root, "Movies")} },
		},
		{
			name:  "root is a file",
			roots: func(tr tree) []string { return []string{filepath.Join(tr.root, "file")} },
			setup: func(t *testing.T, tr tree) {
				require.NoError(t, os.WriteFile(filepath.Join(tr.root, "file"), nil, 0o600))
			},
		},
		{
			name:  "unreadable directory",
			roots: func(tr tree) []string { return []string{tr.lib} },
			setup: func(t *testing.T, tr tree) {
				if os.Geteuid() == 0 {
					t.Skip("root reads a 0o000 directory")
				}
				locked := filepath.Join(tr.lib, "Locked Show")
				require.NoError(t, os.MkdirAll(locked, 0o755))
				require.NoError(t, os.Chmod(locked, 0o000))
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) //nolint:gosec // test cleanup
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTree(t)
			if tc.setup != nil {
				tc.setup(t, tr)
			}
			s := tr.scanner(nil)
			s.Roots = tc.roots(tr)

			links, err := s.LinksTo(context.Background(), hashA)
			require.ErrorIs(t, err, libscan.ErrIncompleteWalk)
			assert.Empty(t, links)

			assert.NoError(t, s.Walk(context.Background(), func(libscan.Link) {}),
				"Walk stays lenient: the dangling sweeper reports what it could see")
		})
	}
}

// A link found before a skipped part of the library is still returned with the error;
// callers treat any error as "linked" anyway.
func TestLinksTo_IncompleteWalkStillReturnsLinksSeen(t *testing.T) {
	tr := newTree(t)
	symlink(t, filepath.Join(tr.fuse, hashA, "e1.mkv"), filepath.Join(tr.lib, "S01E01.mkv"))
	s := tr.scanner(nil)
	s.Roots = []string{tr.lib, filepath.Join(tr.root, "Movies")}
	links, err := s.LinksTo(context.Background(), hashA)
	require.ErrorIs(t, err, libscan.ErrIncompleteWalk)
	assert.Len(t, links, 1)
}
