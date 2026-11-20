package arrroots

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/your-username/lazarr/internal/playback/arr"
)

type fakeArr struct {
	roots    []string
	rootsErr error
	items    []arr.LibraryItem
	rescans  []int
	rootCall int
}

func (f *fakeArr) RootFolders(context.Context) ([]string, error) {
	f.rootCall++
	return f.roots, f.rootsErr
}
func (f *fakeArr) LibraryItems(context.Context) ([]arr.LibraryItem, error) { return f.items, nil }
func (f *fakeArr) Rescan(_ context.Context, id int) error {
	f.rescans = append(f.rescans, id)
	return nil
}

// captureLogs routes slog to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestMappingBothDirections(t *testing.T) {
	m := Mapping{HostRoot: "/debrid_symlinks/TV Shows/sonarr_main", ArrRoot: "/tv"}

	h, ok := m.ToHost("/tv/The Mender")
	require.True(t, ok)
	assert.Equal(t, "/debrid_symlinks/TV Shows/sonarr_main/The Mender", h)
	h, ok = m.ToHost("/tv/")
	require.True(t, ok)
	assert.Equal(t, "/debrid_symlinks/TV Shows/sonarr_main", h)
	_, ok = m.ToHost("/tvshows/X")
	assert.False(t, ok, "a prefix that is not a path element does not match")
	_, ok = m.ToHost("/movies/X")
	assert.False(t, ok)

	a, ok := m.ToArr("/debrid_symlinks/TV Shows/sonarr_main/The Mender/Season 2/S02E01.mkv")
	require.True(t, ok)
	assert.Equal(t, "/tv/The Mender/Season 2/S02E01.mkv", a)
	_, ok = m.ToArr("/debrid_symlinks/TV Shows/sonarr_main_old/X")
	assert.False(t, ok)
	_, ok = m.ToArr("/debrid_symlinks/TV Shows/sonarr_4k/X")
	assert.False(t, ok)

	// Round trip.
	back, ok := m.ToHost(a)
	require.True(t, ok)
	assert.Equal(t, "/debrid_symlinks/TV Shows/sonarr_main/The Mender/Season 2/S02E01.mkv", back)

	id := Mapping{HostRoot: "/data/tv", ArrRoot: "/data/tv"}
	a, ok = id.ToArr("/data/tv/X/e.mkv")
	require.True(t, ok)
	assert.Equal(t, "/data/tv/X/e.mkv", a, "same namespace = identity")
}

func TestConfiguredRoot(t *testing.T) {
	f := &fakeArr{roots: []string{"/tv/"}}
	r := &Resolver{Arrs: []Arr{{Category: "sonarr_main", Client: f, LibraryRoot: "/d/TV Shows/sonarr_main/"}}}
	m, err := r.Mapping(context.Background(), "sonarr_main")
	require.NoError(t, err)
	assert.Equal(t, Mapping{Category: "sonarr_main", HostRoot: "/d/TV Shows/sonarr_main", ArrRoot: "/tv"}, m)

	_, _ = r.Mapping(context.Background(), "sonarr_main")
	assert.Equal(t, 1, f.rootCall, "cached")

	// Several root folders: the one named like library_root.
	f = &fakeArr{roots: []string{"/tv", "/anime"}}
	r = &Resolver{Arrs: []Arr{{Category: "sonarr_main", Client: f, LibraryRoot: "/d/anime"}}}
	m, err = r.Mapping(context.Background(), "sonarr_main")
	require.NoError(t, err)
	assert.Equal(t, "/anime", m.ArrRoot)

	f = &fakeArr{roots: []string{"/tv", "/anime"}}
	r = &Resolver{Arrs: []Arr{{Category: "sonarr_main", Client: f, LibraryRoot: "/d/TV Shows/sonarr_main"}}}
	_, err = r.Mapping(context.Background(), "sonarr_main")
	assert.ErrorIs(t, err, ErrUnresolved)
}

