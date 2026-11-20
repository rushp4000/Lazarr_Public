package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Source defaults to torrent both on upsert and on read of a legacy row.
func TestSourceDefaultsToTorrent(t *testing.T) {
	s := openTestDB(t)
	rel := sampleRelease(hashA)
	require.Empty(t, rel.Source)
	require.NoError(t, s.UpsertRelease(rel, sampleFiles(hashA)))
	assert.Equal(t, SourceTorrent, rel.Source, "upsert normalizes empty Source")

	got, _, err := s.GetRelease(hashA)
	require.NoError(t, err)
	assert.Equal(t, SourceTorrent, got.Source)

	// A legacy row written with '' (pre-Source binary) reads back as torrent too.
	_, err = s.(*sqliteStore).db.Exec(`UPDATE release SET source = '' WHERE hash = ?`, hashA)
	require.NoError(t, err)
	got, _, err = s.GetRelease(hashA)
	require.NoError(t, err)
	assert.Equal(t, SourceTorrent, got.Source)

	// Usenet round-trips.
	rel.Source = SourceUsenet
	require.NoError(t, s.UpsertRelease(rel, sampleFiles(hashA)))
	got, _, err = s.GetRelease(hashA)
	require.NoError(t, err)
	assert.Equal(t, SourceUsenet, got.Source)
}

func TestLocalFileCRUD(t *testing.T) {
	s := openTestDB(t)
	require.NoError(t, s.UpsertRelease(sampleRelease(hashA), sampleFiles(hashA)))

	_, err := s.GetLocalFile(hashA, 1)
	assert.ErrorIs(t, err, ErrNotFound)

	lf := &LocalFile{Hash: hashA, FileID: 1, Size: 1000000, Frontier: 4096, Reason: "watch"}
	require.NoError(t, s.UpsertLocalFile(lf))
	assert.Equal(t, LocalPulling, lf.State, "empty state defaults to pulling")

	got, err := s.GetLocalFile(hashA, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(4096), got.Frontier)
	assert.Equal(t, "watch", got.Reason)

	n, err := s.LocalBytes()
	require.NoError(t, err)
	assert.Equal(t, int64(4096), n, "pulling rows count their frontier")

	got.State = LocalComplete
	got.Frontier = got.Size
	got.Linked = true
	got.PulledAt = 5000
	require.NoError(t, s.UpsertLocalFile(got))
	n, err = s.LocalBytes()
	require.NoError(t, err)
	assert.Equal(t, int64(1000000), n, "complete rows count their size")

	all, err := s.ListLocalFiles("")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.True(t, all[0].Linked)
	comp, err := s.ListLocalFiles(LocalComplete)
	require.NoError(t, err)
	assert.Len(t, comp, 1)
	pull, err := s.ListLocalFiles(LocalPulling)
	require.NoError(t, err)
	assert.Empty(t, pull)
	byHash, err := s.ListLocalFilesByHash(hashA)
	require.NoError(t, err)
	assert.Len(t, byHash, 1)

	// ListReleases derives the counts and honours LocalOnly.
	rels, total, err := s.ListReleases(ReleaseFilter{LocalOnly: true})
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, rels, 1)
	assert.Equal(t, 1, rels[0].LocalFiles)
	assert.Equal(t, 0, rels[0].LocalPulling)

	require.NoError(t, s.DeleteLocalFile(hashA, 1))
	require.NoError(t, s.DeleteLocalFile(hashA, 1), "delete is idempotent")
	_, total, err = s.ListReleases(ReleaseFilter{LocalOnly: true})
	require.NoError(t, err)
	assert.Equal(t, 0, total)
}

func TestLocalFileCascadesWithRelease(t *testing.T) {
	s := openTestDB(t)
	require.NoError(t, s.UpsertRelease(sampleRelease(hashA), sampleFiles(hashA)))
	require.NoError(t, s.UpsertLocalFile(&LocalFile{Hash: hashA, FileID: 1, State: LocalComplete, Size: 10}))
	require.NoError(t, s.UpsertWatch(&Watch{Server: "plex", UserID: "1", Hash: hashA, FileID: 1, LastSeen: 1}))
	require.NoError(t, s.UpsertLibraryLink(&LibraryLink{LinkPath: "/lib/a.mkv", Hash: hashA, FileID: 1, TargetKind: "fuse", LastVerified: 1}))
	require.NoError(t, s.DeleteRelease(hashA))
	lfs, err := s.ListLocalFiles("")
	require.NoError(t, err)
	assert.Empty(t, lfs)
	ws, err := s.ListWatchSince(0)
	require.NoError(t, err)
	assert.Empty(t, ws)
	ls, err := s.ListAllLibraryLinks()
	require.NoError(t, err)
	assert.Empty(t, ls)
}

