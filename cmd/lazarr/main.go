// Command lazarr is a self-hosted, ToS-compliant TorBox lazy-materialize shim that
// presents as a qBittorrent client to Sonarr/Radarr. See /root/Github/Lazarr/docs.
//
// Phase 1 wires catalog -> torbox -> symlink -> qbit and serves the qBittorrent
// WebUI emulation. At grab time it symlinks with NO TorBox add (the ToS-compliant
// core). Phase 2 (vfs/materialize) adds playback materialization + idle release.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/constants"
	"github.com/your-username/lazarr/internal/janitor"
	"github.com/your-username/lazarr/internal/libscan"
	"github.com/your-username/lazarr/internal/localcache"
	"github.com/your-username/lazarr/internal/logging"
	"github.com/your-username/lazarr/internal/materialize"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/missing"
	"github.com/your-username/lazarr/internal/playback"
	"github.com/your-username/lazarr/internal/playback/arr"
	"github.com/your-username/lazarr/internal/qbit"
	"github.com/your-username/lazarr/internal/repair"
	"github.com/your-username/lazarr/internal/symlink"
	"github.com/your-username/lazarr/internal/torbox"
	"github.com/your-username/lazarr/internal/version"
	"github.com/your-username/lazarr/internal/vfs"
	"github.com/your-username/lazarr/internal/webui"
)

// auditInterval is how often the ToS-audit loop diffs mylist vs the materialized
// set (docs/12 guardrail 2). Cheap (one mylist pull); 5m keeps the proof current.
const auditInterval = 5 * time.Minute

