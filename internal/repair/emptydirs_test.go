package repair_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/repair"
)

// fakeLister is a ReleaseLister over a fixed slice (err fails every call).
type fakeLister struct {
	rels []*catalog.Release
	err  error
}

func (f *fakeLister) ListByCategory(cat string) ([]*catalog.Release, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*catalog.Release
	for _, r := range f.rels {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out, nil
}

var (
	edNow = time.Unix(1_790_000_000, 0)
	edOld = edNow.Add(-3 * time.Hour)
)

// mkdirs creates dirs (relative to base) and backdates every one of them to edOld.
func mkdirs(t *testing.T, base string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	backdate(t, base)
}

// backdate sets the mtime of every directory under base (not base itself) to edOld.
func backdate(t *testing.T, base string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != base {
			return os.Chtimes(p, edOld, edOld)
		}
		return nil
	}))
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err == nil {
		return true
	}
	require.True(t, errors.Is(err, os.ErrNotExist), "lstat %s: %v", p, err)
	return false
}

func newEmptyDirSweeper(dl string, lister repair.ReleaseLister, remove bool) *repair.EmptyDirSweeper {
	return &repair.EmptyDirSweeper{
		DownloadDir: dl,
		Categories:  []string{"sonarr_main", "radarr_hq"},
		Store:       lister,
		Remove:      remove,
		Grace:       time.Hour,
		Now:         func() time.Time { return edNow },
	}
}

func TestEmptyDirs_DownloadArea(t *testing.T) {
	dl := filepath.Join(t.TempDir(), "lazarr")
	cat := filepath.Join(dl, "sonarr_main")
	mkdirs(t, dl,
		"sonarr_main/Deleted.S01.1080p/Deleted.S01.1080p", // gone from the catalog: removed
		"sonarr_main/Detached.S01/Sub",                    // detached (hidden from the arr): removed
		"sonarr_main/Listed.S02/Inner",                    // still in the qBit list: protected
		"sonarr_main/Waiting.S03",                         // wait download (detached during cancel retry): protected
		"sonarr_main/Regrab.S04",                          // detached but re-added within grace: protected
		"sonarr_main/HasLink.S05/Season 5",                // holds a symlink: not empty
		"sonarr_main/HasFile.S06",                         // holds a regular file: not empty
		"sonarr_main/Fresh.S07/Inner",                     // modified within grace: left alone
		"radarr_hq",                                       // empty category folder: never removed
	)
	elsewhere := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.MkdirAll(elsewhere, 0o755))
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(cat, "HasLink.S05", "Season 5", "dirlink")))
	require.NoError(t, os.WriteFile(filepath.Join(cat, "HasFile.S06", "x.nfo"), []byte("x"), 0o644))
	backdate(t, dl)
	require.NoError(t, os.Chtimes(filepath.Join(cat, "Fresh.S07", "Inner"), edNow.Add(-time.Minute), edNow.Add(-time.Minute)))

	lister := &fakeLister{rels: []*catalog.Release{
		{Hash: "a", Category: "sonarr_main", Name: "Detached.S01", State: catalog.StateVirtual, DetachedAt: 1, AddedOn: edOld.Unix()},
		{Hash: "b", Category: "sonarr_main", Name: "Listed.S02", State: catalog.StateVirtual, AddedOn: edOld.Unix()},
		{Hash: "c", Category: "sonarr_main", Name: "Waiting.S03", State: catalog.StateDownloading, DetachedAt: 1, AddedOn: edOld.Unix()},
		{Hash: "d", Category: "sonarr_main", Name: "Regrab.S04", State: catalog.StateVirtual, DetachedAt: 1, AddedOn: edNow.Add(-time.Minute).Unix()},
	}}
	s := newEmptyDirSweeper(dl, lister, true)
	rep, err := s.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, 4, rep.Download.Found, "Deleted + its inner folder, Detached + Sub")
	assert.Equal(t, 4, rep.Download.Removed)
	assert.Zero(t, rep.Download.Failed)
	assert.ElementsMatch(t, []string{filepath.Join(cat, "Deleted.S01.1080p"), filepath.Join(cat, "Detached.S01")}, rep.Download.Sample)
	assert.False(t, exists(t, filepath.Join(cat, "Deleted.S01.1080p")))
	assert.False(t, exists(t, filepath.Join(cat, "Detached.S01")))
	for _, keep := range []string{"Listed.S02/Inner", "Waiting.S03", "Regrab.S04", "HasLink.S05/Season 5/dirlink", "HasFile.S06/x.nfo", "Fresh.S07/Inner"} {
		assert.True(t, exists(t, filepath.Join(cat, keep)), keep)
	}
	assert.True(t, exists(t, cat), "category folder is never removed")
	assert.True(t, exists(t, filepath.Join(dl, "radarr_hq")), "empty category folder is never removed")
	assert.True(t, exists(t, elsewhere), "symlink target is never touched")
	assert.Equal(t, rep, s.Last())
}

