// Package localcache is Workstream G: a watch-aware local disk cache in front of
// the TorBox pipe (docs/28). It wraps the materialize engine as a vfs.Materializer,
// pulls whole files to a local twin of the FUSE tree, atomically retargets the
// library symlinks the arrs created, and evicts by a disk-aware, per-user policy.
//
// Provider-agnostic by construction: it never imports torbox; it only sees bytes
// coming out of an Inner engine. With config local_cache.enabled=false, New returns
// a pass-through and nothing here ever runs.
package localcache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/playback"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/playback/jellyfin"
	"github.com/your-username/lazarr/internal/playback/plex"
)

// Inner is what the cache needs from the materialize engine (consumer-defined).
// *materialize.materializer satisfies it.
type Inner interface {
	ReadAt(hash string, fileID int, p []byte, off int64) (int, error)
	ReadAtCtx(ctx context.Context, hash string, fileID int, p []byte, off int64) (int, error)
	Release(hash string) error
	Throttled() bool
}

// Deps wires the engine (built in main).
type Deps struct {
	Store      catalog.Store
	Inner      Inner
	Cache      config.LocalCache
	Playback   config.Playback
	Paths      config.Paths
	Ownership  config.Ownership
	Categories []string
	Sources    []playback.Source
	Arrs       map[string]*arr.Client // by category; may be nil
	Logger     *slog.Logger
	// MountHealthy, when set, gates the reaper like the materialize reapers: a broken
	// FUSE mount means readlinks are lying, so nothing is evicted or retargeted.
	MountHealthy func() bool
	// PublishFn overrides the bind-mount (tests). nil = real publish on linux.
	PublishFn func(dir, at string) error
}

type fileKey struct {
	hash   string
	fileID int
}

// fileState is the in-memory mirror of one local_file row plus its rel_path.
type fileState struct {
	catalog.LocalFile
	rel string
}

type pullReq struct {
	key    fileKey
	rel    string
	size   int64
	reason string
	name   string
}

// Engine is the local cache. It satisfies vfs.Materializer.
type Engine struct {
	cfg       config.LocalCache
	pb        config.Playback
	store     catalog.Store
	inner     Inner
	disk      *diskStore
	links     *linkIndex
	resolver  *playback.Resolver
	sources   []playback.Source
	arrs      map[string]*arr.Client
	log       *slog.Logger
	now       func() time.Time
	mountOK   func() bool
	publish   func(dir, at string) error
	publishAt string
	roots     []string
	movieCat  map[string]bool

	mu       sync.Mutex
	files    map[fileKey]*fileState
	wanted   map[fileKey]struct{}
	lastPoll time.Time
	lastWalk time.Time
	linksN   int
	// unrefSince is when each cached file was first seen by a sweep with no library
	// link: the start of its unreferenced_grace. Rebuilt every sweep; a file that is
	// linked again drops out. In memory only — a restart restarts the grace (safe).
	unrefSince map[fileKey]time.Time
	sessions   []playback.Session
	itemIDs    map[fileKey]map[string]string // server -> item id (for refresh_on_retarget)
	obsDate    map[string]time.Time          // hash -> release date seen in a session
	queue      []pullReq
	queued     map[fileKey]bool
	pulling    map[fileKey]*pullProgress
	pullLog    []time.Time // start times, for the hourly budget
	specLog    []specPull  // speculative pulls in the last 24 h (speculative_gb_per_day)
	// seasonFilled remembers (server|user|series|season) fills already queued.
	seasonFilled map[string]time.Time
	meta         map[string]*releaseMeta

	wake chan struct{}

	// Post-pull analysis (analysis.go): servers that can analyze on demand, the row
	// store, and a deduped single-worker queue.
	analyzers   []playback.Analyzer
	astore      catalog.AnalysisStore
	aq          []fileKey
	aqueued     map[fileKey]bool
	awake       chan struct{}
	downloadDir string

	// statDisk reports the cache volume's headroom (statfs on Dir; tests override it).
	statDisk func() (DiskStat, error)
	// sweepMu serialises retention passes: the reaper's and the puller's make-room pass.
	sweepMu sync.Mutex
	// stuckWarnAt rate-limits the "only wanted/pinned files left" warning.
	stuckWarnAt time.Time

	// noLink is set when the publish bind-mount failed: links are never pointed into
	// the cache tree, but the walk still repairs links that already point there.
	noLink atomic.Bool

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closed    bool
}