// danglingSweepEvery / danglingConfirmGap: the library walk is cheap (~60 ms for ~9k
// symlinks live), so report hourly; unlink only after two sightings an hour apart.
const (
	danglingSweepEvery = time.Hour
	danglingConfirmGap = time.Hour
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("lazarr " + version.Version)
		return
	}

	// Startup banner (printed before the structured logger is installed) so the running
	// build is unmistakable in `docker logs` — like other tools' boot banners. Operators
	// kept mistaking a stale image for a new one (the stack sat on an old tag through
	// three incidents), so the version is the first line every process prints.
	printBanner()

	// Logger: text to stdout + a bounded in-memory ring for the Web UI Logs tab.
	// The level is a LevelVar so the settings page can change verbosity live.
	levelVar := new(slog.LevelVar)
	levelVar.Set(slog.LevelInfo)
	logRing := logging.NewRing(logging.RingCapacity)
	base := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: levelVar})
	slog.SetDefault(slog.New(logging.NewHandler(base, logRing)))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "path", *cfgPath, "err", err)
		os.Exit(1)
	}

	// config log_level first, then the LAZARR_LOG_LEVEL env var as an override
	// (kept for backwards compatibility with existing deployments).
	if lv, lerr := logging.ParseLevel(cfg.LogLevel); lerr == nil {
		levelVar.Set(lv)
	}
	if v := os.Getenv("LAZARR_LOG_LEVEL"); v != "" {
		if lv, lerr := logging.ParseLevel(v); lerr == nil {
			levelVar.Set(lv)
		} else {
			slog.Warn("ignoring invalid LAZARR_LOG_LEVEL", "value", v)
		}
	}

	slog.Info("lazarr starting",
		"version", version.Version,
		"qbit", cfg.QBit.Listen, "fuse", cfg.Paths.FuseMount,
		"download_dir", cfg.Paths.DownloadDir, "db", cfg.Paths.DBPath,
		"slots", cfg.Policy.ActiveSlots, "uncached", cfg.Policy.AllowUncached,
		"categories", cfg.Categories)

	// catalog -> torbox -> symlink -> qbit (Phase-1 wiring order).
	store, err := catalog.OpenSQLite(cfg.Paths.DBPath)
	if err != nil {
		slog.Error("open catalog", "db", cfg.Paths.DBPath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	startTime := time.Now()
	tb := torbox.New(cfg.TorBox)

	// Best-effort connectivity/slot check; non-fatal so lazarr still boots if
	// TorBox is briefly unreachable. Never logs the API key.
	var cachedAccount *torbox.Account
	if acct, err := tb.UserMe(); err != nil {
		slog.Warn("torbox user/me check failed (continuing)", "err", err)
	} else {
		cachedAccount = acct
		slog.Info("torbox account", "plan", acct.Plan, "active_slots", acct.ActiveSlots,
			"long_term_storage", acct.LongTermStore, "cooldown_until", acct.CooldownUntil)
	}

	sym := symlink.New(cfg.Paths, cfg.Ownership)

	// Phase 2: the lazy playback path — materialize engine, FUSE mount, idle + max-hold
	// reapers, and the ToS-audit loop. The engine is constructed BEFORE the qbit server and
	// the mount so it can be wired into both (qbit releases on delete; the mount drives
	// reads), and so SetMountHealthy lands BEFORE Start (S1: no data race on mountHealthy).
	// Slow lane (v1.2.9): the SQLite catalog implements its queue. Wired even when the
	// lane is disabled so the ToS audit still counts downloads left running by an earlier
	// run as held, and the Web UI can list and cancel them.
	slowStore, _ := store.(catalog.SlowLaneStore)

	eng, err := materialize.New(materialize.Deps{
		Store:         store,
		TorBox:        tb,
		Policy:        cfg.Policy,
		ProbeCacheDir: cfg.Paths.ProbeCacheDir,
		ExtraCDNHosts: cfg.TorBox.ExtraCDNHosts,

		DeleteOrphansOlderThan: cfg.Audit.DeleteOrphansOlderThan.D(),
		HeldElsewhere:          slowLaneHeld(slowStore),
	})
	if err != nil {
		slog.Error("materialize engine", "err", err)
		os.Exit(1)
	}

	// Workstream G: the watch-aware local disk cache (docs/28). When enabled it wraps
	// the engine as the FUSE materializer, pulls whole files to a local twin of the
	// FUSE tree, retargets library symlinks, and evicts by a disk-aware policy. When
	// disabled nothing here runs and the FUSE tree talks to the engine as before.
	var (
		fsys *vfs.FS
		lc   *localcache.Engine
		mat  vfs.Materializer = eng
	)
	if cfg.LocalCache.Enabled {
		srcs, arrs := localcache.BuildSources(cfg.Playback)
		lc, err = localcache.New(localcache.Deps{
			Store:      store,
			Inner:      eng,
			Cache:      cfg.LocalCache,
			Playback:   cfg.Playback,
			Paths:      cfg.Paths,
			Ownership:  cfg.Ownership,
			Categories: cfg.Categories,
			Sources:    srcs,
			Arrs:       arrs,
			// fsys is assigned below; the closure reads it at sweep time (S1-safe: the
			// reaper only starts after Mount, and fsys is never reassigned afterwards).
			MountHealthy: func() bool { return fsys != nil && fsys.Healthy() },
		})
		if err != nil {
			slog.Error("local cache", "err", err)
			os.Exit(1)
		}
		mat = lc
		names := make([]string, 0, len(srcs))
		for _, s := range srcs {
			names = append(names, s.Name())
		}
		slog.Info("local cache enabled",
			"dir", cfg.LocalCache.Dir, "publish_at", cfg.EffectivePublishAt(),
			"profile", cfg.LocalCache.Profile, "sources", names, "arrs", len(arrs),
			"pull_on_grab", cfg.LocalCache.PullOnGrab)
	}

	// qbit gets the engine so torrents/delete releases an in-flight materialization (S2),
	// and the local-cache hooks so grabs can pull immediately and deletes drop local bytes.
	qdeps := qbit.Deps{Config: cfg, Store: store, TorBox: tb, Symlink: sym, Engine: eng}
	// Library-link finder for policy.detach_on_delete: walks the arrs' library roots
	// (never download_dir, the FUSE mount or the cache trees) for symlinks into this
	// release's FUSE or cache tree. The link index is the fast path when it is fresh.
	links := &libscan.Scanner{
		Roots:       cfg.EffectiveLibraryRoots(),
		DownloadDir: cfg.Paths.DownloadDir,
		SkipDirs:    []string{cfg.LocalCache.Dir},
		Resolver:    playback.NewResolver(cfg.Paths.FuseMount, cfg.EffectivePublishAt(), nil),
		Store:       store,
	}
	if cfg.Policy.DetachOnDelete {
		qdeps.Links = links
	}
	// Empty-folder cleanup (report-only unless cleanup.remove_empty_dirs).
	emptyDirs := emptyDirSweeper(cfg, store)
	// Library path mapping between Lazarr and each arr (rescans after removed links).
	arrRoots := arrRootResolver(cfg, links)
	// Replacement search after removed links (repair.search_replacement, default off).
	replSearch := replacementSearcher(cfg, store)
	rescanRemoved := removedLinkRescanner(arrRoots, replSearch)
	// Dangling library-link sweeper (report-only unless repair.unlink_orphans).
	dangling := &repair.DanglingSweeper{
		Scanner:         links,
		Store:           store,
		Unlink:          cfg.Repair.UnlinkOrphans,
		ConfirmGap:      danglingConfirmGap,
		MaxHashesPerRun: 20,
		Rescan:          rescanRemoved,
		PruneEmptied:    emptyDirs.PruneEmptied,
	}
	if lc != nil {
		qdeps.GrabHook = lc.OnGrab
		qdeps.ForgetHook = func(h string) {
			if ferr := lc.Forget(h); ferr != nil {
				slog.Warn("local cache: forget on delete", "hash", h, "err", ferr)
			}
		}
	}
	if cfg.Policy.WaitFailBlocklist {
		qdeps.Arrs = waitFailArrs(cfg)
	}
	if slowStore != nil {
		qdeps.SlowLane = slowStore
		qdeps.SlowLaneArrs = slowLaneArrs(cfg)
	}
	qsrv := qbit.New(qdeps)
	// Dead-cache errors from the engine go to the arr like failed wait downloads
	// (policy.wait_fail_blocklist). Set before the mount serves reads.
	eng.SetOnErrored(qsrv.FailInArr)

	srv := &http.Server{
		Addr:              cfg.QBit.Listen,
		Handler:           qsrv,
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// on_cache_miss=wait watcher (no-op when nothing is downloading). Also resumes
	// any StateDownloading rows left by a restart.
	qsrv.StartWaitPoller(ctx)
	// Slow-lane worker (no-op unless slow_lane.enabled); resumes downloading items.
	qsrv.StartSlowLane(ctx)

	go func() {
		slog.Info("qbit listening", "addr", cfg.QBit.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()

	fsys = vfs.New(cfg.Paths.FuseMount, store, mat)
	fsys.SetStableMtime(cfg.LocalCache.Enabled && cfg.LocalCache.StableMtime)
	fsys.SetOwner(cfg.Ownership.PUID, cfg.Ownership.PGID)
	if err := fsys.Mount(); err != nil {
		// FUSE is the core of Phase 2 — without it nothing can be read/played. Close is safe
		// here even though Start has not run yet (no reapers to stop, empty track).
		slog.Error("vfs mount failed (need --cap-add SYS_ADMIN --device /dev/fuse)",
			"mount", cfg.Paths.FuseMount, "err", err)
		_ = eng.Close()
		os.Exit(1)
	}

	// Broken-mount guard (CRITICAL): the reapers call Release -> ControlDelete. If the
	// FUSE mount goes unhealthy on a transient blip the reapers must NOT mass-delete from
	// the TorBox account. Hand the engine a cheap mount-health probe; the reapers skip a
	// sweep (logging a Warn) whenever it reports unhealthy. MUST be set before Start so the
	// reaper goroutine never reads mountHealthy while this writes it (S1).
	eng.SetMountHealthy(fsys.Healthy)
	eng.Start(ctx) // idle + max-hold reapers; stop on ctx cancel and at eng.Close.
	if lc != nil {
		lc.Start(ctx) // publish mount, pull workers, playback poller, link index, cache reaper
	}

	// Observability admin server (opt-in, separate port): /metrics (Prometheus) + /health
	// (JSON). Disabled when metrics.listen is empty. Kept off the arr-facing qbit port; like
	// the qbit listener it is unauthenticated, so bind it to the trusted LAN.
	var adminSrv *http.Server
	if cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.MetricsHandler())
		mux.Handle("/health", metrics.HealthHandler(healthProvider{eng: eng, fsys: fsys}))
		adminSrv = &http.Server{
			Addr:              cfg.Metrics.Listen,
			Handler:           mux,
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			slog.Info("admin listening", "addr", cfg.Metrics.Listen, "endpoints", "/metrics /health")
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin server", "err", err)
				stop()
			}
		}()
	}

	// Web UI server (opt-in, separate port). Disabled when webui.listen is empty.
	// Kept off the arr-facing qbit port. Optionally protected by Basic Auth (set
	// webui.username + webui.password); otherwise trusted-LAN unauthenticated.
	var webuiSrv *http.Server
	if cfg.WebUI.Listen != "" {
		wp := newWebuiProvider(eng, fsys, store, sym, cfg, cachedAccount, startTime)
		wp.missing = missingCollector(cfg)
		if wp.missing != nil {
			wp.missing.Start(ctx)
		}
		wp.lc = lc
		wp.qsrv = qsrv
		wp.dangling = dangling
		wp.cfgPath = *cfgPath
		wp.levelVar = levelVar
		wp.logRing = logRing
		wp.stop = stop
		wh, err := webui.New(wp, cfg.WebUI.Username, cfg.WebUI.Password)
		if err != nil {
			slog.Error("webui setup", "err", err)
			os.Exit(1)
		}
		webuiSrv = &http.Server{
			Addr:              cfg.WebUI.Listen,
			Handler:           wh,
			ReadHeaderTimeout: 15 * time.Second,
		}
		go func() {
			slog.Info("webui listening", "addr", cfg.WebUI.Listen,
				"auth", cfg.WebUI.Username != "")
			if err := webuiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("webui server", "err", err)
				stop()
			}
		}()
	}

	// Pack janitor (opt-in): drop partially imported packs from the arr queues without
	// blocklisting them; detach_on_delete keeps their imported episodes playing.
	if cfg.Policy.PackJanitor {
		j := packJanitor(cfg, store)
		slog.Info("pack janitor enabled", "arrs", len(j.Arrs), "clients", j.Clients, "every", janitor.Interval)
		j.Start(ctx)
	}

	// Dangling library-link sweep: shortly after boot, then hourly.
	go func() {
		first := time.NewTimer(2 * time.Minute)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		t := time.NewTicker(danglingSweepEvery)
		defer t.Stop()
		for {
			if _, err := dangling.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("repair: dangling-link sweep", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	startEmptyDirSweep(ctx, emptyDirs)
	startArrRootResolve(ctx, arrRoots)

	// ToS-audit loop: periodic proof that the account holds nothing we believe is
	// released (scoped to Lazarr-added ids while the account is shared with another tool).
	go func() {
		t := time.NewTicker(auditInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := eng.AuditTOS(); err != nil {
					slog.Warn("tos audit failed", "err", err)
				}
			}
		}
	}()

	// Repair-scan loop: daily checkcached sweep over the whole catalog to find content
	// that TorBox has evicted from its CDN. No TorBox adds; purely read-only. Results
	// are written to the catalog and visible in the Web UI repair tab. With
	// policy.repair_unlink_dead, confirmed-dead library links are then removed and the
	// owning arr is asked to rescan (playback.arrs[].library_root mapping), so it marks
	// the files missing; repair.search_replacement then searches exactly those items.
	if replSearch != nil {
		go replSearch.Run(ctx)
	}
	unlinker := &repair.Unlinker{
		Roots:      cfg.EffectiveLibraryRoots(),
		FuseMount:  cfg.Paths.FuseMount,
		SkipDirs:   []string{cfg.Paths.DownloadDir, cfg.LocalCache.Dir, cfg.EffectivePublishAt()},
		ConfirmGap: constants.RepairUnlinkConfirmGap,
		MaxPerRun:  constants.RepairUnlinkMaxPerRun,
	}
	go func() {
		t := time.NewTicker(constants.DefaultRepairScanEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Detached releases (deleted by the arr while library links still
				// pointed at them) whose links are now all gone are removed here.
				if cfg.Policy.DetachOnDelete {
					qsrv.SweepDetached(ctx)
				}
				if evicted, err := eng.RepairScan(ctx); err != nil {
					slog.Warn("repair scan failed", "err", err)
				} else if len(evicted) > 0 {
					slog.Warn("repair scan: evicted content detected", "count", len(evicted))
					if cfg.Policy.RepairUnlinkDead {
						res, uerr := unlinker.Run(ctx, evicted)
						if uerr != nil {
							slog.Warn("repair: unlink dead links", "err", uerr)
						}
						dirs := emptyDirs.PruneEmptied(res.Removed) // cleanup.remove_empty_dirs
						if len(res.Removed) > 0 {
							// The arr marks the files missing on rescan; with
							// repair.search_replacement they are also searched.
							rescanRemoved(ctx, res.Removed)
						}
						slog.Info("repair: dead-link cleanup done", "confirmed", res.Confirmed,
							"deferred", res.Deferred, "links_removed", len(res.Removed), "dirs_removed", dirs)
					}
				}
			}
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown", "err", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("admin shutdown", "err", err)
		}
	}
	if webuiSrv != nil {
		if err := webuiSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("webui shutdown", "err", err)
		}
	}

	// Stop new reads (unmount FUSE), then release everything still on TorBox so the
	// account is left clean (ToS). eng.Close stops the reapers and waits for them.
	if err := fsys.Unmount(); err != nil {
		slog.Error("vfs unmount", "err", err)
	}
	// Local cache first: in-flight pulls persist their frontier and stop reading
	// through the engine before the engine force-releases everything.
	if lc != nil {
		_ = lc.Close()
	}
	// eng.Close() has no deadline of its own — it force-releases every still-tracked
	// hash via TorBox, and a single degraded ControlDelete call can run TorBox's full
	// retry ladder (~90s). Race it against DefaultShutdownDeadline so a slow/unresponsive
	// TorBox can't wedge the whole process: a leftover TorBox item from a timed-out
	// release is cleaned up by boot reconcile (B2) on next start, but a process that never
	// exits never gets restarted by Docker either.
	closeDone := make(chan error, 1)
	go func() { closeDone <- eng.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			slog.Error("engine close (release-all)", "err", err)
		}
	case <-time.After(constants.DefaultShutdownDeadline):
		slog.Error("engine close (release-all) timed out; exiting anyway — boot reconcile will clean up any TorBox leftovers", "deadline", constants.DefaultShutdownDeadline)
	}
}