func TestEmptyDirs_ReportOnlyRemovesNothing(t *testing.T) {
	dl := filepath.Join(t.TempDir(), "lazarr")
	mkdirs(t, dl, "sonarr_main/Gone.S01/Inner")
	s := newEmptyDirSweeper(dl, &fakeLister{}, false)
	rep, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.False(t, rep.RemoveMode)
	assert.Equal(t, 2, rep.Download.Found)
	assert.Zero(t, rep.Download.Removed)
	assert.True(t, exists(t, filepath.Join(dl, "sonarr_main", "Gone.S01", "Inner")))
}

func TestEmptyDirs_GraceCountsTheWholeSubtree(t *testing.T) {
	dl := filepath.Join(t.TempDir(), "lazarr")
	mkdirs(t, dl, "sonarr_main/Rel/a/b")
	deep := filepath.Join(dl, "sonarr_main", "Rel", "a", "b")
	require.NoError(t, os.Chtimes(deep, edNow, edNow))
	s := newEmptyDirSweeper(dl, &fakeLister{}, true)
	rep, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.Zero(t, rep.Download.Found, "a fresh folder anywhere below keeps the release folder")
	assert.True(t, exists(t, deep))

	s.Now = func() time.Time { return edNow.Add(2 * time.Hour) }
	rep, err = s.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, rep.Download.Removed)
	assert.False(t, exists(t, filepath.Join(dl, "sonarr_main", "Rel")))
}

func TestEmptyDirs_CatalogErrorSkipsCategory(t *testing.T) {
	dl := filepath.Join(t.TempDir(), "lazarr")
	mkdirs(t, dl, "sonarr_main/Gone.S01")
	s := newEmptyDirSweeper(dl, &fakeLister{err: errors.New("db locked")}, true)
	_, err := s.Run(context.Background())
	require.Error(t, err)
	assert.True(t, exists(t, filepath.Join(dl, "sonarr_main", "Gone.S01")), "unknown = keep")
}

func TestEmptyDirs_SymlinkedRootNotFollowed(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	mkdirs(t, base, "real/Gone.S01")
	dl := filepath.Join(base, "lazarr")
	require.NoError(t, os.MkdirAll(dl, 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(dl, "sonarr_main")))
	s := newEmptyDirSweeper(dl, &fakeLister{}, true)
	rep, err := s.Run(context.Background())
	require.NoError(t, err)
	assert.Zero(t, rep.Download.Found)
	assert.True(t, exists(t, filepath.Join(real, "Gone.S01")))
}

// libEnv is a library tree: two arr root folders, a staging folder outside them, a
// FUSE mount inside the shared tree and an empty root folder.
type libEnv struct {
	base, tv, movies, hin, fuse, staging string
	sweeper                              *repair.EmptyDirSweeper
}