type specPull struct {
	at    time.Time
	bytes int64
}

type pullProgress struct {
	req     pullReq
	started time.Time
	cancel  context.CancelFunc
}

// New builds the engine. It creates the cache dir and loads the in-memory index
// from the catalog but starts no goroutines and performs no mount — call Start.
func New(d Deps) (*Engine, error) {
	if d.Store == nil || d.Inner == nil {
		return nil, errors.New("localcache: nil Store or Inner")
	}
	if !d.Cache.Enabled {
		return nil, errors.New("localcache: disabled (main should not construct the engine)")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	ds, err := newDiskStore(d.Cache.Dir)
	if err != nil {
		return nil, err
	}
	cfgAll := config.Config{LocalCache: d.Cache, Paths: d.Paths}
	publishAt := cfgAll.EffectivePublishAt()
	roots := append(cfgAll.EffectiveLibraryRoots(), d.Paths.DownloadDir)
	res := playback.NewResolver(d.Paths.FuseMount, publishAt, d.Playback.PathMap)
	e := &Engine{
		cfg:        d.Cache,
		pb:         d.Playback,
		store:      d.Store,
		inner:      d.Inner,
		disk:       ds,
		resolver:   res,
		sources:    d.Sources,
		arrs:       d.Arrs,
		log:        log,
		now:        time.Now,
		mountOK:    d.MountHealthy,
		publish:    d.PublishFn,
		publishAt:  publishAt,
		roots:      roots,
		movieCat:   movieCategories(d.Categories, d.Playback.Arrs),
		files:      make(map[fileKey]*fileState),
		wanted:     make(map[fileKey]struct{}),
		unrefSince: make(map[fileKey]time.Time),
		itemIDs:    make(map[fileKey]map[string]string),
		obsDate:    make(map[string]time.Time),
		queued:     make(map[fileKey]bool),
		pulling:    make(map[fileKey]*pullProgress),
		meta:       make(map[string]*releaseMeta),

		seasonFilled: make(map[string]time.Time),
		wake:         make(chan struct{}, 1),

		aqueued:     make(map[fileKey]bool),
		awake:       make(chan struct{}, 1),
		downloadDir: d.Paths.DownloadDir,
	}
	for _, src := range d.Sources {
		if an, ok := src.(playback.Analyzer); ok {
			e.analyzers = append(e.analyzers, an)
		}
	}
	if as, ok := d.Store.(catalog.AnalysisStore); ok {
		e.astore = as
	}
	if e.publish == nil {
		e.publish = publish
	}
	e.statDisk = ds.disk
	e.links = &linkIndex{store: d.Store, resolver: res, roots: roots, downloadDir: d.Paths.DownloadDir, puid: d.Ownership.PUID, pgid: d.Ownership.PGID, now: func() time.Time { return e.now() }}
	if err := e.loadIndex(); err != nil {
		return nil, err
	}
	return e, nil
}

// movieCategories decides which categories hold movies: an explicit arr kind wins,
// else the name ("radarr"/"movie" → movie; "sonarr"/"tv" → episodes; unknown → movie).
func movieCategories(cats []string, arrs []config.Arr) map[string]bool {
	out := make(map[string]bool, len(cats))
	kind := map[string]string{}
	for _, a := range arrs {
		kind[a.Category] = a.EffectiveKind()
	}
	for _, c := range cats {
		lc := strings.ToLower(c)
		switch {
		case kind[c] == "radarr":
			out[c] = true
		case kind[c] == "sonarr":
			out[c] = false
		case strings.Contains(lc, "sonarr") || strings.Contains(lc, "tv"):
			out[c] = false
		default:
			out[c] = true
		}
	}
	return out
}

// loadIndex mirrors the local_file table into memory and reconciles it with disk:
// a complete row whose file is gone is dropped; a pulling/failed row keeps its
// frontier (the .part is resumable). Orphan hash dirs are removed by the reaper.
func (e *Engine) loadIndex() error {
	rows, err := e.store.ListLocalFiles("")
	if err != nil {
		return err
	}
	rels := map[string][]catalog.File{}
	for _, lf := range rows {
		files, ok := rels[lf.Hash]
		if !ok {
			_, files, err = e.store.GetRelease(lf.Hash)
			if errors.Is(err, catalog.ErrNotFound) {
				files = nil // release forgotten: the row goes; the boot walk repairs its links
			} else if err != nil {
				return fmt.Errorf("localcache: load index: %w", err) // never drop rows on a guess
			}
			rels[lf.Hash] = files
		}
		rel := ""
		for _, f := range files {
			if f.FileID == lf.FileID {
				rel = f.RelPath
			}
		}
		if rel == "" {
			_ = e.store.DeleteLocalFile(lf.Hash, lf.FileID)
			continue
		}
		if lf.State == catalog.LocalComplete {
			if size, ok := e.disk.stat(lf.Hash, rel); !ok || size != lf.Size {
				// The boot link walk (runReaper, before the first sweep) puts any library
				// link still pointing at this missing file back on FUSE.
				e.log.Warn("localcache: complete row without a matching file on disk — dropping; links go back to FUSE on the boot walk", "hash", short(lf.Hash), "file_id", lf.FileID)
				_ = e.store.DeleteLocalFile(lf.Hash, lf.FileID)
				continue
			}
		}
		e.files[fileKey{lf.Hash, lf.FileID}] = &fileState{LocalFile: *lf, rel: rel}
	}
	return nil
}

// Start publishes the cache tree, launches the puller workers, the playback poller,
// the link-index walker and the reaper. Idempotent.
func (e *Engine) Start(ctx context.Context) {
	e.startOnce.Do(func() {
		if e.cfg.Publish {
			if err := e.publish(e.disk.dir, e.publishAt); err != nil {
				e.log.Error("localcache: publish bind-mount failed — library links will NOT be retargeted until fixed", "err", err)
				e.noLink.Store(true) // never retarget into a tree peers cannot see; repairs still run
			} else {
				e.log.Info("localcache: cache tree published", "dir", e.disk.dir, "at", e.publishAt)
			}
		}
		rctx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		// Resume interrupted pulls.
		e.mu.Lock()
		for k, fs := range e.files {
			if fs.State == catalog.LocalPulling {
				e.queue = append(e.queue, pullReq{key: k, rel: fs.rel, size: fs.Size, reason: fs.Reason})
				e.queued[k] = true
			}
		}
		e.mu.Unlock()
		for i := 0; i < e.cfg.MaxConcurrentPulls; i++ {
			e.wg.Add(1)
			go func() { defer e.wg.Done(); e.runPuller(rctx) }()
		}
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.runPoller(rctx) }()
		e.wg.Add(1)
		go func() { defer e.wg.Done(); e.runReaper(rctx) }()
		if e.analysisEnabled() {
			e.resumeAnalysis()
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				e.preflightAnalysis(rctx)
				e.runAnalyzer(rctx)
			}()
		}
	})
}

