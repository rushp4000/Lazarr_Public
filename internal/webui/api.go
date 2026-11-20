package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/your-username/lazarr/internal/catalog"
	"github.com/your-username/lazarr/internal/localcache"
	"github.com/your-username/lazarr/internal/logging"
	"github.com/your-username/lazarr/internal/materialize"
	"github.com/your-username/lazarr/internal/repair"
)

// releasesResponse is the /api/releases JSON body.
type releasesResponse struct {
	Releases []*catalog.Release `json:"releases"`
	Total    int                `json:"total"`
	Limit    int                `json:"limit"`
	Offset   int                `json:"offset"`
}

// jsonOK writes v as JSON with status 200.
func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("webui: json encode", "err", err)
	}
}

// jsonErr writes a JSON {"error":"..."} body with the given status.
func jsonErr(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleStatus serves GET /api/status.
func (s *handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.prov.Status())
}

// handleReleases serves GET /api/releases?q=&state=&category=&limit=&offset=.
// state=local is a pseudo-state: releases with at least one cached/pulling file.
func (s *handler) handleReleases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := catalog.ReleaseFilter{
		Q:        q.Get("q"),
		State:    catalog.State(q.Get("state")),
		Category: q.Get("category"),
	}
	if f.State == "local" {
		f.State = ""
		f.LocalOnly = true
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			f.Offset = n
		}
	}

	rels, total, err := s.prov.ListReleases(f)
	if err != nil {
		jsonErr(w, "store error", http.StatusInternalServerError)
		slog.Warn("webui: list releases", "err", err)
		return
	}
	if rels == nil {
		rels = []*catalog.Release{}
	}
	jsonOK(w, releasesResponse{
		Releases: rels,
		Total:    total,
		Limit:    f.Limit,
		Offset:   f.Offset,
	})
}

// handleMaterialized serves GET /api/materialized.
func (s *handler) handleMaterialized(w http.ResponseWriter, r *http.Request) {
	items := s.prov.MaterializedSet()
	if items == nil {
		items = []MaterializedItem{}
	}
	jsonOK(w, items)
}

// handleMetricsSummary serves GET /api/metrics-summary.
func (s *handler) handleMetricsSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.prov.MetricsSummary()
	if err != nil {
		jsonErr(w, "metrics gather error", http.StatusInternalServerError)
		slog.Warn("webui: gather metrics", "err", err)
		return
	}
	jsonOK(w, sum)
}

// handleConfig serves GET /api/config.
func (s *handler) handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.prov.SafeConfig())
}

// handleForceRelease serves POST /api/releases/{hash}/release.
func (s *handler) handleForceRelease(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.PathValue("hash"), "")
	if hash == "" {
		jsonErr(w, "missing hash", http.StatusBadRequest)
		return
	}
	if err := s.prov.ForceRelease(hash); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: force release", "hash", hash, "err", err)
		return
	}
	jsonOK(w, map[string]string{"status": "released"})
}