// engStats is the slice of the materialize engine the /health endpoint needs. The concrete
// engine type is unexported, so we bridge to it through this interface (it satisfies it via
// its exported SlotsInUse/SlotsTotal/LastAuditUnix methods).
type engStats interface {
	SlotsInUse() int
	SlotsTotal() int
	LastAuditUnix() int64
}

// printBanner writes the boot banner (version + runtime) to stdout before the structured
// logger is installed, so the running build is the first thing `docker logs` shows.
// waitFailArrs builds the per-category arr clients policy.wait_fail_blocklist uses,
// from playback.arrs (URL + LAZARR_ARR_API_KEY_<CATEGORY>) — the same entries the
// local cache uses, but wired whether or not the local cache is enabled.
func waitFailArrs(cfg *config.Config) map[string]qbit.ArrFailer {
	out := map[string]qbit.ArrFailer{}
	for _, a := range cfg.Playback.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		c := arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)
		for _, cat := range a.Categories() {
			out[cat] = c
		}
	}
	var with, without []string
	for _, cat := range cfg.Categories {
		if _, ok := out[cat]; ok {
			with = append(with, cat)
		} else {
			without = append(without, cat)
		}
	}
	slog.Info("wait_fail_blocklist enabled: failed wait downloads are blocklisted in the arr",
		"categories", with, "no_arr_configured", without)
	if !cfg.Policy.AnyWait() {
		slog.Warn("wait_fail_blocklist has no effect unless on_cache_miss is \"wait\"",
			"on_cache_miss", effectiveCacheMiss(cfg.Policy.OnCacheMiss))
	}
	return out
}

