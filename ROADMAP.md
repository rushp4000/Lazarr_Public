# Roadmap

Work that is built but off by default, or still open. Each entry says what state it's in
and what would unblock it.

---

## Replacement search after TorBox drops content — **built, off by default**

`repair.search_replacement` (internal/replsearch). When TorBox drops content a library
file depends on, the repair sweeps remove the dead link and the arr's rescan marks the
episode/movie missing, but nothing searches for it. With this on, Lazarr reads which
episodes/movies the removed links were (from the arr's file records, before the rescan
deletes them), waits ≥10 min for the rescan, and sends one EpisodeSearch / MoviesSearch
per batch for monitored items that are still missing.

Bounded on purpose (TorBox flags misconfigured automated tools): one search per item per
`cooldown` (72 h, persisted in the `replacement_search` table), `max_per_hour` (10)
across all arrs, at most 200 queued, 3 attempts / 24 h per item. It never touches
TorBox. The arr's grab goes through the normal add path; with `on_cache_miss: wait` an
uncached pick becomes a bounded wait download, as for any other grab.

**To enable:** set `repair.search_replacement.enabled: true` (needs
`policy.repair_unlink_dead` or `repair.unlink_orphans`, and the arr in `playback.arrs`
with `library_root`). Watch `lazarr_replacement_search_total{result}` for a few days.

---

## Watch-aware local disk cache — **shipped, opt-in**

An opt-in local disk cache in front of the TorBox streaming path: Lazarr polls Plex /
Jellyfin sessions, pulls the current and next episodes (and movies on play) to a twin of
the FUSE tree on your own volume, atomically retargets the library symlinks the arrs
created so the media server reads the real file, and evicts by a per-file, per-user,
disk-aware policy that switches the links back first. Usenet grabs are excluded
structurally. The TorBox account lifecycle is untouched.

Notes for operators:

- **`pull_on_grab`** can pull a terabyte in hours during a big grab wave; start with
  movies pull-on-play only.
- **Plex partial scan** should be on, so a retarget doesn't rescan the whole section.
- **Intro/credits analysis (`playback.analysis`)** is built and off by default.

Still open:

1. **Direct-play check** on Plex for a retargeted file.
2. **Arr upgrade/rename of a cached file:** the link index should follow the move and
   leave no orphan cache copy.
3. **Intro/credits markers.**

Code map: `internal/localcache/policy.go` (eviction), `links.go` (the only code that
touches the library) and `engine.go` (the decorator). Live state: Web UI Cache tab and
the `lazarr_localcache_*` metrics.

---

## Native NZB / Usenet support — **not started in this tree**

Idea: Lazarr also emulates a **SABnzbd** client, so an arr can be pointed at usenet
indexers and NZB releases get the same lazy-materialize lifecycle (nothing hoarded at
grab time, materialize on playback, proxy the stream, release when idle). The design
goal is symmetry: a SABnzbd server shaped like the qBit server, and the materialize
engine dispatching on a `Release.Source` field rather than forking.

Blocker: TorBox's usenet create endpoints require the Pro plan. The read-only ones
(`checkcached`, `mylist`) answer on lower tiers, so casual probing makes usenet look
available — the entitlement gate only bites at `createusenetdownload`. Verify response
shapes against a live Pro account before trusting any SDK docs.