// handleAuditRun serves POST /api/audit/run.
func (s *handler) handleAuditRun(w http.ResponseWriter, r *http.Request) {
	if err := s.prov.TriggerAudit(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: trigger audit", "err", err)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// handleRepair serves GET /api/repair — returns the cached evicted list from the catalog.
func (s *handler) handleRepair(w http.ResponseWriter, r *http.Request) {
	evicted, err := s.prov.ListEvicted()
	if err != nil {
		jsonErr(w, "store error", http.StatusInternalServerError)
		slog.Warn("webui: list evicted", "err", err)
		return
	}
	if evicted == nil {
		evicted = []*catalog.Release{}
	}
	jsonOK(w, map[string]any{"evicted": evicted, "count": len(evicted)})
}

// handleOrphans serves GET /api/orphans — the last account orphan audit.
func (s *handler) handleOrphans(w http.ResponseWriter, _ *http.Request) {
	rep := s.prov.Orphans()
	if rep.Items == nil {
		rep.Items = []materialize.AccountOrphan{}
	}
	jsonOK(w, rep)
}

// handleDangling serves GET /api/dangling — the last dangling library-link sweep.
func (s *handler) handleDangling(w http.ResponseWriter, _ *http.Request) {
	rep := s.prov.Dangling()
	if rep.Hashes == nil {
		rep.Hashes = []repair.DanglingHash{}
	}
	jsonOK(w, rep)
}

// handleRepairScan serves POST /api/repair/scan — triggers a live checkcached sweep.
func (s *handler) handleRepairScan(w http.ResponseWriter, r *http.Request) {
	entries, err := s.prov.TriggerRepairScan(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: repair scan", "err", err)
		return
	}
	if entries == nil {
		entries = []materialize.RepairEntry{}
	}
	jsonOK(w, map[string]any{"evicted": entries, "count": len(entries)})
}

// handleSettingsGet serves GET /api/settings — the editable config with secrets blanked.
func (s *handler) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	st := s.prov.GetSettings()
	// Defense in depth: never let secrets out on GET regardless of provider behavior.
	st.TorBoxAPIKey = ""
	st.WebUIPassword = ""
	jsonOK(w, st)
}

// handleSettingsSave serves POST /api/settings. Validation failures come back as a
// 400 with the human-readable reason so the form can show it inline.
func (s *handler) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var st Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&st); err != nil {
		jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	restart, err := s.prov.SaveSettings(st)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		slog.Warn("webui: save settings rejected", "err", err)
		return
	}
	slog.Info("webui: settings saved", "restart_required", restart)
	jsonOK(w, SaveResult{Saved: true, RestartRequired: restart})
}

// handleLogs serves GET /api/logs?level=&limit=.
func (s *handler) handleLogs(w http.ResponseWriter, r *http.Request) {
	level := r.URL.Query().Get("level")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries := s.prov.Logs(level, limit)
	if entries == nil {
		entries = []logging.Entry{}
	}
	jsonOK(w, map[string]any{"entries": entries, "count": len(entries)})
}

// handleRestart serves POST /api/restart — graceful shutdown so the container
// supervisor restarts Lazarr on the saved config.
func (s *handler) handleRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.prov.Restart(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("webui: restart requested")
	jsonOK(w, map[string]string{"status": "restarting"})
}

// handleForgetRelease serves POST /api/repair/{hash}/forget — removes from catalog + symlinks.
func (s *handler) handleForgetRelease(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		jsonErr(w, "missing hash", http.StatusBadRequest)
		return
	}
	if err := s.prov.ForgetRelease(hash); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		slog.Warn("webui: forget release", "hash", hash, "err", err)
		return
	}
	jsonOK(w, map[string]string{"status": "forgotten"})
}

// ---- Slow lane (v1.2.9) -----------------------------------------------------------

// slowLaneAddRequest is the JSON POST /api/slowlane body.
type slowLaneAddRequest struct {
	Magnet   string `json:"magnet"` // magnet link or bare infohash
	Category string `json:"category"`
	// Target (optional): {"series_id":N,"season":S,"movie_id":M,"title":"…"}, every
	// field optional, season 0/absent = any. Limits the automatic manual import to it.
	Target *catalog.SlowLaneTarget `json:"target,omitempty"`
}

// handleSlowLane serves GET /api/slowlane.
func (s *handler) handleSlowLane(w http.ResponseWriter, _ *http.Request) {
	v, err := s.prov.SlowLane()
	if err != nil {
		jsonErr(w, "store error", http.StatusInternalServerError)
		slog.Warn("webui: slow lane list", "err", err)
		return
	}
	if v.Items == nil {
		v.Items = []*catalog.SlowLaneItem{}
	}
	jsonOK(w, v)
}

// handleSlowLaneAdd serves POST /api/slowlane {"magnet":"…","category":"sonarr_main",
// "target":{"series_id":91,"season":2}} (target optional).
// A multipart/form-data body is an upload of .torrent files (handleSlowLaneUpload).
func (s *handler) handleSlowLaneAdd(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		s.handleSlowLaneUpload(w, r)
		return
	}
	var req slowLaneAddRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	it, err := s.prov.SlowLaneAdd(req.Magnet, req.Category, req.Target)
	if err != nil {
		status := http.StatusBadRequest
		if it != nil {
			status = http.StatusConflict // already queued: the body still names it
		}
		jsonErr(w, err.Error(), status)
		return
	}
	jsonOK(w, it)
}