// slowLaneArrs builds the per-category arr clients the slow lane uses to give an adopted
// wait download back to its arr (clear blocklist + re-search), from playback.arrs.
func slowLaneArrs(cfg *config.Config) map[string]qbit.SlowLaneArr {
	out := map[string]qbit.SlowLaneArr{}
	for _, a := range cfg.Playback.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		c := arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)
		for _, cat := range a.Categories() {
			out[cat] = c
		}
	}
	return out
}

// missingCollector builds the Web UI Missing tab's collector over playback.arrs (one
// client per arr instance; also_categories share it). nil when missing.enabled is off
// or no arr has a URL and an API key. Its client gets a longer timeout than the other
// arr users: a 500-record wanted/missing page with series embedded is the biggest
// response Lazarr asks an arr for.
func missingCollector(cfg *config.Config) *missing.Collector {
	if !cfg.Missing.Enabled {
		return nil
	}
	var arrs []missing.Arr
	for _, a := range cfg.Playback.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		kind := a.EffectiveKind()
		arrs = append(arrs, missing.Arr{Category: a.Category, Kind: kind,
			Client: arr.New(kind, a.URL, a.APIKey, playback.NewHTTPClient(60*time.Second))})
	}
	if len(arrs) == 0 {
		return nil
	}
	cats := make([]string, len(arrs))
	for i, a := range arrs {
		cats[i] = a.Category
	}
	slog.Info("missing list enabled (Web UI Missing tab)", "arrs", cats, "every", cfg.Missing.RefreshInterval.D())
	return missing.New(arrs, cfg.Missing.RefreshInterval.D())
}

// slowLaneHeld returns the ToS-audit hook listing the TorBox ids of downloading
// slow-lane items (nil when the store has no slow lane).
func slowLaneHeld(st catalog.SlowLaneStore) func() ([]int64, error) {
	if st == nil {
		return nil
	}
	return func() ([]int64, error) {
		items, err := st.ListSlowLane(catalog.SlowDownloading)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(items))
		for _, it := range items {
			if it.TorBoxID != 0 {
				ids = append(ids, it.TorBoxID)
			}
		}
		return ids, nil
	}
}

// packJanitor builds the janitor over playback.arrs (one client per arr instance; aliases
// via also_categories share that instance's queue).
func packJanitor(cfg *config.Config, store catalog.Store) *janitor.Janitor {
	arrs := map[string]janitor.Arr{}
	for _, a := range cfg.Playback.Arrs {
		if a.URL == "" || a.APIKey == "" {
			continue
		}
		arrs[a.Category] = arr.New(a.EffectiveKind(), a.URL, a.APIKey, nil)
	}
	clients := cfg.Policy.PackJanitorClients
	if len(clients) == 0 {
		clients = []string{"Lazarr", "Lazarr Seerr"}
	}
	return &janitor.Janitor{
		Arrs:    arrs,
		Clients: clients,
		Known: func(h string) bool {
			rel, _, err := store.GetRelease(h)
			return err == nil && rel != nil
		},
	}
}

func printBanner() {
	fmt.Printf(""+
		"  _                                \n"+
		" | |    __ _ ____ _ _ __ _ __ \n"+
		" | |   / _` |_  / _` | '__| '__|\n"+
		" | |__| (_| |/ / (_| | |  | |   \n"+
		" |_____\\__,_/___\\__,_|_|  |_|   \n"+
		"  lazy-materialize TorBox shim   %s (%s/%s)\n\n",
		version.Version, runtime.GOOS, runtime.GOARCH)
}

// healthProvider adapts the engine + FUSE mount to metrics.HealthProvider for /health.
type healthProvider struct {
	eng  engStats
	fsys *vfs.FS
}

func (h healthProvider) Mounted() bool        { return h.fsys.Healthy() }
func (h healthProvider) SlotsInUse() int      { return h.eng.SlotsInUse() }
func (h healthProvider) SlotsTotal() int      { return h.eng.SlotsTotal() }
func (h healthProvider) LastAuditUnix() int64 { return h.eng.LastAuditUnix() }
func (h healthProvider) Version() string      { return version.Version }

// ─── Web UI provider ──────────────────────────────────────────────────────────

// webuiEngine is the slice of *materialize.materializer the webuiProvider needs.
// It deliberately does not overlap with engStats so both can coexist without a
// wide interface, and uses the concrete *materialize.materializer type in practice.
type webuiEngine interface {
	SlotsInUse() int
	SlotsTotal() int
	LastAuditUnix() int64
	Release(hash string) error
	AuditTOS() error
	RepairScan(ctx context.Context) ([]materialize.RepairEntry, error)
	MaterializedSnapshot() []materialize.MaterializedEntry
	Orphans() materialize.OrphanReport
}

// webuiProvider adapts Lazarr's concrete internals to webui.Provider.
type webuiProvider struct {
	eng     webuiEngine
	fsys    *vfs.FS
	store   catalog.Store
	sym     symlink.Manager
	cfg     *config.Config
	account *webui.AccountInfo // cached from boot-time UserMe (may be nil)
	start   time.Time

	lc       *localcache.Engine      // nil when local_cache.enabled is false
	qsrv     qbit.Server             // slow-lane entry points (nil in tests)
	dangling *repair.DanglingSweeper // dangling library-link sweeper
	missing  *missing.Collector      // Missing tab; nil when off

	// Settings/observability plumbing (wired by main after construction).
	cfgPath  string         // path of the loaded config.yaml (settings editor target)
	levelVar *slog.LevelVar // live log-level control
	logRing  *logging.Ring  // recent-records buffer for /api/logs
	stop     func()         // cancels the root signal context (graceful restart)
}

func newWebuiProvider(
	eng webuiEngine,
	fsys *vfs.FS,
	store catalog.Store,
	sym symlink.Manager,
	cfg *config.Config,
	acct *torbox.Account,
	start time.Time,
) *webuiProvider {
	wp := &webuiProvider{eng: eng, fsys: fsys, store: store, sym: sym, cfg: cfg, start: start}
	if acct != nil {
		wp.account = &webui.AccountInfo{
			Plan:          acct.Plan,
			ActiveSlots:   acct.ActiveSlots,
			CooldownUntil: acct.CooldownUntil,
			LongTermStore: acct.LongTermStore,
		}
	}
	return wp
}

func (p *webuiProvider) Status() webui.StatusSnapshot {
	return webui.StatusSnapshot{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(p.start).Seconds()),
		Mounted:       p.fsys.Healthy(),
		SlotsInUse:    p.eng.SlotsInUse(),
		SlotsTotal:    p.eng.SlotsTotal(),
		LastAuditUnix: p.eng.LastAuditUnix(),
		Account:       p.account,
	}
}

