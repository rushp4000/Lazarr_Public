package materialize

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/constants"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/torbox"
)

// AuditTOS is the compliance proof (docs/12 §guardrail 2). It diffs TorBox's mylist against
// what Lazarr believes it has materialized and alarms on leaks — anything the account still
// holds within LAZARR'S SCOPE that Lazarr believes is released.
//
// The account may be SHARED with another debrid tool that keeps its own torrents there,
// so the audit is scoped to Lazarr-added torrent_ids only: it never inspects, and never
// alarms on, ids Lazarr never added. Scoping uses the union of (a) ids Lazarr currently
// believes are materialized (Store.MaterializedIDs) and (b) ids Lazarr has added during
// this process lifetime (the seen set), minus what is currently believed-held.
//
// Returns an error only on an API failure; a detected leak is logged/alarmed, not returned
// as an error (a leak is an operational alarm, not a call failure).
// auditDriftGrace suppresses the drift WARN for items materialized very recently:
// TorBox's mylist lags a fresh createtorrent by up to ~a minute even with
// bypass_cache=true (observed live 2026-06-10: add at 05:06:10, mylist still
// missing it at the 05:07:12 audit, present minutes later). A fresh add that has
// not propagated yet is not drift.
const auditDriftGrace = 10 * 60 // seconds

func (m *materializer) AuditTOS() error {
	matRels, err := m.store.MaterializedReleases()
	if err != nil {
		return fmt.Errorf("materialize: audit: materialized releases: %w", err)
	}
	believedSet := make(map[int64]struct{}, len(matRels))
	matAt := make(map[int64]int64, len(matRels)) // id -> MaterializedAt (drift grace)
	for _, r := range matRels {
		believedSet[r.TorBoxID] = struct{}{}
		matAt[r.TorBoxID] = r.MaterializedAt
	}
	// on_cache_miss=wait downloads legitimately sit on the account while TorBox
	// fetches them — believed-held, never a leak. The qbit wait-poller owns their
	// removal (complete or bail).
	dlRels, err := m.store.DownloadingReleases()
	if err != nil {
		return fmt.Errorf("materialize: audit: downloading releases: %w", err)
	}
	for _, r := range dlRels {
		believedSet[r.TorBoxID] = struct{}{}
		matAt[r.TorBoxID] = r.MaterializedAt
	}
	// Slow-lane downloads (v1.2.9) are held on purpose too; the slow-lane worker owns
	// their removal and notices one that vanishes, so they stay out of the drift check.
	elsewhere := map[int64]struct{}{}
	if m.heldElsewhere != nil {
		ids, err := m.heldElsewhere()
		if err != nil {
			return fmt.Errorf("materialize: audit: held elsewhere: %w", err)
		}
		for _, id := range ids {
			believedSet[id] = struct{}{}
			elsewhere[id] = struct{}{}
		}
	}

	// Lazarr's scope = ids we currently believe held + ids we ever added this lifetime.
	scope := make(map[int64]struct{}, len(believedSet))
	for id := range believedSet {
		scope[id] = struct{}{}
	}
	m.mu.Lock()
	for id := range m.seen {
		scope[id] = struct{}{}
	}
	for _, ent := range m.track {
		// In-memory truth: these are genuinely materialized right now.
		believedSet[ent.torboxID] = struct{}{}
		scope[ent.torboxID] = struct{}{}
	}
	m.mu.Unlock()

	// Pull the whole account (paged), but only reason about ids in Lazarr's scope.
	held := make(map[int64]struct{})
	var account []torbox.TorrentDetail
	for offset := 0; ; offset += constants.MyListPageMax {
		page, err := m.tb.MyList(offset)
		if err != nil {
			return fmt.Errorf("materialize: audit: mylist offset %d: %w", offset, err)
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			held[d.ID] = struct{}{}
		}
		account = append(account, page...)
		if len(page) < constants.MyListPageMax {
			break
		}
	}

	var leaks, missing int
	// Leak: an id within Lazarr's scope that the account still holds but that we do NOT
	// currently believe is materialized => we released it (or never tracked it) yet it
	// lingers. This is the ToS alarm.
	for id := range scope {
		_, stillHeld := held[id]
		_, believedHeld := believedSet[id]
		if stillHeld && !believedHeld {
			leaks++
			m.log.Error("TOS AUDIT: leaked torrent on account (believed released)", "torbox_id", id)
		}
	}
	// Drift (informational): we believe an id is held but the account does not have it
	// (TorBox purged it, or an out-of-band delete). Not a ToS violation; surfaced for ops.
	// Items materialized within the grace window are skipped — mylist propagation lag,
	// not drift.
	now := m.now().Unix()
	for id := range believedSet {
		if _, ok := elsewhere[id]; ok {
			continue
		}
		if _, ok := held[id]; !ok {
			if at, known := matAt[id]; known && at > 0 && now-at < auditDriftGrace {
				m.log.Debug("TOS AUDIT: fresh add not yet visible in mylist (grace)", "torbox_id", id)
				continue
			}
			missing++
			m.log.Warn("TOS AUDIT: materialized id not present on account (drift)", "torbox_id", id)
		}
	}

	if leaks == 0 && missing == 0 {
		m.log.Info("TOS AUDIT: clean", "in_scope", len(scope), "believed_held", len(believedSet))
	}
	metrics.SetTosAuditLeaks(leaks)
	m.auditOrphans(account, believedSet)
	m.lastAudit.Store(m.now().Unix())
	return nil
}