func TestWatchUpsertMerges(t *testing.T) {
	s := openTestDB(t)
	require.NoError(t, s.UpsertRelease(sampleRelease(hashA), sampleFiles(hashA)))

	w := &Watch{Server: "jellyfin", UserID: "u1", UserName: "admin", Hash: hashA, FileID: 1,
		SeriesKey: "s1", Season: 1, Episode: 3, ProgressPct: 40, FirstSeen: 100, LastSeen: 100}
	require.NoError(t, s.UpsertWatch(w))
	// Later observation: progress moved on, completed flips, last_seen advances.
	w.ProgressPct = 95
	w.Completed = true
	w.FirstSeen = 500 // must NOT overwrite the earlier first_seen
	w.LastSeen = 500
	require.NoError(t, s.UpsertWatch(w))
	// An older stale observation must not regress completed/last_seen.
	w.Completed = false
	w.LastSeen = 200
	require.NoError(t, s.UpsertWatch(w))

	rows, err := s.ListWatchByFile(hashA, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Completed)
	assert.Equal(t, int64(100), rows[0].FirstSeen)
	assert.Equal(t, int64(500), rows[0].LastSeen)
	assert.Equal(t, "admin", rows[0].UserName)

	// A second user is an independent row.
	require.NoError(t, s.UpsertWatch(&Watch{Server: "jellyfin", UserID: "u2", Hash: hashA, FileID: 1, LastSeen: 50}))
	rows, err = s.ListWatchByFile(hashA, 1)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	since, err := s.ListWatchSince(100)
	require.NoError(t, err)
	assert.Len(t, since, 1)
}

func TestLibraryLinkCRUD(t *testing.T) {
	s := openTestDB(t)
	require.NoError(t, s.UpsertRelease(sampleRelease(hashA), sampleFiles(hashA)))
	require.NoError(t, s.UpsertLibraryLink(&LibraryLink{LinkPath: "/lib/a.mkv", Hash: hashA, FileID: 1, TargetKind: "fuse", LastVerified: 10}))
	require.NoError(t, s.UpsertLibraryLink(&LibraryLink{LinkPath: "/lib/b.mkv", Hash: hashA, FileID: 1, TargetKind: "cache", LastVerified: 20}))
	require.NoError(t, s.UpsertLibraryLink(&LibraryLink{LinkPath: "/lib/a.mkv", Hash: hashA, FileID: 1, TargetKind: "cache", LastVerified: 30}))

	links, err := s.ListLibraryLinks(hashA, 1)
	require.NoError(t, err)
	require.Len(t, links, 2)
	assert.Equal(t, "cache", links[0].TargetKind)
	assert.Equal(t, int64(30), links[0].LastVerified)

	n, err := s.PruneLibraryLinks(25)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	all, err := s.ListAllLibraryLinks()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "/lib/a.mkv", all[0].LinkPath)

	require.NoError(t, s.DeleteLibraryLink("/lib/a.mkv"))
	require.NoError(t, s.DeleteLibraryLink("/lib/a.mkv"))
	all, err = s.ListAllLibraryLinks()
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestLocalAnalysisRoundTripAndCascade(t *testing.T) {
	s := openTestDB(t)
	as, ok := s.(AnalysisStore)
	require.True(t, ok, "the sqlite store implements AnalysisStore")
	require.NoError(t, s.UpsertRelease(sampleRelease(hashA), sampleFiles(hashA)))
	a := &LocalAnalysis{Hash: hashA, FileID: 1, Server: "plex", RequestedAt: 5}
	require.NoError(t, as.UpsertLocalAnalysis(a))
	assert.Equal(t, AnalysisPending, a.State, "empty state defaults to pending")
	active, err := as.ListActiveAnalysis()
	require.NoError(t, err)
	require.Len(t, active, 1)

	a.State, a.ItemIDs, a.Intro, a.Credits, a.FinishedAt = AnalysisDone, "7,8", 1, 2, 9
	require.NoError(t, as.UpsertLocalAnalysis(a))
	rows, err := as.ListLocalAnalysis(hashA, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, *a, *rows[0])
	active, err = as.ListActiveAnalysis()
	require.NoError(t, err)
	assert.Empty(t, active)

	require.NoError(t, s.DeleteRelease(hashA))
	rows, err = as.ListLocalAnalysis(hashA, 1)
	require.NoError(t, err)
	assert.Empty(t, rows, "rows cascade with the release")
}