func (p *webuiProvider) ListReleases(f catalog.ReleaseFilter) ([]*catalog.Release, int, error) {
	return p.store.ListReleases(f)
}

func (p *webuiProvider) MaterializedSet() []webui.MaterializedItem {
	snap := p.eng.MaterializedSnapshot()
	out := make([]webui.MaterializedItem, len(snap))
	for i, e := range snap {
		it := webui.MaterializedItem{
			Hash:       e.Hash,
			TorBoxID:   e.TorBoxID,
			Refs:       e.Refs,
			LastUsedNs: e.LastUsedNs,
		}
		// Join with the catalog so the UI shows the release title, not an infohash.
		if rel, _, err := p.store.GetRelease(e.Hash); err == nil && rel != nil {
			it.Name = rel.Name
			it.Category = rel.Category
			it.TotalSize = rel.TotalSize
		}
		out[i] = it
	}
	return out
}

func (p *webuiProvider) MetricsSummary() (*metrics.Summary, error) {
	return metrics.GatherSummary()
}

func (p *webuiProvider) ForceRelease(hash string) error {
	return p.eng.Release(hash)
}

func (p *webuiProvider) TriggerAudit() error {
	return p.eng.AuditTOS()
}

func (p *webuiProvider) TriggerRepairScan(ctx context.Context) ([]materialize.RepairEntry, error) {
	return p.eng.RepairScan(ctx)
}

func (p *webuiProvider) Orphans() materialize.OrphanReport { return p.eng.Orphans() }

func (p *webuiProvider) Dangling() repair.DanglingReport {
	if p.dangling == nil {
		return repair.DanglingReport{}
	}
	return p.dangling.Last()
}

func (p *webuiProvider) ListEvicted() ([]*catalog.Release, error) {
	return p.store.ListEvicted()
}

// ForgetRelease removes a release from Lazarr entirely — engine release (if materialized),
// symlinks deleted, catalog entry removed — so the arr's next health-check flags the file
// missing and triggers a re-search through the arr's indexers.
func (p *webuiProvider) ForgetRelease(hash string) error {
	// Local cache first: links back to FUSE, bytes gone, before the symlinks vanish.
	if p.lc != nil {
		if err := p.lc.Forget(hash); err != nil {
			slog.Warn("webui: forget: local cache", "hash", hash, "err", err)
		}
	}
	// Best-effort engine release first (no-op if not materialized, returns ErrNotFound).
	_ = p.eng.Release(hash)
	if err := p.sym.Remove(hash); err != nil {
		slog.Warn("webui: forget: remove symlinks", "hash", hash, "err", err)
	}
	return p.store.DeleteRelease(hash)
}

// GetSettings maps the running config to the editable settings form. Secrets are
// blanked; *Set flags report presence so the form can say "configured".
func (p *webuiProvider) GetSettings() webui.Settings {
	c := p.cfg
	return webui.Settings{
		LogLevel:          effectiveLogLevel(c.LogLevel),
		TorBoxAPIKeySet:   c.TorBox.APIKey != "",
		TorBoxAPIBase:     c.TorBox.APIBase,
		QBitListen:        c.QBit.Listen,
		QBitUsername:      c.QBit.Username,
		QBitPassword:      c.QBit.Password,
		Categories:        append([]string(nil), c.Categories...),
		DownloadDir:       c.Paths.DownloadDir,
		FuseMount:         c.Paths.FuseMount,
		DBPath:            c.Paths.DBPath,
		ProbeCacheDir:     c.Paths.ProbeCacheDir,
		AllowUncached:     c.Policy.AllowUncached,
		IdleTTL:           c.Policy.IdleTTL.D().String(),
		MaxHold:           c.Policy.MaxHold.D().String(),
		ActiveSlots:       c.Policy.ActiveSlots,
		ProbeCache:        c.Policy.ProbeCache,
		OnCacheMiss:       effectiveCacheMiss(c.Policy.OnCacheMiss),
		CacheWaitBudget:   c.Policy.CacheWaitBudget.D().String(),
		MaxWaitDownloads:  c.Policy.MaxWaitDownloads,
		CacheWaitMax:      c.Policy.CacheWaitMax.D().String(),
		ReadaheadWindows:  c.Policy.ReadaheadWindows,
		ReadaheadChunkMiB: c.Policy.ReadaheadChunkMiB,
		PUID:              c.Ownership.PUID,
		PGID:              c.Ownership.PGID,
		OwnershipEnv:      c.Playback.EnvSet.Ownership,
		MetricsListen:     c.Metrics.Listen,
		WebUIListen:       c.WebUI.Listen,
		WebUIUsername:     c.WebUI.Username,
		WebUIPasswordSet:  c.WebUI.Password != "",
		Cache:             cacheSettingsFrom(c),
		Playback:          playbackSettingsFrom(c),
		SlowLane:          slowLaneSettingsFrom(c),
		Cleanup:           cleanupSettingsFrom(c),
		Profiles:          profileInfos(),
	}
}

func cacheSettingsFrom(c *config.Config) webui.CacheSettings {
	lc := c.LocalCache
	return webui.CacheSettings{
		Enabled:             lc.Enabled,
		Dir:                 lc.Dir,
		Publish:             lc.Publish,
		PublishAt:           c.EffectivePublishAt(),
		LibraryRoots:        append([]string(nil), lc.LibraryRoots...),
		LinkIndexInterval:   lc.LinkIndexInterval.D().String(),
		UnreferencedGrace:   lc.UnreferencedGrace.D().String(),
		TorrentOnly:         lc.TorrentOnly,
		Profile:             lc.Profile,
		FreeHighPct:         lc.FreeHighPct,
		FreeLowPct:          lc.FreeLowPct,
		ReservePct:          lc.ReservePct,
		TTLWatching:         lc.TTL.Watching.D().String(),
		TTLFinished:         lc.TTL.Finished.D().String(),
		TTLFinishedMovie:    lc.TTL.FinishedMovie.D().String(),
		TTLUntouched:        lc.TTL.Untouched.D().String(),
		TTLNewMovie:         lc.TTL.NewMovie.D().String(),
		TTLDefault:          lc.TTL.Default.D().String(),
		ActiveWindow:        lc.ActiveWindow.D().String(),
		NewReleaseWindow:    lc.NewReleaseWindow.D().String(),
		PullOnGrab:          append([]string(nil), lc.PullOnGrab...),
		PrefetchAhead:       lc.PrefetchAhead,
		PullCurrent:         lc.PullCurrent,
		MaxConcurrentPulls:  lc.MaxConcurrentPulls,
		MaxPullsPerHour:     lc.MaxPullsPerHour,
		SpeculativeGBPerDay: lc.SpeculativeGBPerDay,
		MinFileMiB:          lc.MinFileMiB,
		PullWindowMiB:       lc.PullWindowMiB,
		StableMtime:         lc.StableMtime,
	}
}