func newLibEnv(t *testing.T, remove bool) *libEnv {
	t.Helper()
	base := t.TempDir()
	e := &libEnv{
		base:    base,
		tv:      filepath.Join(base, "TV Shows", "sonarr_main"),
		movies:  filepath.Join(base, "Movies", "radarr_4k"),
		hin:     filepath.Join(base, "Movies", "radarr_hq"),
		fuse:    filepath.Join(base, "TV Shows", "sonarr_main", "lazarr_mount"),
		staging: filepath.Join(base, "staging", "sonarr_main"),
	}
	mkdirs(t, base,
		"TV Shows/sonarr_main/The Mender/Season 1",
		"TV Shows/sonarr_main/The Mender/Season 2",
		"TV Shows/sonarr_main/Envoys/Season 1",
		"TV Shows/sonarr_main/Has Subs/Season 1",
		"TV Shows/sonarr_main/lazarr_mount/hash/empty",
		"Movies/radarr_4k/Saboteurs (2012)",
		"Movies/radarr_hq",
		"staging/sonarr_main/leftover",
		"lazarr/sonarr_main/Gone.S01",
	)
	require.NoError(t, os.Symlink("/nowhere/e1.mkv", filepath.Join(e.tv, "The Mender", "Season 1", "S01E01.mkv")))
	require.NoError(t, os.WriteFile(filepath.Join(e.tv, "Has Subs", "Season 1", "S01E01.en.srt"), []byte("x"), 0o644))
	backdate(t, base)
	e.sweeper = &repair.EmptyDirSweeper{
		DownloadDir:        filepath.Join(base, "lazarr"),
		Store:              &fakeLister{},
		LibraryRootFolders: []string{e.tv, e.movies, e.hin},
		SkipDirs:           []string{e.fuse},
		Remove:             remove,
		SweepLibrary:       true,
		Grace:              time.Hour,
		Now:                func() time.Time { return edNow },
	}
	return e
}

func TestEmptyDirs_LibrarySweep(t *testing.T) {
	e := newLibEnv(t, true)
	rep, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)

	assert.False(t, exists(t, filepath.Join(e.tv, "The Mender", "Season 2")), "empty season removed")
	assert.True(t, exists(t, filepath.Join(e.tv, "The Mender", "Season 1", "S01E01.mkv")), "dangling symlink is content")
	assert.False(t, exists(t, filepath.Join(e.tv, "Envoys")), "empty series removed")
	assert.True(t, exists(t, filepath.Join(e.tv, "Has Subs", "Season 1", "S01E01.en.srt")), "non-video leftovers keep the folder")
	assert.False(t, exists(t, filepath.Join(e.movies, "Saboteurs (2012)")))
	assert.True(t, exists(t, filepath.Join(e.fuse, "hash", "empty")), "skip dirs are never entered")
	for _, root := range []string{e.tv, e.movies, e.hin} {
		assert.True(t, exists(t, root), "library root folder is never removed: %s", root)
	}
	assert.True(t, exists(t, filepath.Join(e.staging, "leftover")), "nothing outside the root folders is touched")
	assert.True(t, exists(t, filepath.Join(e.base, "lazarr", "sonarr_main", "Gone.S01")), "download dir is not part of the library walk")
	assert.Equal(t, 4, rep.Library.Removed, "Season 2, Envoys, Envoys/Season 1, Saboteurs")
}

func TestEmptyDirs_LibrarySweepOff(t *testing.T) {
	e := newLibEnv(t, true)
	e.sweeper.SweepLibrary = false
	rep, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)
	assert.Zero(t, rep.Library.Found)
	assert.True(t, exists(t, filepath.Join(e.tv, "Envoys", "Season 1")))
}

func TestEmptyDirs_PruneEmptied(t *testing.T) {
	e := newLibEnv(t, true)
	// The repair sweep just removed links from these folders (fresh mtimes do not matter).
	s2 := filepath.Join(e.tv, "The Mender", "Season 2")
	require.NoError(t, os.Chtimes(s2, edNow, edNow))
	n := e.sweeper.PruneEmptied([]string{
		filepath.Join(s2, "S02E01.mkv"),
		filepath.Join(s2, "S02E02.mkv"),
		filepath.Join(e.tv, "Envoys", "Season 1", "S01E01.mkv"),
		filepath.Join(e.tv, "Has Subs", "Season 1", "S01E02.mkv"),
		filepath.Join(e.hin, "S.mkv"),                        // directly in a root folder
		filepath.Join(e.staging, "leftover", "x.mkv"),        // outside every root folder
		filepath.Join(e.fuse, "hash", "empty", "x.mkv"),      // inside a skip dir
		filepath.Join(e.movies, "Saboteurs (2012)", "S.mkv"), // emptied movie folder
		filepath.Join(e.movies, "Gone (2001)", "Gone.mkv"),   // folder already gone
	})
	assert.Equal(t, 4, n, "Season 2, Envoys/Season 1, Envoys, Saboteurs")
	assert.False(t, exists(t, s2))
	assert.True(t, exists(t, filepath.Join(e.tv, "The Mender", "Season 1")), "sibling with a link stays, so the series stays")
	assert.False(t, exists(t, filepath.Join(e.tv, "Envoys")))
	assert.True(t, exists(t, filepath.Join(e.tv, "Has Subs", "Season 1")), "an .srt keeps the folder")
	assert.False(t, exists(t, filepath.Join(e.movies, "Saboteurs (2012)")))
	for _, keep := range []string{e.tv, e.movies, e.hin, filepath.Join(e.staging, "leftover"), filepath.Join(e.fuse, "hash", "empty")} {
		assert.True(t, exists(t, keep), keep)
	}
}