// Close stops every goroutine; in-flight pulls persist their frontier and resume on
// the next start. It does NOT unmount the published tree (docs/28 §4.4).
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		if e.cancel != nil {
			e.cancel()
		}
		e.wg.Wait()
	})
	return nil
}

// Release implements vfs.Materializer by delegating to the inner engine (TorBox side).
func (e *Engine) Release(hash string) error { return e.inner.Release(hash) }

// Resolver exposes the path resolver (used by main for health/debug).
func (e *Engine) Resolver() *playback.Resolver { return e.resolver }

// BuildSources constructs the media-server sources and arr clients from config.
func BuildSources(pb config.Playback) ([]playback.Source, map[string]*arr.Client) {
	var srcs []playback.Source
	if pb.Plex.URL != "" && pb.Plex.Token != "" {
		srcs = append(srcs, plex.New(pb.Plex.URL, pb.Plex.Token, nil))
	}
	if pb.Jellyfin.URL != "" && pb.Jellyfin.APIKey != "" {
		srcs = append(srcs, jellyfin.New(pb.Jellyfin.URL, pb.Jellyfin.APIKey, nil))
	}
	arrs := map[string]*arr.Client{}
	for _, a := range pb.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		c := arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)
		for _, cat := range a.Categories() {
			arrs[cat] = c
		}
	}
	return srcs, arrs
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