func slowLaneSettingsFrom(c *config.Config) webui.SlowLaneSettings {
	sl := c.SlowLane
	return webui.SlowLaneSettings{
		Enabled:           sl.Enabled,
		MaxConcurrent:     sl.MaxConcurrent,
		PollInterval:      sl.PollInterval.D().String(),
		MaxAge:            sl.MaxAge.D().String(),
		AdoptWaitFailures: sl.AdoptWaitFailures,
		AutoImport:        &sl.AutoImport,
		AutoImportAfter:   sl.AutoImportAfter.D().String(),
	}
}

// applySlowLaneSettings overlays the slow-lane form onto nc. An empty duration keeps the
// current value (an older form that does not send the field must not reset it).
func applySlowLaneSettings(nc *config.Config, s webui.SlowLaneSettings) error {
	nc.SlowLane.Enabled = s.Enabled
	nc.SlowLane.AdoptWaitFailures = s.AdoptWaitFailures && s.Enabled
	if s.MaxConcurrent != 0 {
		nc.SlowLane.MaxConcurrent = s.MaxConcurrent
	}
	if s.PollInterval != "" {
		d, err := config.ParseDuration(s.PollInterval)
		if err != nil {
			return fmt.Errorf("slow lane check interval %q: %w", s.PollInterval, err)
		}
		nc.SlowLane.PollInterval = config.Duration(d)
	}
	if s.MaxAge != "" {
		d, err := config.ParseDuration(s.MaxAge)
		if err != nil {
			return fmt.Errorf("slow lane give-up age %q: %w", s.MaxAge, err)
		}
		nc.SlowLane.MaxAge = config.Duration(d)
	}
	if s.AutoImport != nil {
		nc.SlowLane.AutoImport = *s.AutoImport
	}
	if s.AutoImportAfter != "" {
		d, err := config.ParseDuration(s.AutoImportAfter)
		if err != nil {
			return fmt.Errorf("slow lane auto import delay %q: %w", s.AutoImportAfter, err)
		}
		nc.SlowLane.AutoImportAfter = config.Duration(d)
	}
	return nil
}

func playbackSettingsFrom(c *config.Config) webui.PlaybackSettings {
	pb := c.Playback
	out := webui.PlaybackSettings{
		PollInterval:      pb.PollInterval.D().String(),
		WatchedPct:        pb.WatchedPct,
		RefreshOnRetarget: pb.RefreshOnRetarget,
		PlexURL:           pb.Plex.URL,
		PlexTokenSet:      pb.Plex.Token != "",
		PlexTokenEnv:      pb.EnvSet.PlexToken,
		JellyfinURL:       pb.Jellyfin.URL,
		JellyfinKeySet:    pb.Jellyfin.APIKey != "",
		JellyfinKeyEnv:    pb.EnvSet.JellyfinAPIKey,
		PathMap:           []webui.PathMapSetting{},
		Arrs:              []webui.ArrSetting{},
	}
	for _, m := range pb.PathMap {
		out.PathMap = append(out.PathMap, webui.PathMapSetting{From: m.From, To: m.To})
	}
	for _, a := range pb.Arrs {
		out.Arrs = append(out.Arrs, webui.ArrSetting{
			Category: a.Category, URL: a.URL, Kind: a.EffectiveKind(),
			KeySet: a.APIKey != "", KeyEnv: pb.EnvSet.ArrAPIKeys[a.Category], EnvVar: config.ArrEnvKey(a.Category),
		})
	}
	return out
}

func profileInfos() []webui.ProfileInfo {
	var out []webui.ProfileInfo
	for _, p := range config.Profiles() {
		out = append(out, webui.ProfileInfo{Name: p.Name, Finished: p.Finished, FinishedMovie: p.FinishedMovie, Untouched: p.Untouched,
			NewMovie: p.NewMovie, Default: p.Default, FreeHighPct: p.FreeHighPct, FreeLowPct: p.FreeLowPct})
	}
	return out
}

// applyCacheSettings overlays the cache + playback form onto nc. Secrets are never
// taken from the form: env-sourced ones are re-applied by Load, YAML ones survive
// because nc was loaded from disk.
func applyCacheSettings(nc *config.Config, s webui.Settings) error {
	dur := func(field, v string) (config.Duration, error) {
		d, err := config.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s %q: %w", field, v, err)
		}
		return config.Duration(d), nil
	}
	cs := s.Cache
	lc := &nc.LocalCache
	lc.Enabled = cs.Enabled
	lc.Dir = strings.TrimSpace(cs.Dir)
	lc.Publish = cs.Publish
	lc.PublishAt = strings.TrimSpace(cs.PublishAt)
	lc.LibraryRoots = normalizeCategories(cs.LibraryRoots)
	lc.TorrentOnly = cs.TorrentOnly
	lc.Profile = cs.Profile
	lc.FreeHighPct = cs.FreeHighPct
	lc.FreeLowPct = cs.FreeLowPct
	lc.ReservePct = cs.ReservePct
	lc.PullOnGrab = normalizeCategories(cs.PullOnGrab)
	lc.PrefetchAhead = cs.PrefetchAhead
	lc.PullCurrent = cs.PullCurrent
	lc.MaxConcurrentPulls = cs.MaxConcurrentPulls
	lc.MaxPullsPerHour = cs.MaxPullsPerHour
	lc.SpeculativeGBPerDay = cs.SpeculativeGBPerDay
	lc.MinFileMiB = cs.MinFileMiB
	lc.PullWindowMiB = cs.PullWindowMiB
	lc.StableMtime = cs.StableMtime
	var err error
	for _, f := range []struct {
		name string
		dst  *config.Duration
		src  string
	}{
		{"link index interval", &lc.LinkIndexInterval, cs.LinkIndexInterval},
		{"unreferenced grace", &lc.UnreferencedGrace, cs.UnreferencedGrace},
		{"keep while watching", &lc.TTL.Watching, cs.TTLWatching},
		{"keep after finished", &lc.TTL.Finished, cs.TTLFinished},
		{"keep movies after finished", &lc.TTL.FinishedMovie, cs.TTLFinishedMovie},
		{"keep if never played", &lc.TTL.Untouched, cs.TTLUntouched},
		{"keep new releases", &lc.TTL.NewMovie, cs.TTLNewMovie},
		{"keep everything else", &lc.TTL.Default, cs.TTLDefault},
		{"active window", &lc.ActiveWindow, cs.ActiveWindow},
		{"new release window", &lc.NewReleaseWindow, cs.NewReleaseWindow},
		{"poll interval", &nc.Playback.PollInterval, s.Playback.PollInterval},
	} {
		if f.src == "" {
			continue // keep current
		}
		if *f.dst, err = dur(f.name, f.src); err != nil {
			return err
		}
	}
	ps := s.Playback
	pb := &nc.Playback
	pb.WatchedPct = ps.WatchedPct
	pb.RefreshOnRetarget = ps.RefreshOnRetarget
	pb.Plex.URL = strings.TrimSpace(ps.PlexURL)
	pb.Jellyfin.URL = strings.TrimSpace(ps.JellyfinURL)
	pb.PathMap = nil
	for _, m := range ps.PathMap {
		if strings.TrimSpace(m.From) == "" && strings.TrimSpace(m.To) == "" {
			continue
		}
		pb.PathMap = append(pb.PathMap, config.PathMap{From: strings.TrimSpace(m.From), To: strings.TrimSpace(m.To)})
	}
	existing := map[string]config.Arr{}
	for _, a := range pb.Arrs {
		existing[a.Category] = a
	}
	pb.Arrs = nil
	for _, a := range ps.Arrs {
		cat := strings.TrimSpace(a.Category)
		if cat == "" {
			continue
		}
		na := config.Arr{Category: cat, URL: strings.TrimSpace(a.URL), Kind: a.Kind,
			// not on this form: preserved (library roots are edited by the cleanup form)
			AlsoCategories: existing[cat].AlsoCategories,
			LibraryRoot:    existing[cat].LibraryRoot, NoLibrarySweep: existing[cat].NoLibrarySweep}
		if na.Kind == "radarr" || na.Kind == "sonarr" {
			if na.Kind == (config.Arr{Category: cat}).EffectiveKind() {
				na.Kind = "" // inferred; keep the YAML terse
			}
		} else {
			na.Kind = ""
		}
		na.APIKey = existing[cat].APIKey // YAML-sourced key survives; env keys re-apply on load
		pb.Arrs = append(pb.Arrs, na)
	}
	return nil
}

