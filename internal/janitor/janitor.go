// Package janitor removes partially imported season packs from the arrs' queues
// without failing them (policy.pack_janitor, v1.2.8, opt-in).
//
// When an arr imports only some episodes of a cached season pack (the rest are not
// upgrades of files it already has), Sonarr's CompletedDownloadService leaves the
// download ImportBlocked FOREVER and re-runs import, i.e. three ffprobes per file
// through FUSE, every minute. Any remover so far (another tool's queue cleaner,
// Cleanuparr strikes, a human) used blocklist=true, which blocklists a pack that
// actually worked and fires a season search. The janitor instead removes such an item
// with blocklist=false&skipRedownload=true; with policy.detach_on_delete the resulting
// torrents/delete keeps the imported episodes playing.
//
// Sonarr parks the same leftovers in importPending as well as importBlocked (seen live
// 2026-10-12: ten force-grabbed packs sat in importPending with only "Not an upgrade" /
// "already imported" messages). importPending is also the brief state of a download that
// has just completed, so such an item is removed only when it qualified on two
// consecutive passes (at least one Interval apart).
package janitor

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/playback/arr"
)

// Interval is how often the janitor scans every arr queue.
const Interval = 5 * time.Minute

// maxRemovalsPerRun bounds one pass (blast radius if an arr ever misreports).
const maxRemovalsPerRun = 20

// allowedRejections are the ONLY import rejections that make a blocked download safe
// to drop: every not-imported file is already covered by a file the arr has (Sonarr
// UpgradeSpecification / AlreadyImportedSpecification wording, matched case-insensitively).
var allowedRejections = []string{
	"not an upgrade",
	"not a custom format upgrade",
	"already imported",
	"existing file",
	"not a quality revision upgrade",
}

// Arr is the slice of an arr client the janitor needs (satisfied by *arr.Client).
type Arr interface {
	Queue(ctx context.Context) ([]arr.QueueRecord, error)
	ImportedFromDownload(ctx context.Context, hash string) (bool, error)
	RemoveQueueItemKeep(ctx context.Context, id int) error
}

// Janitor scans the configured arrs.
type Janitor struct {
	// Arrs maps a label (the arr's primary category) to its client.
	Arrs map[string]Arr
	// Clients are the arr-side download-client names that are Lazarr ("Lazarr",
	// "Lazarr Seerr"). Items of any other client are never touched.
	Clients []string
	// Known, when set, must report true for a hash before it is removed (the catalog
	// holds the release, so detach_on_delete can keep its imported links alive).
	Known func(hash string) bool

	// pendingSeen holds the importPending candidates of the previous pass (RunOnce is
	// not called concurrently: Start runs it from one goroutine).
	pendingSeen map[string]bool
}

// Candidate is a blocked download the rules allow removing (before the history check).
type Candidate struct {
	Hash       string
	QueueID    int
	Title      string
	Rejections int  // rejected files
	Pending    bool // at least one record is importPending (needs a second pass)
}

// Candidates applies the queue-side rules: a Lazarr client's download whose every
// record is importBlocked or importPending and whose every import rejection is an
// allowed one, with at least one rejection. Pure; tested with recorded queue JSON.
func Candidates(records []arr.QueueRecord, clients []string) []Candidate {
	type group struct {
		ids        []int
		title      string
		blocked    bool
		pending    bool
		ok         bool
		rejections map[string]bool
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range records {
		h := strings.ToLower(r.DownloadID)
		if h == "" || !clientMatches(r.DownloadClient, clients) {
			continue
		}
		g := groups[h]
		if g == nil {
			g = &group{blocked: true, ok: true, title: r.Title, rejections: map[string]bool{}}
			groups[h] = g
			order = append(order, h)
		}
		g.ids = append(g.ids, r.ID)
		switch {
		case strings.EqualFold(r.TrackedDownloadState, "importBlocked"):
		case strings.EqualFold(r.TrackedDownloadState, "importPending"):
			g.pending = true
		default:
			g.blocked = false
		}
		for _, sm := range r.StatusMessages {
			for _, m := range sm.Messages {
				if !allowed(m) {
					g.ok = false
				}
				g.rejections[sm.Title] = true
			}
		}
	}
	var out []Candidate
	for _, h := range order {
		g := groups[h]
		if !g.blocked || !g.ok || len(g.rejections) == 0 {
			continue
		}
		sort.Ints(g.ids)
		out = append(out, Candidate{Hash: h, QueueID: g.ids[0], Title: g.title, Rejections: len(g.rejections), Pending: g.pending})
	}
	return out
}

func clientMatches(name string, clients []string) bool {
	return slices.ContainsFunc(clients, func(c string) bool { return strings.EqualFold(strings.TrimSpace(c), name) })
}

func allowed(msg string) bool {
	m := strings.ToLower(msg)
	for _, a := range allowedRejections {
		if strings.Contains(m, a) {
			return true
		}
	}
	return false
}

// Result summarizes one pass.
type Result struct {
	Candidates int
	Removed    []string // "<arr>:<hash>"
}

// RunOnce scans every arr once.
func (j *Janitor) RunOnce(ctx context.Context) Result {
	var res Result
	seen := map[string]bool{}
	defer func() { j.pendingSeen = seen }()
	names := make([]string, 0, len(j.Arrs))
	for n := range j.Arrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		a := j.Arrs[name]
		recs, err := a.Queue(ctx)
		if err != nil {
			slog.Warn("janitor: queue", "arr", name, "err", err)
			continue
		}
		for _, c := range Candidates(recs, j.Clients) {
			if len(res.Removed) >= maxRemovalsPerRun || ctx.Err() != nil {
				return res
			}
			res.Candidates++
			if c.Pending {
				key := name + ":" + c.Hash
				seen[key] = true
				if !j.pendingSeen[key] {
					continue // first sighting in importPending: wait one more pass
				}
			}
			if j.Known != nil && !j.Known(c.Hash) {
				continue
			}
			imported, err := a.ImportedFromDownload(ctx, c.Hash)
			if err != nil {
				slog.Warn("janitor: history", "arr", name, "hash", c.Hash, "err", err)
				continue
			}
			if !imported {
				continue // nothing of it is in the library: not a partial pack
			}
			if err := a.RemoveQueueItemKeep(ctx, c.QueueID); err != nil {
				slog.Warn("janitor: remove", "arr", name, "hash", c.Hash, "queue_id", c.QueueID, "err", err)
				continue
			}
			metrics.IncPackJanitorRemoved()
			res.Removed = append(res.Removed, name+":"+c.Hash)
			slog.Info("janitor: removed partially imported pack from the arr queue (no blocklist, no re-search)",
				"arr", name, "hash", c.Hash, "title", c.Title, "rejected_files", c.Rejections)
		}
	}
	return res
}

// Start runs RunOnce every Interval until ctx is canceled.
func (j *Janitor) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				j.RunOnce(ctx)
			}
		}
	}()
}