// libTree builds a host library with two Sonarr roots that share some series names.
func libTree(t *testing.T) (base string, walk func(context.Context, func(string)) error) {
	t.Helper()
	base = t.TempDir()
	var links []string
	add := func(rel string) {
		p := filepath.Join(base, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, nil, 0o644))
		links = append(links, p)
	}
	add("TV Shows/sonarr_main/The Mender/Season 1/S01E01.mkv")
	add("TV Shows/sonarr_main/The Mender/Season 1/S01E02.mkv")
	add("TV Shows/sonarr_main/Charter House/Season 02/S02E01.mkv")
	add("TV Shows/sonarr_main/Solo Lantern/Season 1/S01E01.mkv")
	add("TV Shows/sonarr_4k/Solo Lantern/Season 1/S01E01.mkv")
	add("TV Shows/sonarr_4k/Envoys/Season 1/S01E01.mkv")
	return base, func(_ context.Context, fn func(string)) error {
		for _, l := range links {
			fn(l)
		}
		return nil
	}
}

func TestAutoDetect(t *testing.T) {
	base, walk := libTree(t)
	rd := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{
		{ID: 1, Path: "/tv/The Mender"}, {ID: 2, Path: "/tv/Charter House"}, {ID: 3, Path: "/tv/Solo Lantern"},
		{ID: 4, Path: "/tv/Pluribus"}, // no links yet
	}}
	r := &Resolver{Arrs: []Arr{{Category: "sonarr_main", Client: rd}}, Walk: walk}
	m, err := r.Mapping(context.Background(), "sonarr_main")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "TV Shows", "sonarr_main"), m.HostRoot,
		"3 series vote for sonarr_main, 1 (Solo Lantern) for sonarr_4k")
	assert.Equal(t, "/tv", m.ArrRoot)
	assert.True(t, m.Detected)
}

func TestAutoDetectAmbiguousOrClaimed(t *testing.T) {
	_, walk := libTree(t)
	// Only the shared series: one vote each = ambiguous.
	k4 := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{{ID: 9, Path: "/tv/Solo Lantern"}}}
	r := &Resolver{Arrs: []Arr{{Category: "sonarr_4k", Client: k4}}, Walk: walk}
	_, err := r.Mapping(context.Background(), "sonarr_4k")
	require.ErrorIs(t, err, ErrUnresolved)
	assert.Contains(t, err.Error(), "ambiguous")

	// The winner is another arr's configured root.
	base, walk := libTree(t)
	rd := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{{ID: 1, Path: "/tv/The Mender"}}}
	r = &Resolver{Walk: walk, Arrs: []Arr{
		{Category: "sonarr_x", Client: rd},
		{Category: "sonarr_main", Client: &fakeArr{roots: []string{"/tv"}}, LibraryRoot: filepath.Join(base, "TV Shows", "sonarr_main")},
	}}
	_, err = r.Mapping(context.Background(), "sonarr_x")
	require.ErrorIs(t, err, ErrUnresolved)
	assert.Contains(t, err.Error(), "sonarr_main's library_root")
}

// An arr that cannot be mapped logs one WARN (not one per call), its rescans are
// skipped, and it is retried after failTTL.
func TestUnresolvedWarnsOnce(t *testing.T) {
	logs := captureLogs(t)
	now := time.Unix(1_790_000_000, 0)
	f := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{{ID: 1, Path: "/tv/Nothing On Disk"}}}
	_, walk := libTree(t)
	r := &Resolver{Arrs: []Arr{{Category: "sonarr_hq", Client: f}}, Walk: walk, Now: func() time.Time { return now }}

	for range 3 {
		_, err := r.Mapping(context.Background(), "sonarr_hq")
		require.ErrorIs(t, err, ErrUnresolved)
	}
	r.Rescan(context.Background(), []string{"/anything/x.mkv"})
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"), logs.String())
	assert.Contains(t, logs.String(), "rescans after removed library links are disabled for this arr")
	assert.Contains(t, logs.String(), "arr=sonarr_hq")
	assert.Equal(t, 1, f.rootCall, "failure cached")
	assert.Empty(t, f.rescans)

	now = now.Add(failTTL + time.Minute)
	f.rootsErr = errors.New("down")
	_, err := r.Mapping(context.Background(), "sonarr_hq")
	require.Error(t, err)
	assert.Equal(t, 2, f.rootCall, "retried after failTTL")
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"), "still only one WARN")

	// Without auto-detection and without library_root.
	r = &Resolver{Arrs: []Arr{{Category: "radarr_main", Client: &fakeArr{roots: []string{"/movies"}}}}}
	_, err = r.Mapping(context.Background(), "radarr_main")
	assert.ErrorIs(t, err, ErrUnresolved)
}

