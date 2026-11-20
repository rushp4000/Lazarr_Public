package localcache

import (
	"sort"
	"time"

	"github.com/your-username/lazarr/internal/catalog"
)

// Snapshot is the Web UI / health view of the cache.
type Snapshot struct {
	Enabled   bool   `json:"enabled"`
	Dir       string `json:"dir"`
	PublishAt string `json:"publish_at"`
	Linking   bool   `json:"linking"`  // false when the publish mount failed (links never retargeted)
	Analysis  bool   `json:"analysis"` // post-pull media-server analysis is enabled (docs/28 §16)
	Profile   string `json:"profile"`

	DiskFree  uint64  `json:"disk_free"`
	DiskTotal uint64  `json:"disk_total"`
	FreePct   float64 `json:"free_pct"`
	Scale     float64 `json:"scale"`
	HighPct   int     `json:"free_high_pct"`
	LowPct    int     `json:"free_low_pct"`

	LocalBytes int64          `json:"local_bytes"`
	Files      map[string]int `json:"files"`

	Queue    []QueueItem   `json:"queue"`
	Pulling  []PullView    `json:"pulling"`
	Sessions []SessionView `json:"sessions"`

	LastPollUnix int64    `json:"last_poll_unix"`
	LastWalkUnix int64    `json:"last_walk_unix"`
	LinksIndexed int      `json:"links_indexed"`
	Sources      []string `json:"sources"`
}

type QueueItem struct {
	Hash   string `json:"hash"`
	FileID int    `json:"file_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Size   int64  `json:"size"`
}

type PullView struct {
	Hash        string `json:"hash"`
	FileID      int    `json:"file_id"`
	Name        string `json:"name"`
	Reason      string `json:"reason"`
	Size        int64  `json:"size"`
	Frontier    int64  `json:"frontier"`
	StartedUnix int64  `json:"started_unix"`
}

type SessionView struct {
	Server      string  `json:"server"`
	User        string  `json:"user"`
	Title       string  `json:"title"`
	Series      string  `json:"series"`
	Season      int     `json:"season"`
	Episode     int     `json:"episode"`
	ProgressPct float64 `json:"progress_pct"`
	State       string  `json:"state"`
	Lazarr      bool    `json:"lazarr"` // resolved to a Lazarr release
	Local       string  `json:"local"`  // "", "pulling", "complete"
}

// LocalFileView is one cached file joined with its release for the Cache tab.
type LocalFileView struct {
	Hash     string `json:"hash"`
	FileID   int    `json:"file_id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	RelPath  string `json:"rel_path"`
	State    string `json:"state"`
	Size     int64  `json:"size"`
	Frontier int64  `json:"frontier"`
	Class    string `json:"class"`
	Reason   string `json:"reason"`
	PulledAt int64  `json:"pulled_at"`
	LastRead int64  `json:"last_read"`
	Linked   bool   `json:"linked"`
	Error    string `json:"error,omitempty"`
	// Analysis is the post-pull media-server analysis per server (docs/28 §16).
	Analysis []catalog.LocalAnalysis `json:"analysis,omitempty"`
}

// Snapshot returns a point-in-time view. Cheap: one statfs plus in-memory state.
func (e *Engine) Snapshot() Snapshot {
	disk, _ := e.statDisk()
	e.mu.Lock()
	defer e.mu.Unlock()
	snap := Snapshot{
		Enabled:      true,
		Dir:          e.disk.dir,
		PublishAt:    e.publishAt,
		Linking:      e.links != nil && !e.noLink.Load(),
		Analysis:     e.analysisEnabled(),
		Profile:      e.cfg.Profile,
		DiskFree:     disk.Free,
		DiskTotal:    disk.Total,
		FreePct:      disk.FreePct(),
		Scale:        Scale(disk, e.cfg),
		HighPct:      e.cfg.FreeHighPct,
		LowPct:       e.cfg.FreeLowPct,
		Files:        map[string]int{},
		LastPollUnix: unixOrZero(e.lastPoll),
		LastWalkUnix: unixOrZero(e.lastWalk),
		LinksIndexed: e.linksN,
	}
	for _, s := range e.sources {
		snap.Sources = append(snap.Sources, s.Name())
	}
	names := map[string]string{}
	for k, fs := range e.files {
		snap.Files[string(fs.State)]++
		if fs.State == catalog.LocalComplete {
			snap.LocalBytes += fs.Size
		} else {
			snap.LocalBytes += fs.Frontier
		}
		_ = k
	}
	for _, r := range e.queue {
		snap.Queue = append(snap.Queue, QueueItem{Hash: r.key.hash, FileID: r.key.fileID, Name: r.name, Reason: r.reason, Size: r.size})
		names[r.key.hash] = r.name
	}
	for k, p := range e.pulling {
		var frontier int64
		if fs := e.files[k]; fs != nil {
			frontier = fs.Frontier
		}
		snap.Pulling = append(snap.Pulling, PullView{Hash: k.hash, FileID: k.fileID, Name: p.req.name, Reason: p.req.reason, Size: p.req.size, Frontier: frontier, StartedUnix: p.started.Unix()})
	}
	sort.Slice(snap.Pulling, func(i, j int) bool { return snap.Pulling[i].StartedUnix < snap.Pulling[j].StartedUnix })
	for _, s := range e.sessions {
		v := SessionView{Server: s.Server, User: s.UserName, Title: s.Title, Series: s.SeriesTitle, Season: s.Season, Episode: s.Episode, ProgressPct: s.ProgressPct(), State: string(s.State)}
		if ref, err := e.resolver.Resolve(s.Path); err == nil {
			v.Lazarr = true
			for k, fs := range e.files {
				if k.hash == ref.Hash && fs.rel == ref.RelPath {
					v.Local = string(fs.State)
				}
			}
		}
		snap.Sessions = append(snap.Sessions, v)
	}
	return snap
}

// LocalFiles lists every cached file with its release identity, newest pull first.
func (e *Engine) LocalFiles() []LocalFileView {
	e.mu.Lock()
	rows := make([]LocalFileView, 0, len(e.files))
	for k, fs := range e.files {
		rows = append(rows, LocalFileView{
			Hash: k.hash, FileID: k.fileID, RelPath: fs.rel, State: string(fs.State), Size: fs.Size,
			Frontier: fs.Frontier, Class: fs.Class, Reason: fs.Reason, PulledAt: fs.PulledAt,
			LastRead: fs.LastRead, Linked: fs.Linked, Error: fs.Error,
		})
	}
	e.mu.Unlock()
	names := map[string]*catalog.Release{}
	for i := range rows {
		rel, ok := names[rows[i].Hash]
		if !ok {
			rel, _, _ = e.store.GetRelease(rows[i].Hash)
			names[rows[i].Hash] = rel
		}
		if rel != nil {
			rows[i].Name = rel.Name
			rows[i].Category = rel.Category
		}
		if e.astore != nil {
			if as, err := e.astore.ListLocalAnalysis(rows[i].Hash, rows[i].FileID); err == nil {
				for _, a := range as {
					rows[i].Analysis = append(rows[i].Analysis, *a)
				}
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].PulledAt != rows[j].PulledAt {
			return rows[i].PulledAt > rows[j].PulledAt
		}
		if rows[i].Hash != rows[j].Hash {
			return rows[i].Hash < rows[j].Hash
		}
		return rows[i].FileID < rows[j].FileID
	})
	return rows
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