const (
	// slowUploadFileMax caps one uploaded .torrent (real ones are well under 1 MiB).
	slowUploadFileMax = 2 << 20
	// slowUploadRequestMax bounds a whole multipart upload (several .torrent files).
	slowUploadRequestMax = 16 << 20
	// slowUploadMaxFiles bounds how many .torrent parts one upload may carry.
	slowUploadMaxFiles = 32
	// slowUploadFieldMax caps a text field (category, magnet, target).
	slowUploadFieldMax = 64 << 10
)

// slowLaneAddResult is one input of a multipart POST /api/slowlane and what became of it.
type slowLaneAddResult struct {
	Source string                `json:"source"`           // the uploaded file name, or "magnet"
	Item   *catalog.SlowLaneItem `json:"item,omitempty"`   // the queued item (or the active one, with Exists)
	Exists bool                  `json:"exists,omitempty"` // already in the slow lane
	Error  string                `json:"error,omitempty"`  // why it was not queued
}

// slowLaneUploadResponse is the multipart POST /api/slowlane body.
type slowLaneUploadResponse struct {
	Queued  int                 `json:"queued"`
	Results []slowLaneAddResult `json:"results"`
	Error   string              `json:"error,omitempty"` // set when nothing was queued
}

// handleSlowLaneUpload serves a multipart POST /api/slowlane: `category` (required), one
// or more `torrent` file parts and/or a `magnet` field, and an optional `target` field
// (the JSON "target" object of the JSON body; it applies to every input). Every input
// gets its own result, so one bad file never blocks the others. 200 when at least one
// was queued, else 409 when every input was already in the slow lane, else 400.
func (s *handler) handleSlowLaneUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, slowUploadRequestMax)
	mr, err := r.MultipartReader()
	if err != nil {
		jsonErr(w, "invalid multipart body: "+err.Error(), http.StatusBadRequest)
		return
	}
	type upload struct {
		name string
		data []byte
		err  string
	}
	var (
		category, magnet string
		target           *catalog.SlowLaneTarget
		files            []upload
	)
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slowUploadReadErr(w, err)
			return
		}
		switch p.FormName() {
		case "torrent", "torrents": // "torrents" is qBittorrent's name for the same part
			u := upload{name: p.FileName()}
			if u.name == "" {
				u.name = fmt.Sprintf("file %d", len(files)+1)
			}
			data, tooBig, rerr := readPartMax(p, slowUploadFileMax)
			switch {
			case rerr != nil:
				_ = p.Close()
				slowUploadReadErr(w, rerr)
				return
			case len(files) >= slowUploadMaxFiles:
				u.err = fmt.Sprintf("too many files in one upload (max %d)", slowUploadMaxFiles)
			case tooBig:
				u.err = fmt.Sprintf("file too large (max %d MiB per .torrent)", slowUploadFileMax>>20)
			default:
				u.data = data
			}
			files = append(files, u)
		case "category", "magnet", "target":
			v, tooBig, rerr := readPartMax(p, slowUploadFieldMax)
			if rerr != nil || tooBig {
				_ = p.Close()
				if rerr == nil {
					rerr = fmt.Errorf("field %q too large", p.FormName())
				}
				slowUploadReadErr(w, rerr)
				return
			}
			switch p.FormName() {
			case "category":
				category = string(v)
			case "magnet":
				magnet = strings.TrimSpace(string(v))
			default: // target
				if strings.TrimSpace(string(v)) != "" {
					target = &catalog.SlowLaneTarget{}
					if jerr := json.Unmarshal(v, target); jerr != nil {
						_ = p.Close()
						jsonErr(w, "invalid target: "+jerr.Error(), http.StatusBadRequest)
						return
					}
				}
			}
		default:
			if _, derr := io.Copy(io.Discard, p); derr != nil {
				_ = p.Close()
				slowUploadReadErr(w, derr)
				return
			}
		}
		_ = p.Close()
	}
	if len(files) == 0 && magnet == "" {
		jsonErr(w, "nothing to add: send one or more `torrent` files (or a `magnet` field)", http.StatusBadRequest)
		return
	}

	resp := slowLaneUploadResponse{Results: []slowLaneAddResult{}}
	record := func(src string, it *catalog.SlowLaneItem, err error) {
		res := slowLaneAddResult{Source: src, Item: it}
		switch {
		case err == nil:
			resp.Queued++
		case it != nil:
			res.Exists, res.Error = true, err.Error() // already active: Item names it
		default:
			res.Error = err.Error()
		}
		resp.Results = append(resp.Results, res)
	}
	if magnet != "" {
		it, err := s.prov.SlowLaneAdd(magnet, category, target)
		record("magnet", it, err)
	}
	for _, f := range files {
		if f.err != "" {
			record(f.name, nil, errors.New(f.err))
			continue
		}
		it, err := s.prov.SlowLaneAddTorrent(f.data, category, target)
		record(f.name, it, err)
	}
	slog.Info("webui: slow lane upload", "category", category, "inputs", len(resp.Results), "queued", resp.Queued)

	status := http.StatusOK
	if resp.Queued == 0 {
		status = http.StatusConflict
		msgs := make([]string, 0, len(resp.Results))
		for _, res := range resp.Results {
			if !res.Exists {
				status = http.StatusBadRequest
			}
			msgs = append(msgs, res.Source+": "+res.Error)
		}
		resp.Error = strings.Join(msgs, "; ")
		if len(resp.Results) == 1 {
			resp.Error = resp.Results[0].Error
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Debug("webui: json encode", "err", err)
	}
}