// effectiveLogLevel normalizes "" to "info" for display.
func effectiveLogLevel(s string) string {
	if s == "" {
		return "info"
	}
	return s
}

// effectiveCacheMiss normalizes "" to "error" for display.
func effectiveCacheMiss(s string) string {
	if s == "" {
		return "error"
	}
	return s
}

// SaveSettings overlays s onto the on-disk config, validates, writes the file
// atomically, hot-applies the log level, and reports whether anything else changed
// (= restart required). Empty secret fields keep the current values.
func (p *webuiProvider) SaveSettings(s webui.Settings) (bool, error) {
	if p.cfgPath == "" {
		return false, fmt.Errorf("settings editing unavailable: config path unknown")
	}
	// Start from the file on disk (source of truth), not the running config — a
	// previous save that still awaits restart must not be silently reverted.
	nc, err := config.Load(p.cfgPath)
	if err != nil {
		return false, fmt.Errorf("reload current config: %w", err)
	}

	nc.LogLevel = s.LogLevel
	if s.TorBoxAPIKey != "" {
		nc.TorBox.APIKey = s.TorBoxAPIKey
	}
	nc.TorBox.APIBase = s.TorBoxAPIBase
	nc.QBit.Listen = s.QBitListen
	nc.QBit.Username = s.QBitUsername
	nc.QBit.Password = s.QBitPassword
	nc.Categories = normalizeCategories(s.Categories)
	nc.Paths.DownloadDir = s.DownloadDir
	nc.Paths.FuseMount = s.FuseMount
	nc.Paths.DBPath = s.DBPath
	nc.Paths.ProbeCacheDir = s.ProbeCacheDir
	nc.Policy.AllowUncached = s.AllowUncached
	idle, err := time.ParseDuration(s.IdleTTL)
	if err != nil {
		return false, fmt.Errorf("keep on TorBox for (idle_ttl) %q: %w", s.IdleTTL, err)
	}
	hold, err := time.ParseDuration(s.MaxHold)
	if err != nil {
		return false, fmt.Errorf("absolute limit (max_hold) %q: %w", s.MaxHold, err)
	}
	nc.Policy.IdleTTL = config.Duration(idle)
	nc.Policy.MaxHold = config.Duration(hold)
	nc.Policy.ActiveSlots = s.ActiveSlots
	nc.Policy.ProbeCache = s.ProbeCache
	nc.Policy.OnCacheMiss = s.OnCacheMiss
	if s.CacheWaitBudget != "" {
		budget, berr := time.ParseDuration(s.CacheWaitBudget)
		if berr != nil {
			return false, fmt.Errorf("cache wait budget %q: %w", s.CacheWaitBudget, berr)
		}
		nc.Policy.CacheWaitBudget = config.Duration(budget)
	}
	nc.Policy.MaxWaitDownloads = s.MaxWaitDownloads
	if s.CacheWaitMax != "" {
		wmax, werr := time.ParseDuration(s.CacheWaitMax)
		if werr != nil {
			return false, fmt.Errorf("cache wait max %q: %w", s.CacheWaitMax, werr)
		}
		nc.Policy.CacheWaitMax = config.Duration(wmax)
	}
	nc.Policy.ReadaheadWindows = s.ReadaheadWindows
	nc.Policy.ReadaheadChunkMiB = s.ReadaheadChunkMiB
	if !nc.Playback.EnvSet.Ownership { // PUID/PGID env wins; the form is read-only then
		nc.Ownership.PUID = s.PUID
		nc.Ownership.PGID = s.PGID
	}
	nc.Metrics.Listen = s.MetricsListen
	nc.WebUI.Listen = s.WebUIListen
	nc.WebUI.Username = s.WebUIUsername
	if s.WebUIPassword != "" {
		nc.WebUI.Password = s.WebUIPassword
	}
	if s.WebUIUsername == "" {
		nc.WebUI.Password = "" // auth off: clear the orphan password so validate passes
	}
	if err := applyCacheSettings(nc, s); err != nil {
		return false, err
	}
	if err := applySlowLaneSettings(nc, s.SlowLane); err != nil {
		return false, err
	}
	if err := applyCleanupSettings(nc, s.Cleanup); err != nil {
		return false, err
	}
	// The cache dir is published into the shared tree; check it against the RUNNING
	// private paths too, so a single request cannot move db_path aside and publish the
	// old location in the same breath.
	if nc.LocalCache.Enabled {
		for name, priv := range map[string]string{"db_path": p.cfg.Paths.DBPath, "probe_cache_dir": p.cfg.Paths.ProbeCacheDir, "the config file": p.cfg.ConfigDir} {
			if priv != "" && (priv == nc.LocalCache.Dir || strings.HasPrefix(priv, strings.TrimRight(nc.LocalCache.Dir, "/")+"/")) {
				return false, fmt.Errorf("local cache folder (%s) must not contain %s (%s)", nc.LocalCache.Dir, name, priv)
			}
		}
	}

	if err := nc.Validate(); err != nil {
		return false, err
	}
	if err := config.Save(p.cfgPath, nc); err != nil {
		return false, err
	}

	// Hot-apply the log level to the running process.
	if lv, lerr := logging.ParseLevel(nc.LogLevel); lerr == nil && p.levelVar != nil {
		p.levelVar.Set(lv)
	}
	p.cfg.LogLevel = nc.LogLevel // keep the running view consistent for the next diff

	// Restart needed iff anything besides the log level differs from what is running.
	runningCmp := *p.cfg
	newCmp := *nc
	runningCmp.LogLevel, newCmp.LogLevel = "", ""
	restart := !reflect.DeepEqual(runningCmp, newCmp)
	return restart, nil
}

