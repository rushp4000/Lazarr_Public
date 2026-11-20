// Package qbit emulates the qBittorrent WebUI API for the *arr suite. Endpoints, the
// torrents/info field set, and the "report complete from checkcached size" trick are in
// docs/03-arr-qbit-integration.md. Built by Agent Q (docs/09). Use the qbit-emu skill.
package qbit

import (
	"context"
	"net/http"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/symlink"
	"github.com/your-username/lazarr/internal/torbox"
)

// Deps are what the qBit server needs (wired in main).
type Deps struct {
	Config  *config.Config
	Store   catalog.Store
	TorBox  torbox.Client
	Symlink symlink.Manager
	// Engine releases an in-flight materialization on torrents/delete so a delete during
	// playback (upgrade-replace, Phase-4 import churn) frees the TorBox item + slot instead
	// of leaking it (S2). Optional and nil-safe: qbit tests that wire no engine pass nil.
	Engine Releaser
	// GrabHook, when set, is called after a successful grab (release stored, symlinks
	// created) so the local cache (docs/28 §7.3) can start an on-grab pull. Runs
	// synchronously on the request goroutine and must return quickly.
	GrabHook func(rel *catalog.Release, files []catalog.File)
	// ForgetHook, when set, is called on torrents/delete BEFORE the catalog row goes so
	// the local cache can retarget links back to FUSE and drop its files.
	ForgetHook func(hash string)
	// Arrs maps a category to the arr that owns it, for policy.wait_fail_blocklist:
	// a failed wait download is removed from that arr's queue as failed (blocklist +
	// re-search). Optional; a category with no entry keeps the plain error state.
	Arrs map[string]ArrFailer
	// Links finds the library symlinks (outside paths.download_dir) that still point at
	// a release. With policy.detach_on_delete, torrents/delete keeps the catalog row of
	// a release that has any, so the imported files keep playing. Optional: nil means
	// "no library links known" and every delete removes the row (pre-1.2.8 behavior).
	Links LinkFinder
	// SlowLane is the slow-lane queue (slow_lane.*, v1.2.9); nil disables the feature even
	// when the config enables it. Satisfied by the SQLite catalog store.
	SlowLane catalog.SlowLaneStore
	// SlowLaneArrs maps a category to its arr, so the slow lane can give an adopted wait
	// download back to the arr that already failed it (clear blocklist + re-search) and,
	// when the arr also implements SlowLaneImporter, run the automatic manual import.
	SlowLaneArrs map[string]SlowLaneArr
}

// LinkFinder reports the library links that reference a release. Satisfied by
// *libscan.Scanner. An error means "unknown": the caller must assume links exist.
type LinkFinder interface {
	LinksTo(ctx context.Context, hash string) ([]libscan.Link, error)
}

// ArrFailer is the slice of an arr client the wait pool needs: mark a download
// failed in the arr (blocklist the release, search for another). Satisfied by
// *arr.Client; returns arr.ErrNotInQueue when the arr's queue has no such item.
type ArrFailer interface {
	FailDownload(ctx context.Context, hash string) error
}

// Releaser is the slice of the materialize engine qbit needs: release a materialized
// release by infohash. Satisfied by *materialize.materializer via its Release method.
type Releaser interface {
	Release(hash string) error
}

// Server is the qBittorrent-emulation HTTP handler. New(Deps) (built by Agent Q)
// returns something satisfying http.Handler, mounting /api/v2/* per docs/03.
type Server interface {
	http.Handler
	// StartWaitPoller launches the on_cache_miss=wait background watcher (one cheap
	// store query per tick when idle). Call once from main; stops with ctx.
	StartWaitPoller(ctx context.Context)
	// SweepDetached deletes the rows of detached releases whose library links are all
	// gone (the arr upgraded or deleted the files). Returns how many it removed.
	SweepDetached(ctx context.Context) int
	// FailInArr asks the owning arr to fail (blocklist + re-search) a release another
	// component just marked errored, e.g. the materialize engine on dead-cache. No-op
	// unless policy.wait_fail_blocklist is on; never blocks on the arr.
	FailInArr(hash, reason string)

	// StartSlowLane launches the slow-lane worker (no-op unless slow_lane.enabled).
	StartSlowLane(ctx context.Context)
	// SlowLaneEnqueue queues a magnet link / infohash for a category (Web UI / API),
	// optionally for a target series/season or movie (nil = none).
	SlowLaneEnqueue(input, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error)
	// SlowLaneEnqueueTorrent queues an uploaded .torrent file for a category (Web UI /
	// API), optionally for a target (nil = none); private and v2-only torrents are refused.
	SlowLaneEnqueueTorrent(torrent []byte, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error)
	// SlowLaneCancel removes an active item (deleting its TorBox download first).
	SlowLaneCancel(hash string) error
	// SlowLaneList returns every slow-lane item, newest first.
	SlowLaneList() ([]*catalog.SlowLaneItem, error)
	// SlowLaneHeldIDs returns the TorBox ids of downloading slow-lane items (ToS audit).
	SlowLaneHeldIDs() ([]int64, error)
}