// Rescan translates the arr's item folders to host paths, rescans the owner of each
// removed link once, and leaves arrs whose root does not contain the link alone.
func TestRescanTranslatesPaths(t *testing.T) {
	rd := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{
		{ID: 412, Path: "/tv/Charter House"}, {ID: 9, Path: "/tv/Charter"}, {ID: 3, Path: "/tv/Solo Lantern"},
	}}
	k4 := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{{ID: 77, Path: "/tv/Solo Lantern"}}}
	r := &Resolver{Arrs: []Arr{
		{Category: "sonarr_main", Client: rd, LibraryRoot: "/d/TV Shows/sonarr_main"},
		{Category: "sonarr_4k", Client: k4, LibraryRoot: "/d/TV Shows/sonarr_4k"},
	}}
	r.Rescan(context.Background(), []string{
		"/d/TV Shows/sonarr_main/Charter House/Season 02/S02E01.mkv",
		"/d/TV Shows/sonarr_main/Charter House/Season 02/S02E02.mkv",
		"/d/TV Shows/sonarr_main/Charter/S01E01.mkv",
		"/d/TV Shows/sonarr_4k/Solo Lantern/Season 1/S01E01.mkv",
		"/d/TV Shows/sonarr_main/Gone From Arr/S01E01.mkv",
	})
	assert.Equal(t, []int{412, 9}, rd.rescans)
	assert.Equal(t, []int{77}, k4.rescans, "the same series name in another arr is that arr's")
}

func TestTargetsCarryArrPathsAndRescanReportsAccepted(t *testing.T) {
	rd := &fakeArr{roots: []string{"/tv"}, items: []arr.LibraryItem{
		{ID: 412, Path: "/tv/Charter House"}, {ID: 9, Path: "/tv/Charter"},
	}}
	r := &Resolver{Arrs: []Arr{{Category: "sonarr_main", Client: rd, LibraryRoot: "/d/TV Shows/sonarr_main"}}}
	got := r.Targets(context.Background(), []string{
		"/d/TV Shows/sonarr_main/Charter House/Season 02/S02E01.mkv",
		"/d/TV Shows/sonarr_main/Charter House/Season 02/S02E02.mkv",
		"/d/TV Shows/sonarr_main/Charter/S01E01.mkv",
		"/d/TV Shows/other/x.mkv",
	})
	require.Len(t, got, 2)
	assert.Equal(t, Target{Category: "sonarr_main", ItemID: 412, ArrPaths: []string{
		"/tv/Charter House/Season 02/S02E01.mkv", "/tv/Charter House/Season 02/S02E02.mkv"}}, got[0])
	assert.Equal(t, Target{Category: "sonarr_main", ItemID: 9, ArrPaths: []string{"/tv/Charter/S01E01.mkv"}}, got[1])
	assert.Empty(t, rd.rescans, "Targets never rescans")

	ok := r.RescanTargets(context.Background(), append(got, Target{Category: "gone", ItemID: 1}))
	assert.Equal(t, got, ok, "only targets of a known arr whose rescan was accepted")
	assert.Equal(t, []int{412, 9}, rd.rescans)
}
