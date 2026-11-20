# Lazarr

**A self-hosted, ToS-compliant streaming bridge between the *arr suite and [TorBox](https://torbox.app).**

Lazarr looks like a qBittorrent download client to Sonarr/Radarr — but nothing is ever
downloaded. When an arr grabs a release, Lazarr verifies it is in TorBox's cache and
instantly places a file link in your library. When you press **play** (Plex, Jellyfin,
Emby…), Lazarr adds the torrent to your TorBox account on the fly and streams the bytes
straight to your player. After the item sits unwatched for an idle window, Lazarr
**removes it from TorBox again** — automatically, every time.

The result: an effectively infinite, instant library, while your TorBox account stays
clean. No hoarding, no 30-day purges silently breaking your files, no dead 0-byte
"downloads".

> **⚠️ Status: early.** Lazarr is young, single-tenant, and TorBox-only. It has run in
> production, but expect rough edges. Back up your arr configs
> before pointing them at anything new.
>
> **Open source, no support.** This is published as-is under MIT. There is no support,
> no SLA, and no roadmap commitments — issues and PRs are welcome but may not get a
> response. Fork freely.
>
> **🤖 This project is 100% AI-developed** (designed, written, reviewed, and tested by
> Claude, driven by a human operator). Read the code accordingly.

---

## How it works

```
  Sonarr/Radarr            Lazarr                       TorBox
       │   grab               │                            │
       ├────────────────────► │  checkcached (no add!)     │
       │                      ├──────────────────────────► │
       │   "complete"         │  symlink → virtual file    │
       │ ◄────────────────────┤                            │
       │   import (instant)   │                            │
       │                      │                            │
  Plex │   play ▶             │                            │
       ├────────────────────► │  add torrent + request CDN │
       │                      ├──────────────────────────► │
       │   bytes ◄────────────┤ ◄────────── stream ────────┤
       │                      │                            │
       │   (idle 7 days)      │  delete torrent            │
       │                      ├──────────────────────────► │
       │   play again ▶       │  ...repeats transparently  │
```

1. **Grab — instant, nothing added.** Lazarr checks the release against TorBox's cache
   (`checkcached` — a read-only call), records the file list and sizes, and creates
   symlinks into a virtual FUSE drive. The arr imports immediately. Your TorBox account
   is untouched.
2. **Play — materialize on demand.** The first read of the file (a player, or the arr's
   import probe) makes Lazarr add the torrent to TorBox, fetch a presigned CDN link, and
   range-proxy the bytes through the FUSE mount. First play costs a few extra seconds.
3. **Clean up — automatic.** A reaper releases items after `idle_ttl` unwatched (default
   7 days) and enforces a hard `max_hold` ceiling (default 30 days). When all streaming
   slots are full, the least-recently-used idle item is evicted to admit a new play.
   Your library files keep working — the next play just re-materializes.

### Why this is ToS-compliant

TorBox prohibits tools that artificially keep transfers active ("excessive transfer
retention") and purges unaccessed transfers after ~30 days. Eager-add tools collide with
this head-on: they push your whole library into the account, TorBox purges it, and your
files die. Lazarr never adds at grab time, releases after use, and continuously audits
its own behavior: a built-in **ToS audit** diffs your TorBox account against what Lazarr
believes it added and alarms on any leak. There is also a daily **availability scanner**
that detects content TorBox has evicted from cache so your arr can re-search it.

---

## Features

- **qBittorrent WebUI API emulation** — Sonarr/Radarr connect natively; one category per
  arr instance.
- **Cached-only by default** — only accepts releases TorBox already has (instant
  playback). Configurable cache-miss handling: surface an error, **reject so the arr
  instantly tries another release**, or **wait** — let TorBox download it with a real
  progress bar in the arr, bounded by a configurable ETA budget.
- **Configurable readahead** — parallel window prefetch sized for your bitrate, from
  light 1080p up to 4K remux.
- **FUSE virtual drive** — files exist at full size without using disk; reads are
  SSRF-safe range-proxies to TorBox's CDN with automatic link refresh on expiry *and* on
  dead CDN nodes.
- **Slot-aware** — respects your plan's concurrent-download slots (Essential = 3) with
  LRU eviction under pressure; auto-detects from your account if unset.
- **Watch-aware local cache (opt-in)** — keep real copies of what is being watched on
  your own disk: Lazarr polls Plex/Jellyfin sessions, pulls the current and next
  episodes to a local twin of the FUSE tree, atomically switches the library symlink to
  the real file (Plex reads local, no streaming), and switches it back before evicting
  by a per-file, per-user, disk-aware retention policy. Movie grabs can pull at request
  time. Usenet grabs are never cached. The TorBox account lifecycle above is unchanged.
- **Web UI** — dashboard, library, active streams, content-health/repair, a **Missing**
  list of what the linked arrs still want (search / hand to the slow lane), live logs, and
  a **fully editable settings page** (every config value, including the API key, log
  level applied live; restart button for the rest).
- **Self-auditing** — ToS audit every 5 minutes, daily availability scan, Prometheus
  `/metrics` + `/health`, structured logs with runtime-switchable levels.
- **Crash-safe** — boot reconciliation releases anything left on TorBox by an unclean
  exit; stale FUSE mounts are detected and recovered automatically.
- Single static Go binary, pure-Go SQLite, multi-arch Docker images (amd64/arm64).

---

## Installation

### Requirements

- Docker (or a Linux host with FUSE3 for bare-metal).
- A TorBox account + API key ([torbox.app/settings](https://torbox.app/settings)).
- Host `/etc/fuse.conf` must contain `user_allow_other` (one-time:
  `echo user_allow_other | sudo tee -a /etc/fuse.conf`).
- Sonarr/Radarr and your media server must mount the **same host data path at the same
  container path** as Lazarr (details below).

### 1. Compose

```yaml
services:
  lazarr:
    image: ghcr.io/your-username/lazarr:latest
    container_name: lazarr
    restart: unless-stopped          # required for the Web UI's Restart button
    ports:
      - "8780:8080"                  # qBittorrent API (arrs connect here)
      - "8781:8081"                  # Web UI
      - "8782:9090"                  # metrics /health (optional)
    volumes:
      - ./config:/config             # config.yaml + database
      - type: bind                   # shared data root (symlinks + FUSE mount)
        source: /srv/lazarr/data
        target: /data
        bind:
          propagation: rshared       # REQUIRED: propagates the FUSE mount
    cap_add:
      - SYS_ADMIN                    # REQUIRED to mount FUSE
    devices:
      - /dev/fuse
    security_opt:
      - apparmor:unconfined          # REQUIRED for FUSE in Docker
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:9090/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
```

Lazarr runs as **root inside the container** (FUSE mounting in Docker requires effective
`CAP_SYS_ADMIN`, which a non-root `user:` does not get). Instead pass your arrs' uid:gid as
`PUID`/`PGID` in Lazarr's environment (or set `ownership.puid/pgid` in the config). Lazarr
chowns every symlink it creates to that owner so the arrs can move them, and its virtual
drive reports files as owned by it and group-writable so arr upgrades can replace them.

Your **arr and media-server containers** must bind the same host dir at the same path so
the symlinks resolve, e.g. add to each:

```yaml
    volumes:
      - type: bind
        source: /srv/lazarr/data
        target: /data
        bind:
          propagation: rslave
```

### 2. Config

Copy [`config.example.yaml`](config.example.yaml) to `./config/config.yaml`, set
`torbox.api_key`, and adjust paths if needed. Start the container. From then on,
**everything is editable in the Web UI** (`http://<host>:8781` → Settings) — it rewrites
config.yaml for you and offers a one-click restart.

### 3. Connect your arrs

In each Sonarr/Radarr: **Settings → Download Clients → ➕ → qBittorrent**

| Field    | Value                                          |
|----------|------------------------------------------------|
| Host     | your Docker host's IP                          |
| Port     | `8780` (or your mapping)                       |
| Username | `lazarr` (config `qbit.username`)              |
| Password | `lazarr` (config `qbit.password`)              |
| Category | one unique name per arr, e.g. `radarr`, `sonarr_4k` — must match a `categories:` entry in Lazarr |

The Web UI's Settings page shows this exact table with your live values, and the
category editor adds new arrs in two clicks.

#### Different cache-miss handling for Seerr requests (optional)

`policy.on_cache_miss_by_category` sets the miss policy per category. A common split: requests
coming from Seerr stay **cached-only** (`reject`, the arr immediately tries its next
release), while your own direct adds may **wait** for TorBox to download. Stock Sonarr
v4 / Radarr route grabs by download-client **tags**: if any client's tags match the
series/movie tags, only those clients are used, otherwise only untagged clients
(`DownloadClientProvider.GetDownloadClient`). So:

1. In Lazarr, add the new categories and the policy, and tell Lazarr which arr owns
   them (for `wait_fail_blocklist`, release dates and the pack janitor):
   ```yaml
   categories: [sonarr_main, sonarr_main_seerr, radarr_main, radarr_main_seerr]
   policy:
     on_cache_miss: wait
     on_cache_miss_by_category: {sonarr_main_seerr: reject, radarr_main_seerr: reject}
   playback:
     arrs:
       - {category: sonarr_main, url: "http://sonarr_main:8989", also_categories: [sonarr_main_seerr]}
       - {category: radarr_main, url: "http://radarr_main:7878", also_categories: [radarr_main_seerr]}
   ```
2. In each arr, add a **second** qBittorrent client, e.g. "Lazarr Seerr", with the same
   host and port as the first, the category `<arr>_seerr` and the tag `seerr`. Leave the
   existing "Lazarr" client untagged.
3. In Seerr, set the tag `seerr` on each Sonarr/Radarr server (Settings → Services →
   *server* → Tags), so every series/movie it requests carries the tag.
4. If a queue cleaner such as Cleanuparr ignores the "Lazarr" client by name, add
   "Lazarr Seerr" to that list too.

#### Tested with

| Application | Version | Status |
|---|---|---|
| Radarr | v6.1.x | ✅ grab → import → playback, tested in production |
| Sonarr | v4.0.x | ✅ grab pipeline tested; season packs work via multi-file torrents |
| Plex | current | ✅ direct play through the FUSE mount |
| Lidarr / Readarr / Whisparr | — | ❓ untested; the qBittorrent emulation is generic |
| Jellyfin / Emby | — | ❓ untested; any player that reads files should work |

### Image

`ghcr.io/your-username/lazarr` is a placeholder: the release workflow
(`.github/workflows/release.yml`) publishes to `ghcr.io/<your GitHub user>/lazarr` when
you push a `v*` tag from your own fork, or swap `image:` for `build: .` to build locally.
If your package is private, the host needs a registry login (`ghcr.io` + a PAT with
`read:packages`). Keep `restart: unless-stopped`: it is what makes the Web UI's
*Restart Lazarr* button work.

---

## Configuration reference

All values live in `config.yaml` and are editable from the Web UI (the API key is
write-only there: it can be replaced but never displayed).

| Key | Default | What it does |
|---|---|---|
| `log_level` | `info` | `debug` / `info` / `warn` / `error` — applied live, no restart |
| `torbox.api_key` | — | your TorBox API key (**required**) |
| `torbox.api_base` | `https://api.torbox.app/v1/api` | API base URL |
| `torbox.extra_cdn_hosts` | `[]` | extra CDN host suffixes (e.g. `.tb-cdn.net`) if TorBox ships a CDN domain Lazarr doesn't know yet |
| `qbit.listen` | `:8080` | arr-facing qBittorrent API |
| `qbit.username` / `qbit.password` | `lazarr`/`lazarr` | credentials the arrs use |
| `categories` | `[]` | one per arr instance |
| `paths.download_dir` | — | where import symlinks are created (arr "save path") |
| `paths.fuse_mount` | — | the virtual drive mountpoint |
| `paths.db_path` | `/data/lazarr.sqlite` | catalog database |
| `paths.probe_cache_dir` | `/data/probe` | small on-disk header cache |
| `policy.allow_uncached` | `false` | accept releases TorBox hasn't cached yet |
| `policy.idle_ttl` | `168h` (7d) | remove from TorBox after this long unwatched |
| `policy.max_hold` | `720h` (30d) | absolute per-item ceiling on TorBox |
| `policy.active_slots` | `3` | concurrent items on TorBox (match your plan; `0` = auto) |
| `policy.probe_cache` | `true` | cache file headers so library scans don't cost adds |
| `policy.on_cache_miss` | `error` | `error` / `reject` (arr retries another release) / `wait` (TorBox downloads it) |
| `policy.on_cache_miss_by_category` | `{}` | per-category override of `on_cache_miss`, e.g. `{sonarr_main_seerr: reject}` (see "Different cache-miss handling for Seerr requests") |
| `policy.cache_wait_budget` | `15m` | `wait` mode: unconditional wait; past it a download keeps going only while progress is rising |
| `policy.cache_wait_max` | `0` (= 3× budget) | `wait` mode: hard ceiling for a still-progressing download; bail if TorBox's ETA lands beyond it |
| `policy.max_wait_downloads` | `1` | `wait` mode: concurrent TorBox downloads cap; overflow is refused (`Fails.`) |
| `policy.wait_no_progress` | `15m` | `wait` mode: no progress and 0 seeds/0 peers for this long = dead swarm, bail early; `0` = off |
| `policy.use_arr_release_name` | `true` | name the download folder after the arr's release title (magnet `dn` / .torrent name) instead of TorBox's cached name; TorBox's name is kept as `torbox_name` |
| `policy.refuse_add_errors` | `true` | never store an error row at add time: a checkcached failure/backoff, wait-cap overflow, failed uncached add or failed torrentinfo answers `Fails.` so the arr takes its next release |
| `policy.wait_fail_blocklist` | `false` | `wait` mode: a failed wait download is removed from the owning arr's queue with blocklist (the arr blocklists the release and searches for another) instead of sitting as a warning; needs that category in `playback.arrs` + `LAZARR_ARR_API_KEY_<CATEGORY>`. Also covers a content-gate refusal (accepted as an empty error row and failed the same way, so RSS stops re-grabbing the fake; a repeat of the same hash within 12h gets a plain `Fails.`) and a release the engine marks errored on dead-cache |
| `torbox.max_requests_per_minute` | `240` | Lazarr's own TorBox API budget (token bucket); TorBox allows 300/60 s per key. A 429 or `x-ratelimit-remaining: 0` pauses all TorBox calls until the window resets |
| `policy.requestdl_negative_ttl` | `90s` | after a requestdl failure, reads of that file fail fast for this long without calling TorBox; `0` = off |
| `policy.backoff_read_wait` | `0s` | during an API backoff, reads fail at once (`0`) or wait up to this long for it to end |
| `policy.content_gate` | `true` | refuse at add time (arr falls through; with `wait_fail_blocklist` the arr blocklists it) a cached release with an executable/script file (except `RARBG_DO_NOT_MIRROR.exe`), no video ≥ `min_video_mib` (`.iso` does not count as video), or archives per `reject_archives` |
| `policy.min_video_mib` | `100` | content gate: smallest qualifying video file; `0` = any size |
| `policy.reject_archives` | `only` | `only` = archives with no qualifying video; `any` = any archive file at all; `none` |
| `policy.detach_on_delete` | `true` | when an arr deletes a download whose files it already imported (partial season pack, queue cleaner, manual remove), keep the release hidden from the arr so the library symlinks keep playing; a daily sweep drops it once no library link is left. `false` = old behavior (imported links dangle) |
| `policy.pack_janitor` | `false` | every 5 min remove partially imported packs (importBlocked, or importPending on two consecutive passes; ≥1 file imported, every other file "not an upgrade"/"already imported"/"not a quality revision upgrade") from the arr queues **without** blocklist or re-search; needs `detach_on_delete` and `playback.arrs` |
| `policy.pack_janitor_clients` | `["Lazarr", "Lazarr Seerr"]` | arr-side download-client names the janitor may touch |
| `policy.repair_unlink_dead` | `false` | Daily repair scan removes library symlinks to releases TorBox evicted (two sightings ≥12h apart, ≤50 per run) so the arr re-grabs them |
| `policy.readahead_windows` | `4` | parallel 1 MiB prefetch depth; 0 = off, 4–8 for 4K |
| `slow_lane.enabled` | `false` | background TorBox downloads with no time budget for slow uncached releases (see "Slow lane") |
| `slow_lane.max_concurrent` | `1` | simultaneous slow-lane downloads (1–5; each holds a TorBox slot) |
| `slow_lane.poll_interval` | `10m` | how often progress is checked and queued items are started (1m–24h) |
| `slow_lane.max_age` | `168h` (7d) | give up on an item (deleting its TorBox download) this long after it was queued; `0` = never |
| `slow_lane.adopt_wait_failures` | `false` | move an `on_cache_miss: wait` download that failed only for being slow into the slow lane instead of deleting it |
| `slow_lane.auto_import` | `true` | finish "added" hand-offs the arr cannot import by name with the arr's manual import (missing episodes/movies only; see "Slow lane") |
| `slow_lane.auto_import_after` | `15m` | head start for the arr's own import, and the gap between auto-import attempts (1m–24h) |
| `missing.enabled` | `true` | the Web UI *Missing* tab: what the arrs in `playback.arrs` still want (see "Missing tab"); runs only when at least one arr has an API key |
| `missing.refresh_interval` | `30m` | how often each arr is asked for its list (about 3 requests per arr); 5m–24h. The tab's Refresh button can refresh sooner, at most once a minute |
| `repair.unlink_orphans` | `false` | the hourly sweep reports library symlinks into Lazarr whose release is gone (Web UI Health tab, `lazarr_dangling_library_links`); `true` removes them after 1 h (≤20 releases/run) and asks the owning arr (`playback.arrs`) to rescan so it searches again |
| `repair.search_replacement.enabled` | `false` | after `repair.unlink_orphans` / `policy.repair_unlink_dead` removed links and the arr's rescan shows those episodes/movies missing (≥10 min later), send the arr an EpisodeSearch / MoviesSearch for exactly them. Monitored items only (Sonarr: episode and series). Never touches TorBox. Metric `lazarr_replacement_search_total{result}` |
| `repair.search_replacement.max_per_hour` | `10` | searched episodes/movies per rolling hour across all arrs (1–30); the rest wait, at most 200 queued |
| `repair.search_replacement.cooldown` | `72h` | at most one search per episode/movie per this window (24h–720h), persisted across restarts, so a replacement that dies again cannot loop |
| `cleanup.remove_empty_dirs` | `false` (report only) | the hourly sweep reports empty folders (`lazarr_empty_dirs`): release folders under `download_dir/<category>/` whose release the arr no longer sees, and library folders emptied by the repair sweeps; `true` removes them (rmdir only, symlinks never followed; never a category folder, a library root folder or anything above them) |
| `cleanup.empty_dir_grace` | `1h` | a folder modified within this (it or any folder in it) is left alone; 5m–168h |
| `cleanup.sweep_library` | `false` | also sweep empty folders that already exist below each `playback.arrs[].library_root` every hour; skipped for an arr with `no_library_sweep`, an arr with "Create empty series/movie folders" on, and an arr Lazarr cannot ask |
| `audit.delete_orphans_older_than` | `0` (report only) | the audit lists account torrents no release references (Web UI Health tab, `lazarr_tos_audit_orphans`); `> 0` (min `1h`) deletes orphans older than that, max 10 per audit. On a shared key, other apps' torrents count as orphans too |
| `ownership.puid` / `pgid` | `0` | owner (your arrs' uid:gid) for created symlinks and virtual-drive files; env `PUID`/`PGID` (or `LAZARR_PUID`/`LAZARR_PGID`) override |
| `metrics.listen` | `:9090` | Prometheus `/metrics` + `/health`; empty = off |
| `webui.listen` | `:8081` | Web UI; empty = off |
| `webui.username` / `password` | — | enable HTTP Basic Auth (recommended) |
| `local_cache.enabled` | `false` | watch-aware local disk cache (see below) |
| `local_cache.dir` | `/cache` | container path of the cache volume |
| `local_cache.publish` / `publish_at` | `true` / `<fuse_mount parent>/lazarr_cache` | bind-mount the cache next to the FUSE tree so every peer container sees it |
| `local_cache.profile` | `balanced` | `conservative` / `balanced` / `generous` retention presets |
| `local_cache.free_high_pct` / `free_low_pct` / `reserve_pct` | `20` / `5` / `2` | disk-aware scaling and eviction thresholds |
| `local_cache.ttl.*` | per profile | retention per class: `watching`, `finished`, `finished_movie` (0 = same as `finished`), `untouched`, `new_movie`, `default` |
| `local_cache.pull_on_grab` | `[]` | categories whose grabs pull to disk immediately |
| `local_cache.prefetch_ahead` | `2` | episodes kept local ahead of each viewer |
| `local_cache.season_fill` | off / `2` / `30` | `enabled` / `after_completed` / `max_episodes`: pull the rest of a season once a viewer has finished that many of its episodes |
| `local_cache.speculative_gb_per_day` | `250` | rolling 24 h cap on speculative (season-fill) pulls; live-session pulls are never capped; 0 = none |
| `local_cache.torrent_only` | `true` | never cache usenet-sourced releases |
| `playback.plex.url` / `playback.jellyfin.url` | — | media servers to poll; tokens via `LAZARR_PLEX_TOKEN` / `LAZARR_JELLYFIN_API_KEY` |
| `playback.arrs[].library_root` | — | the arr's root folder as Lazarr sees it (e.g. `/data/TV Shows/sonarr_main` for an arr whose root folder is `/tv`). Maps the arr's paths so the repair sweeps (`repair.unlink_orphans`, `policy.repair_unlink_dead`) can ask the right arr to rescan, and bounds the empty-folder cleanup. Unset = auto-detected from the library links for rescans (one WARN and no rescans for that arr if that fails); no library cleanup |
| `playback.arrs[].no_library_sweep` | `false` | keep `cleanup.sweep_library` off this arr's root |
| `playback.arrs[].also_categories` | `[]` | further Lazarr categories the same arr grabs with (e.g. its `_seerr` client); they share its URL and API key |
| `playback.arrs[]` | `[]` | optional Radarr/Sonarr per category for release dates (and `policy.wait_fail_blocklist`); keys via `LAZARR_ARR_API_KEY_<CATEGORY>` |
| `playback.poll_interval` | `30s` | session poll cadence (0 = off) |

### Slow lane (optional)

`on_cache_miss: wait` is built for releases TorBox can fetch in minutes: it holds the
arr's grab open and gives up after `cache_wait_budget`. Old shows, multi-season packs and
thin swarms can take hours or days. With `slow_lane.enabled: true` those go to the
**slow lane** instead — a persistent queue (it survives restarts) worked in the
background:

- **Add:** paste a magnet link or infohash on the Web UI's *Slow lane* tab, or upload
  `.torrent` files there (file picker or drag-and-drop onto the card). Over the API, on
  the Web UI port (`webui.listen`; add `-u user:pass` when a Web UI login is set):

  ```sh
  # magnet link or bare infohash (JSON)
  curl -H 'Content-Type: application/json' \
    -d '{"magnet": "magnet:?xt=urn:btih:...", "category": "sonarr_main"}' http://<host>:8081/api/slowlane
  # one or more .torrent files (multipart; repeat -F torrent=@… for more)
  curl -F category=sonarr_main -F torrent=@file.torrent http://<host>:8081/api/slowlane
  ```

  Magnets copied from HTML/JSON (`&amp;` separators, e.g. the EZTV API's `magnet_url`)
  are cleaned first, so the name and every tracker survive. An optional JSON
  `"target": {"series_id": 91, "season": 2, "movie_id": 0, "title": "..."}` (every field
  optional, season 0 = any) says what the download is for; the automatic import below
  then only imports files that map to it. A multipart upload takes the same object as a
  `target` field (`-F 'target={"series_id": 91, "season": 2}'`), applied to every file
  in it. A series target for a category whose arr is Radarr (or a movie target for a
  Sonarr one) is refused: the kind comes from `playback.arrs` (`kind`), else the
  category name (`sonarr…` / `radarr…`). The Web UI also warns before queueing a name
  with `SxxEyy` / `Sxx` / `Season N` in it for a Radarr category.

  A `.torrent` is queued under its v1 infohash with its trackers (`announce` +
  `announce-list`) kept in the magnet TorBox gets, which thin swarms need. Refused:
  **private** torrents (TorBox would announce with your private-tracker passkey, and
  private swarms do not work from TorBox anyway), BitTorrent v2-only torrents (no v1
  infohash; hybrid v1+v2 ones are fine), files over 2 MiB, and more than 32 files or
  16 MiB per request. The multipart answer lists every input with its queued item or its
  error (one bad file does not block the others): 200 if anything was queued, 409 if
  everything was already in the slow lane, else 400. With `adopt_wait_failures`, a wait
  download about to be dropped only for being slow is moved here automatically; the arr
  still sees that grab fail and moves on.
- **Download:** at most `max_concurrent` items run on TorBox at once; every
  `poll_interval` Lazarr checks progress and starts queued items. There is no time budget,
  only `max_age`. Every call goes through the TorBox rate-limit governor: a backoff, a 429
  or TorBox's hourly uncached-add cap simply defers the start to the next check.
- **Hand-off:** once TorBox has it cached, the account copy is released and the content
  gate checks the real file list (no executables, no archives, a real video). Then:
  - the arr's own grab still in Lazarr (in error state): it completes in place;
  - the arr's history has a grab of this hash (it gave up on it): Sonarr/Radarr never
    import a download id whose last event is "failed", so Lazarr clears that blocklist
    entry and asks the arr to search those episodes / that movie again (needs the
    category in `playback.arrs`); the new grab of the now-cached release is instant;
  - otherwise ("added") it appears in the category as a completed download and the arr
    imports what it can parse from the folder name. That is not always enough: Sonarr
    rejects or half-imports multi-season packs (Lindy Mercer S01-S02 imported only
    S01), and imports nothing under a foreign-language folder name or a `5x01`-style
    folder.
- **Automatic manual import** (`auto_import`, on by default): `auto_import_after` (15m)
  after an "added" hand-off, Lazarr asks the arr for
  `GET /api/v3/manualimport?folder=<the download's content_path>&filterExistingFiles=true`
  (no `seriesId`: with it Sonarr lists the series' library folder instead), which parses
  every FILE on its own, and posts a `ManualImport` command (`importMode: move`,
  `downloadId` = the hash) for the files that are safe:
  - no rejections from the arr;
  - the whole folder maps to ONE series (Sonarr) / movie (Radarr), or the item's
    `target` decides; files of other series or (with `target.season`) other seasons are
    skipped. A folder mapping to several series without a target is left alone, with
    the reason on the item;
  - only monitored episodes / movies that have NO file yet: never a replacement, so a
    re-run is a no-op for what is already in the library; two files for one episode, or
    several importable files for one movie, are skipped.

  Lazarr waits for the command (bounded), then re-reads the episodes / movie to count
  what really imported, and notes it on the item (e.g. `auto manual import: 34 file(s)
  S02E01–E34`). Rejections, a folder the arr lists as empty, or a failed import are
  retried `auto_import_after` later; at most 3 attempts per item, 2 items per check. A
  file the arr rejects only because the episode / movie already has a file ("Not an
  upgrade", "Not a quality revision upgrade", "already imported") counts as *has file*,
  not as a rejection: when nothing else is left, the item is done at once ("nothing left
  to import").
  Needs the category in `playback.arrs`. Metric: `lazarr_slowlane_events_total{event="auto_import"}`.
- **Status:** state, progress, swarm and the hand-off detail per item on the *Slow lane*
  tab (cancel deletes a running TorBox download; one already deleted on the TorBox
  dashboard counts as deleted); metrics `lazarr_slowlane_items{state}`
  and `lazarr_slowlane_events_total`. Running downloads count as held in the ToS audit.
  A download removed from the TorBox account before it finished (deleted on the
  dashboard, or TorBox's no-progress-for-2-days sweep) fails as `removed_on_torbox`
  instead of being re-added; enqueue it again to retry.

Each running slow-lane download holds one of your plan's TorBox slots next to playback and
wait downloads, so keep `max_concurrent` low (TorBox Essential has 3).

### Missing tab

The Web UI's *Missing* tab answers "what do my arrs still need?" without ad-hoc scripts
against each arr's *Wanted → Missing*. For every arr in `playback.arrs` with an API key,
Lazarr keeps a list of:

- **Sonarr:** monitored episodes that have already aired and have no file, grouped by
  show and season: episode ranges (`E1–4, E8, E10–12`) and count, first/last air date,
  quality profile, whether the season is monitored;
- **Radarr:** monitored movies with no file that are available (past their minimum
  availability);

each with a *downloading* badge when the arr already has it in its queue, and an
*in slow lane* badge when an active slow-lane item was added for it (its `target`). The
list is refreshed every `missing.refresh_interval` (default 30 min; one `wanted/missing`
page per 500 items, one `qualityprofile` and one `queue` request per arr, arrs asked one
after the other) and on demand with the tab's *Refresh* button (at most once a minute).
An arr that fails keeps its previous list and shows the error; the others are unaffected.

Per row:

- **Search** asks the arr to search its indexers (`SeasonSearch`, or `EpisodeSearch` when
  a season misses a single episode; `MoviesSearch`). This is the tab's only write to an
  arr; a row can be searched once per 5 minutes.
- **Copy search text** copies e.g. `Show Name S02` / `Movie Name 2004` for searching a
  torrent site by hand.
- **Add torrent…** opens the *Slow lane* tab with the row's arr selected and a
  "for: Show Name S02 ✕" chip; the magnet you add (or the `.torrent` you upload) is sent with that target
  (`{"magnet": "...", "category": "sonarr_main", "target": {"series_id": 75, "season": 2, "title": "Show Name S02"}}`,
  movies `{"movie_id": 202, "title": "Movie (2004)"}`).

API (webui port): `GET /api/missing` returns
`{enabled, refreshed_at, refreshing, refresh_interval, arrs: [{category, kind, error?, total_missing, queued, refreshed_at, items: [...]}]}`;
`?refresh=1` refreshes first (HTTP 429 + `Retry-After` inside the one-minute gap).
`POST /api/missing/search` takes `{"category": "sonarr_main", "series_id": 75, "season": 2}`
(or `"episode_ids": [...]`) / `{"category": "radarr_main", "movie_id": 202}` and returns
`{category, command, command_id}`. Metrics: `lazarr_missing_items{category}`,
`lazarr_missing_refreshes_total{category,result}`, `lazarr_missing_searches_total{command,result}`.

### Local cache (optional)

With `local_cache.enabled: true` Lazarr keeps **real files on your own disk** for what is
actually being watched, in front of the streaming path above:

- **Signal:** it polls Plex (`/status/sessions`) and/or Jellyfin (`/Sessions`) and joins
  each playing item to its release by reading the library symlink — no path guessing.
- **Pull:** the current episode and the next `prefetch_ahead` episodes (crossing season
  boundaries) are streamed once through the normal engine into
  `<dir>/<infohash>/<rel_path>`, a twin of the FUSE tree. Movies pull when played; movie
  categories listed in `pull_on_grab` start pulling at grab time instead (heavy: one grab
  wave can pull a terabyte). With `season_fill` on, the rest of a season is pulled once a
  viewer has finished `after_completed` episodes of it, within `speculative_gb_per_day`.
- **Switch:** when a pull completes the library symlink is atomically retargeted from the
  FUSE path to the real file. Plex/Jellyfin index the symlink path, which never changes,
  so nothing needs a refresh. Plex still notices the change and, with its partial scan
  off, rescans the whole section each time; turn partial scan on. Before eviction the
  link is switched back.
- **Retention:** per file, per user (two viewers on different seasons each keep their
  own window hot), classed as *wanted / watching / new movie / default / untouched /
  finished*, with windows that shrink as the volume fills and an ordered eviction below
  the low-water mark. Nothing wanted by a live viewer is ever evicted. A pull that would
  take free space to or below `free_low_pct` first evicts in that same order to make
  room, and never starts if it would leave less than `reserve_pct` free.
- **Not cached, ever:** usenet-sourced releases (`torrent_only`), uncached or evicted
  content, files under `min_file_mib`.

Deployment: bind-mount your big volume at `local_cache.dir` on the Lazarr service only.
With `publish: true` Lazarr bind-mounts it at `publish_at` (a sibling of the FUSE mount)
inside its own mount namespace, and the existing rshared data mount propagates it to
Plex, Jellyfin and the arrs exactly like the FUSE mount. Secrets go in the compose
environment:

```yaml
    environment:
      - LAZARR_PLEX_TOKEN=...
      - LAZARR_JELLYFIN_API_KEY=...
      - LAZARR_ARR_API_KEY_RADARR=...      # one per category you list under playback.arrs
    volumes:
      - /path/to/big/disk/lazarr-cache:/cache
```

`stable_mtime` is off by default: turning it on makes the FUSE tree report each release's
grab time as its file date (so a cached copy is indistinguishable from the streamed one
to a scanner), but it changes the date of every existing file once, which can trigger a
one-time library re-analysis.

**Disk-pressure test.** `internal/localcache/pressure_test.go` (build tag `pressure`, not
part of `go test ./...`) drives the real puller, planner, reaper, link index and sqlite
catalog against a private 512 MiB tmpfs with 40-80 MiB synthetic files: sustained growth
past capacity, the reserve refusal and retry, another process filling the disk, and the
TTL scaling between the water marks. The test mounts the tmpfs itself, so it needs root,
and it unmounts it when it finishes:

```sh
sudo go test -tags pressure -run TestPressure -v -count=1 ./internal/localcache/
# no root? mount a small scratch fs yourself and point the test at it:
LAZARR_PRESSURE_DIR=/path/to/small/mount go test -tags pressure -run TestPressure -v ./internal/localcache/
```

It refuses to run on a filesystem larger than 4 GiB or under `/mnt`, `/cache`, `/config`,
`/data` or `/debrid_symlinks`. `LAZARR_PRESSURE_VERBOSE=1` echoes the engine log, and
the `RESULT` lines summarise each scenario. If a run is killed, remove the leftover mount
with `umount "${TMPDIR:-/tmp}"/lazarr-pressure-*`.

---

## Security & reverse proxy

- **Set a Web UI login** (Settings → Web UI login, or `webui.username/password`). The
  dashboard can change every setting including the TorBox key — and, with the local
  cache configured, the Plex/Jellyfin/arr URLs its tokens are sent to and the directory
  it publishes into the shared tree — so on anything but a fully trusted LAN, auth on.
- The arr-facing qBittorrent port uses the configured username/password; the metrics
  port is unauthenticated — keep both LAN-only (don't port-forward any of them).
- **Reverse proxy (optional):** if you want the Web UI reachable from outside, put it
  behind your proxy with TLS, e.g. Caddy:

  ```
  lazarr.example.com {
      reverse_proxy 192.168.1.10:8781
  }
  ```

  or nginx:

  ```nginx
  location / {
      proxy_pass http://192.168.1.10:8781;
      proxy_set_header Host $host;
  }
  ```

  Only ever expose the Web UI port (8781) — never the qbit (8780) or metrics (8782)
  ports. Plain HTTP Basic Auth over the internet requires TLS at the proxy.

## Privacy

Lazarr talks to exactly two endpoints: `api.torbox.app` and TorBox's content CDN
(`*.tb-cdn.io` / `*.tb-cdn.earth`, extensible via `torbox.extra_cdn_hosts`) — plus, only
when you configure them, your own Plex/Jellyfin servers and Radarr/Sonarr instances on
your LAN (read-only session and metadata calls; see the local cache section). **No
telemetry, no phone-home, no update checks, no analytics.** The Web UI is fully embedded
in the binary (no external fonts/scripts). See [ARCHITECTURE.md](ARCHITECTURE.md) for the
full internals and the TorBox-only provider constraints.

---

## Observability

- `GET :8782/health` — JSON: mount health, slots in use, last audit.
- `GET :8782/metrics` — Prometheus counters (grabs, materializations, releases, link
  refreshes, rate limits, **`lazarr_tos_audit_leaks`** — alert if ever non-zero).
- Web UI → Logs — recent records with level filter; full stream in `docker logs`.

## FAQ

**Does playback ever touch my disk?** Not by default — bytes are range-proxied from
TorBox's CDN through the FUSE mount to your player; the only local state is the SQLite
catalog, symlinks, and a tiny header cache. With the optional local cache enabled,
what is being watched is kept as real files on the volume you point it at, and evicted
by its retention policy.

**What happens if Lazarr crashes mid-stream?** On restart it clears the stale FUSE
mount, reconciles with TorBox, and releases anything it left behind. Your library
links are untouched.

**What if TorBox evicts content I imported?** The daily availability scan marks it in
the Web UI's Health tab; one click deletes the dead link so your arr re-searches a
fresh copy.

**Lidarr/Readarr/other clients?** The qBittorrent emulation is generic; only
Sonarr/Radarr are tested so far.

## License

[MIT](LICENSE)
