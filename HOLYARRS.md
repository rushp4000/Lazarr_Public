# Lazarr and HolyArrs: how this could fit the debrid roadmap item

Written for the HolyArrs maintainer. Context: HolyArrs issue #130 (download through a
debrid service, with TorBox included) and its sub-issue #444 (use Decypharr as a
qBittorrent downloader).

## What Lazarr is, in one paragraph

A single Go binary that looks like qBittorrent to Sonarr/Radarr but never downloads.
On grab it calls TorBox's read-only `checkcached`, records the file list, and exposes
the release as full-size files on a FUSE mount, so the arr imports instantly. On first
read (a play), it adds the torrent to TorBox, gets a CDN link and range-proxies the
bytes. After an idle window it deletes the torrent from TorBox again. The TorBox
account stays near-empty, which is what TorBox's retention rules expect; tools that
add at grab time fill the account and lose files to the ~30-day purge.

TorBox only. MIT licensed, so any part of it can go into HolyArrs (AGPL or your
commercial licence) without asking.

## How it differs from Decypharr (#444)

Different trade-offs rather than a drop-in replacement. Both are Go and MIT.

| | Lazarr | Decypharr |
|---|---|---|
| What the debrid account holds | Only what is playing or was played recently: added on first read, deleted after `idle_ttl` (default 7 d) | The library: torrents are sent to the provider at grab, and its repair worker re-inserts missing ones |
| Grab | Read-only `checkcached`, no account change; the arr imports instantly | Add to the provider |
| TorBox's ~30-day purge of untouched content | Nothing sits untouched on the account, so nothing to purge; the next play re-adds | Untouched library items can be purged and need repair |
| Account hygiene | Slot-aware (plan's concurrent limit, LRU eviction), 5-min audit that diffs the account against what Lazarr thinks it holds, crash-safe boot reconciliation | — |
| qBit `sync/maindata` (what HolyArrs reads) | Implemented | 404 in 2.7.2, per #444 |
| Providers | TorBox only | Real-Debrid, TorBox, Debrid-Link, AllDebrid, Premiumize |
| Usenet | No | Yes (NNTP streaming) |
| Maturity | Early, one operator, AI-developed | v2.x, large user base, many contributors |

In short: for TorBox users, Lazarr keeps the library working without parking it on
the account; for every other provider, or Usenet, Decypharr is the tool. HolyArrs could
support both through the same qBit-compatible path.

## Three ways it could fit, cheapest first

### 1. Sidecar, no HolyArrs code: Lazarr as a qBittorrent-compatible client

Lazarr implements the qBit WebUI endpoints Sonarr/Radarr use, **including
`sync/maindata`** (the 404 that blocks Decypharr in #444). It always answers with a
full snapshot (`rid: 1`, `full_update: true`). Not yet tested against HolyArrs.
Things to check:

- **`torrents/files` names are relative to the release folder** (`content_path`), not
  prefixed with the torrent's root folder: the same shape #444 reports for Decypharr.
- **Imports are symlinks** into the FUSE tree (`download_dir/<category>/<name>/<file>`
  → `fuse_mount/<hash>/<file>`), so HolyArrs must follow links
  (`HOLYARRS_FOLLOW_LINKS=1`, #158) and must not hardlink or copy them.
- **A grab is "complete" immediately** (progress 1.0, state `pausedUP`), except in
  `on_cache_miss: wait` mode, where real TorBox progress is reported.
- One category per HolyArrs library/quality tier, as with the arrs.

### 2. A native "TorBox (stream on demand)" client type

A thin client in HolyArrs that talks to Lazarr's own API instead of the qBit emulation,
so the UI can show what is actually going on: cached / waiting for TorBox / evicted
from TorBox's cache / held for playback. Useful existing endpoints:
`POST /api/slowlane` on the Web UI port (magnet or infohash + category + optional
series/season/movie target), and `GET /health` + Prometheus `/metrics` on the metrics
port. A small `/api/release/{hash}`
status endpoint could be added on Lazarr's side if this direction is interesting.

### 3. Parts for the planned built-in downloader

Both projects are Go. Packages that could be lifted more or less whole:

| Package | What it gives you |
|---|---|
| `internal/torbox` | TorBox client: error taxonomy (link expired, not found, rate-limited), and a governor for TorBox's 300 req / 60 s per-key limit and the 60/h uncached-add cap |
| `internal/materialize` | Slot accounting for the plan's concurrent-download limit, LRU eviction, add-on-play / release-on-idle lifecycle, SSRF-pinned range proxy with link refresh on expiry and dead CDN nodes, ToS audit (diffs the account against what it believes it holds) |
| `internal/vfs` | Read-only FUSE tree `/<infohash>/<path>` |
| `internal/qbit/gate.go` | Content gate: refuses cached releases with executables, a lone `.iso`, or archives with no real video (overlaps HolyArrs' fake/malware release filter) |
| `internal/repair`, `internal/replsearch` | Daily re-check of the whole catalog against TorBox's cache, dangling-link cleanup, bounded replacement search. These match the repair ideas raised in #130 |

**The catch:** FUSE in Docker needs `CAP_SYS_ADMIN`, `/dev/fuse`, `apparmor:unconfined`
and an `rshared` bind for the media servers to see the mount. That is a much heavier
footprint than HolyArrs' single small unprivileged container, which argues for keeping
the streaming half as an optional sidecar (options 1 or 2) and pulling only the
TorBox client, content gate and repair logic into HolyArrs itself.

## Honest caveats

- Early, single-tenant, TorBox-only. Real-Debrid has no free `checkcached` any more, so
  the "nothing added at grab time" trick does not carry over directly
  (`ARCHITECTURE.md` lists what a second provider would need).
- 100% AI-developed (Claude), driven by one person. Read the code accordingly.
- No support commitment; fork or copy freely.

Start with `README.md` (how it works, config), then `ARCHITECTURE.md` (internals and
the TorBox contract).