// orphanMaxDeletesPerRun bounds audit.delete_orphans_older_than per audit.
const orphanMaxDeletesPerRun = 10

// auditOrphans (v1.2.8) lists account torrents that NO catalog row references: not
// believed held (materialized / downloading), not tracked, not mid-materialize, and
// older than the mylist propagation grace. The scoped leak check above only sees ids
// added in this process lifetime, so leftovers from earlier lifetimes (6 June items on
// 2026-10-11) were invisible. Report only unless audit.delete_orphans_older_than > 0.
func (m *materializer) auditOrphans(account []torbox.TorrentDetail, believed map[int64]struct{}) {
	now := m.now()
	m.mu.Lock()
	busy := make(map[string]struct{}, len(m.track)+len(m.inflight))
	for h, ent := range m.track {
		busy[h] = struct{}{}
		believed[ent.torboxID] = struct{}{}
	}
	for h := range m.inflight {
		busy[h] = struct{}{}
	}
	m.mu.Unlock()

	rep := OrphanReport{At: now.Unix(), Deletes: m.deleteOrphansAfter > 0}
	deleted := 0
	for _, d := range account {
		if _, ok := believed[d.ID]; ok {
			continue
		}
		hash := strings.ToLower(d.Hash)
		if _, ok := busy[hash]; ok {
			continue
		}
		if d.CreatedAt.IsZero() || now.Sub(d.CreatedAt) < auditDriftGrace*time.Second {
			continue // fresh (or unknown-age) add: a materialize may not be registered yet
		}
		o := AccountOrphan{TorBoxID: d.ID, Hash: hash, Name: d.Name, State: d.DownloadState, CreatedAt: d.CreatedAt}
		if rel, _, err := m.store.GetRelease(hash); err == nil && rel != nil {
			o.InCatalog = true
		}
		if m.deleteOrphansAfter > 0 && now.Sub(d.CreatedAt) >= m.deleteOrphansAfter && deleted < orphanMaxDeletesPerRun {
			if err := m.tb.ControlDelete(d.ID); err != nil {
				m.log.Warn("TOS AUDIT: orphan delete failed", "torbox_id", d.ID, "err", err)
			} else {
				o.Deleted = true
				deleted++
				m.log.Warn("TOS AUDIT: deleted account orphan (audit.delete_orphans_older_than)",
					"torbox_id", d.ID, "hash", short(hash), "name", d.Name, "added", d.CreatedAt.UTC().Format(time.RFC3339))
			}
		}
		rep.Items = append(rep.Items, o)
	}
	m.orphanMu.Lock()
	prev := len(m.orphans.Items)
	m.orphans = rep
	m.orphanMu.Unlock()
	if n := len(rep.Items); n > 0 {
		lvl := slog.LevelDebug // unchanged list: keep the 5-minute audit quiet
		if n != prev || deleted > 0 {
			lvl = slog.LevelWarn
		}
		m.log.Log(context.Background(), lvl, "TOS AUDIT: account holds torrents no Lazarr release references (orphans)",
			"count", n, "deleted", deleted, "delete_enabled", rep.Deletes)
	}
	metrics.SetTosAuditOrphans(len(rep.Items) - deleted)
}

// Orphans returns the last orphan report (for the Web UI). Not in the Engine interface.
func (m *materializer) Orphans() OrphanReport {
	m.orphanMu.Lock()
	defer m.orphanMu.Unlock()
	rep := m.orphans
	rep.Items = append([]AccountOrphan(nil), rep.Items...)
	return rep
}