// normalizeCategories trims whitespace and drops empties/duplicates, preserving order.
func normalizeCategories(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// ─── slow lane ────────────────────────────────────────────────────────────────

func (p *webuiProvider) SlowLane() (webui.SlowLaneView, error) {
	v := webui.SlowLaneView{
		Enabled:         p.cfg.SlowLane.Enabled,
		MaxConcurrent:   p.cfg.SlowLane.MaxConcurrent,
		PollInterval:    p.cfg.SlowLane.PollInterval.D().String(),
		AutoImport:      p.cfg.SlowLane.AutoImport,
		AutoImportAfter: p.cfg.SlowLane.AutoImportAfter.D().String(),
		Categories:      append([]string(nil), p.cfg.Categories...),
		CategoryKinds:   map[string]string{},
	}
	for _, c := range p.cfg.Categories {
		if k := p.cfg.CategoryKind(c); k != "" {
			v.CategoryKinds[c] = k
		}
	}
	if p.qsrv == nil {
		return v, nil
	}
	items, err := p.qsrv.SlowLaneList()
	v.Items = items
	return v, err
}

func (p *webuiProvider) SlowLaneAdd(input, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error) {
	if p.qsrv == nil {
		return nil, qbit.ErrSlowLaneDisabled
	}
	return p.qsrv.SlowLaneEnqueue(input, category, target)
}

func (p *webuiProvider) SlowLaneAddTorrent(torrent []byte, category string, target *catalog.SlowLaneTarget) (*catalog.SlowLaneItem, error) {
	if p.qsrv == nil {
		return nil, qbit.ErrSlowLaneDisabled
	}
	return p.qsrv.SlowLaneEnqueueTorrent(torrent, category, target)
}

// The Missing tab is an optional interface; keep the provider implementing it.
var _ webui.MissingProvider = (*webuiProvider)(nil)

func (p *webuiProvider) Missing(ctx context.Context, refresh bool) (missing.Snapshot, error) {
	if p.missing == nil {
		return missing.Snapshot{}, webui.ErrMissingDisabled
	}
	return p.missing.Get(ctx, refresh)
}

func (p *webuiProvider) MissingSearch(ctx context.Context, req missing.SearchRequest) (missing.SearchResult, error) {
	if p.missing == nil {
		return missing.SearchResult{}, webui.ErrMissingDisabled
	}
	return p.missing.Search(ctx, req)
}

func (p *webuiProvider) SlowLaneCancel(hash string) error {
	if p.qsrv == nil {
		return qbit.ErrSlowLaneDisabled
	}
	return p.qsrv.SlowLaneCancel(hash)
}

func (p *webuiProvider) Logs(level string, limit int) []logging.Entry {
	if p.logRing == nil {
		return nil
	}
	lv, err := logging.ParseLevel(level)
	if err != nil {
		lv = slog.LevelDebug // unknown filter -> show everything captured
	}
	return p.logRing.Snapshot(lv, limit)
}

// Restart schedules a graceful shutdown just after the HTTP response flushes. The
// container's restart policy (restart: unless-stopped) brings Lazarr back up.
func (p *webuiProvider) Restart() error {
	if p.stop == nil {
		return fmt.Errorf("restart unavailable")
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		slog.Info("webui: restarting (graceful shutdown; supervisor restarts the container)")
		p.stop()
	}()
	return nil
}

func (p *webuiProvider) SafeConfig() webui.SafeConfig {
	return webui.SafeConfig{
		TorBoxAPIBase: p.cfg.TorBox.APIBase,
		// api_key intentionally omitted
		QBitListen:    p.cfg.QBit.Listen,
		AdminListen:   p.cfg.Metrics.Listen,
		WebUIListen:   p.cfg.WebUI.Listen,
		DownloadDir:   p.cfg.Paths.DownloadDir,
		FuseMount:     p.cfg.Paths.FuseMount,
		DBPath:        p.cfg.Paths.DBPath,
		Categories:    p.cfg.Categories,
		AllowUncached: p.cfg.Policy.AllowUncached,
		IdleTTL:       p.cfg.Policy.IdleTTL.D().String(),
		MaxHold:       p.cfg.Policy.MaxHold.D().String(),
		ActiveSlots:   p.cfg.Policy.ActiveSlots,
		ProbeCache:    p.cfg.Policy.ProbeCache,
		OwnershipPUID: p.cfg.Ownership.PUID,
		OwnershipPGID: p.cfg.Ownership.PGID,
		AuthEnabled:   p.cfg.WebUI.Username != "",
	}
}

// ---- Workstream G: local cache provider methods ----

func (p *webuiProvider) CacheSnapshot() *localcache.Snapshot {
	if p.lc == nil {
		return nil
	}
	snap := p.lc.Snapshot()
	return &snap
}

func (p *webuiProvider) CacheFiles() []localcache.LocalFileView {
	if p.lc == nil {
		return nil
	}
	return p.lc.LocalFiles()
}

func (p *webuiProvider) CachePull(hash string) (int, error) {
	if p.lc == nil {
		return 0, fmt.Errorf("local cache is disabled")
	}
	return p.lc.PullRelease(hash)
}

func (p *webuiProvider) CacheEvict(hash string) error {
	if p.lc == nil {
		return fmt.Errorf("local cache is disabled")
	}
	return p.lc.EvictRelease(hash)
}

func (p *webuiProvider) CacheAnalyze(hash string) (int, error) {
	if p.lc == nil {
		return 0, fmt.Errorf("local cache is disabled")
	}
	return p.lc.AnalyzeRelease(hash)
}

func (p *webuiProvider) CacheAnalyzeBackfill() (int, error) {
	if p.lc == nil {
		return 0, fmt.Errorf("local cache is disabled")
	}
	return p.lc.AnalyzeBackfill()
}

func (p *webuiProvider) CachePoll(ctx context.Context) error {
	if p.lc == nil {
		return fmt.Errorf("local cache is disabled")
	}
	p.lc.PollOnce(ctx)
	return nil
}