func TestEmptyDirs_PruneEmptiedNeedsRemoveAndRoots(t *testing.T) {
	e := newLibEnv(t, false)
	link := filepath.Join(e.tv, "Envoys", "Season 1", "S01E01.mkv")
	assert.Zero(t, e.sweeper.PruneEmptied([]string{link}), "report-only never removes")
	e.sweeper.Remove = true
	e.sweeper.LibraryRootFolders = nil
	assert.Zero(t, e.sweeper.PruneEmptied([]string{link}), "no root folders configured = never")
	assert.True(t, exists(t, filepath.Dir(link)))
}

func TestEmptyDirs_PruneEmptiedDoesNotRemoveSymlinkedDir(t *testing.T) {
	e := newLibEnv(t, true)
	target := filepath.Join(e.base, "elsewhere")
	require.NoError(t, os.MkdirAll(target, 0o755))
	ln := filepath.Join(e.tv, "Linked")
	require.NoError(t, os.Symlink(target, ln))
	assert.Zero(t, e.sweeper.PruneEmptied([]string{filepath.Join(ln, "x.mkv")}))
	assert.True(t, exists(t, ln), "a symlink to a folder is never removed")
	assert.True(t, exists(t, target), "nor its target")
}

func TestDangling_PrunesEmptiedFoldersBeforeRescan(t *testing.T) {
	e := newDanglingEnv(t, true)
	var pruned []string
	e.sweeper.PruneEmptied = func(links []string) int {
		assert.Empty(t, e.rescanned, "prune runs before the rescan")
		pruned = append(pruned, links...)
		return 1
	}
	_, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)
	e.now = e.now.Add(61 * time.Minute)
	rep, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)
	assert.Len(t, pruned, 2)
	assert.Equal(t, 1, rep.DirsRemoved)
}

// Two links in sibling seasons: the series folder goes once the last season is gone,
// whichever link comes first.
func TestEmptyDirs_PruneEmptiedSiblingSeasons(t *testing.T) {
	e := newLibEnv(t, true)
	mkdirs(t, e.tv, "Charter House/Season 01", "Charter House/Season 02")
	n := e.sweeper.PruneEmptied([]string{
		filepath.Join(e.tv, "Charter House", "Season 01", "S01E01.mkv"),
		filepath.Join(e.tv, "Charter House", "Season 02", "S02E01.mkv"),
	})
	assert.Equal(t, 3, n)
	assert.False(t, exists(t, filepath.Join(e.tv, "Charter House")))
	assert.True(t, exists(t, e.tv))
}

// SweepRoot can keep the hourly sweep off a root (e.g. an arr that recreates empty
// folders), and a root nested in another root is never removed by the outer walk.
func TestEmptyDirs_SweepRootFilterAndNestedRoot(t *testing.T) {
	e := newLibEnv(t, true)
	nested := filepath.Join(e.tv, "Kids")
	mkdirs(t, e.tv, "Kids")
	e.sweeper.LibraryRootFolders = append(e.sweeper.LibraryRootFolders, nested)
	e.sweeper.SweepRoot = func(_ context.Context, root string) bool { return root != e.movies }
	_, err := e.sweeper.Run(context.Background())
	require.NoError(t, err)
	assert.True(t, exists(t, nested), "an empty nested root folder is never removed")
	assert.True(t, exists(t, filepath.Join(e.movies, "Saboteurs (2012)")), "filtered root is not swept")
	assert.False(t, exists(t, filepath.Join(e.tv, "Envoys")))
}