// readPartMax reads at most limit bytes of a multipart part. A longer part is drained
// (the request body is bounded by MaxBytesReader) and reported as tooBig.
func readPartMax(p io.Reader, limit int64) (data []byte, tooBig bool, err error) {
	data, err = io.ReadAll(io.LimitReader(p, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		if _, err := io.Copy(io.Discard, p); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	}
	return data, false, nil
}

// slowUploadReadErr answers a multipart upload that could not be read: 413 when the
// whole request is over slowUploadRequestMax, else 400.
func slowUploadReadErr(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		jsonErr(w, fmt.Sprintf("upload too large (max %d MiB per request)", slowUploadRequestMax>>20),
			http.StatusRequestEntityTooLarge)
		return
	}
	jsonErr(w, "invalid multipart body: "+err.Error(), http.StatusBadRequest)
}

// handleSlowLaneCancel serves POST /api/slowlane/{hash}/cancel.
func (s *handler) handleSlowLaneCancel(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		jsonErr(w, "missing hash", http.StatusBadRequest)
		return
	}
	if err := s.prov.SlowLaneCancel(hash); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"cancelled": true})
}

// ---- Workstream G: local cache ----------------------------------------------------

// handleCache serves GET /api/cache — the live snapshot ({"enabled":false} when off).
func (s *handler) handleCache(w http.ResponseWriter, r *http.Request) {
	snap := s.prov.CacheSnapshot()
	if snap == nil {
		jsonOK(w, map[string]any{"enabled": false})
		return
	}
	jsonOK(w, snap)
}

// handleCacheFiles serves GET /api/cache/files.
func (s *handler) handleCacheFiles(w http.ResponseWriter, r *http.Request) {
	files := s.prov.CacheFiles()
	if files == nil {
		files = []localcache.LocalFileView{}
	}
	jsonOK(w, map[string]any{"files": files, "count": len(files)})
}

// handleCachePull serves POST /api/cache/{hash}/pull.
func (s *handler) handleCachePull(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	n, err := s.prov.CachePull(hash)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"queued": n})
}

// handleCacheAnalyze serves POST /api/cache/{hash}/analyze.
func (s *handler) handleCacheAnalyze(w http.ResponseWriter, r *http.Request) {
	n, err := s.prov.CacheAnalyze(r.PathValue("hash"))
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"queued": n})
}

// handleCacheAnalyzeBackfill serves POST /api/cache/analysis/backfill.
func (s *handler) handleCacheAnalyzeBackfill(w http.ResponseWriter, _ *http.Request) {
	n, err := s.prov.CacheAnalyzeBackfill()
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"queued": n})
}

// handleCacheEvict serves POST /api/cache/{hash}/evict.
func (s *handler) handleCacheEvict(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if err := s.prov.CacheEvict(hash); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"evicted": true})
}

// handleCachePoll serves POST /api/cache/poll — poll the media servers now.
func (s *handler) handleCachePoll(w http.ResponseWriter, r *http.Request) {
	if err := s.prov.CachePoll(r.Context()); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]any{"polled": true})
}
