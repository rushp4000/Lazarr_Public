package repair

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/materialize"
)

const (
	deadHash  = "dddddddddddddddddddddddddddddddddddddddd"
	aliveHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var now = time.Unix(1_790_100_000, 0)

func confirmed(hash string) materialize.RepairEntry {
	return materialize.RepairEntry{Hash: hash, State: catalog.StateError,
		PrevEvictedAt: now.Add(-24 * time.Hour).Unix()}
}

type tree struct {
	root, fuse, cache, download, library string
}

func newTree(t *testing.T) tree {
	t.Helper()
	root := t.TempDir()
	tr := tree{
		root:     root,
		fuse:     filepath.Join(root, "lazarr_mount"),
		cache:    filepath.Join(root, "lazarr_cache"),
		download: filepath.Join(root, "lazarr"),
		library:  filepath.Join(root, "TV Shows", "Show"),
	}
	for _, d := range []string{tr.fuse, tr.cache, tr.download, tr.library} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	return tr
}

func (tr tree) link(t *testing.T, at, target string) string {
	t.Helper()
	require.NoError(t, os.Symlink(target, at))
	return at
}

func (tr tree) unlinker() *Unlinker {
	return &Unlinker{
		Roots:      []string{tr.root},
		FuseMount:  tr.fuse,
		SkipDirs:   []string{tr.download, tr.cache},
		ConfirmGap: 12 * time.Hour,
		MaxPerRun:  50,
		Now:        func() time.Time { return now },
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// TestUnlinker_RemovesOnlyConfirmedDeadFuseLinks: a library link into the FUSE tree
// at a confirmed-dead hash is removed; links to live hashes, cache-tree links,
// Lazarr's own download-dir links and regular files are untouched.
func TestUnlinker_RemovesOnlyConfirmedDeadFuseLinks(t *testing.T) {
	tr := newTree(t)
	dead := tr.link(t, filepath.Join(tr.library, "S01E01.mkv"), filepath.Join(tr.fuse, deadHash, "rel", "e1.mkv"))
	alive := tr.link(t, filepath.Join(tr.library, "S01E02.mkv"), filepath.Join(tr.fuse, aliveHash, "e2.mkv"))
	cached := tr.link(t, filepath.Join(tr.library, "S01E03.mkv"), filepath.Join(tr.cache, deadHash, "e3.mkv"))
	own := tr.link(t, filepath.Join(tr.download, "e1.mkv"), filepath.Join(tr.fuse, deadHash, "rel", "e1.mkv"))
	regular := filepath.Join(tr.library, "S01E04.mkv")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0o644))

	res, err := tr.unlinker().Run(t.Context(), []materialize.RepairEntry{confirmed(deadHash)})
	require.NoError(t, err)
	assert.Equal(t, []string{dead}, res.Removed)
	assert.Equal(t, 1, res.Confirmed)
	assert.False(t, exists(dead))
	for _, p := range []string{alive, cached, own, regular} {
		assert.True(t, exists(p), "must keep %s", p)
	}
}

// TestUnlinker_RequiresConfirmation: a first sighting, a too-recent earlier sighting,
// or a release currently on the account must not be acted on.
func TestUnlinker_RequiresConfirmation(t *testing.T) {
	tr := newTree(t)
	l := tr.link(t, filepath.Join(tr.library, "S01E01.mkv"), filepath.Join(tr.fuse, deadHash, "e1.mkv"))
	u := tr.unlinker()

	cases := map[string]materialize.RepairEntry{
		"first sighting": {Hash: deadHash, State: catalog.StateError},
		"too recent": {Hash: deadHash, State: catalog.StateError,
			PrevEvictedAt: now.Add(-time.Hour).Unix()},
		"materialized": {Hash: deadHash, State: catalog.StateMaterialized,
			PrevEvictedAt: now.Add(-24 * time.Hour).Unix()},
		"downloading": {Hash: deadHash, State: catalog.StateDownloading,
			PrevEvictedAt: now.Add(-24 * time.Hour).Unix()},
	}
	for name, e := range cases {
		res, err := u.Run(t.Context(), []materialize.RepairEntry{e})
		require.NoError(t, err, name)
		assert.Empty(t, res.Removed, name)
		assert.True(t, exists(l), name)
	}
}

// TestUnlinker_CapsPerRun: beyond MaxPerRun the remaining dead releases wait for the
// next run.
func TestUnlinker_CapsPerRun(t *testing.T) {
	tr := newTree(t)
	tr.link(t, filepath.Join(tr.library, "a.mkv"), filepath.Join(tr.fuse, aliveHash, "a.mkv"))
	tr.link(t, filepath.Join(tr.library, "d.mkv"), filepath.Join(tr.fuse, deadHash, "d.mkv"))
	u := tr.unlinker()
	u.MaxPerRun = 1

	res, err := u.Run(t.Context(), []materialize.RepairEntry{confirmed(deadHash), confirmed(aliveHash)})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Confirmed)
	assert.Equal(t, 1, res.Deferred)
	assert.Len(t, res.Removed, 1, "sorted: only the first hash is processed")
	assert.False(t, exists(filepath.Join(tr.library, "a.mkv")))
	assert.True(t, exists(filepath.Join(tr.library, "d.mkv")))
}

// TestUnlinker_RelativeTarget: arrs may write relative symlinks.
func TestUnlinker_RelativeTarget(t *testing.T) {
	tr := newTree(t)
	rel, err := filepath.Rel(tr.library, filepath.Join(tr.fuse, deadHash, "e.mkv"))
	require.NoError(t, err)
	l := tr.link(t, filepath.Join(tr.library, "e.mkv"), rel)

	res, err := tr.unlinker().Run(t.Context(), []materialize.RepairEntry{confirmed(deadHash)})
	require.NoError(t, err)
	assert.Equal(t, []string{l}, res.Removed)
}
