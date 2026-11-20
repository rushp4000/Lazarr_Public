package webui

import (
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed assets/templates/index.html
var assetFS embed.FS

// handler is the concrete Web UI http.Handler. It holds the provider and the
// compiled template set.
type handler struct {
	prov    Provider
	missing MissingProvider // nil when prov does not implement it
	tmpl    *template.Template
}

// New returns an http.Handler serving the Lazarr dashboard. Wrap it in an
// http.Server and mount on config.WebUI.Listen. When username + password are
// non-empty, all routes are protected by HTTP Basic Auth.
//
// Requests to non-API paths fall through to the index.html shell page.
func New(prov Provider, username, password string) (http.Handler, error) {
	sub, err := fs.Sub(assetFS, "assets/templates")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.ParseFS(sub, "index.html")
	if err != nil {
		return nil, err
	}

	h := &handler{prov: prov, tmpl: tmpl}
	h.missing, _ = prov.(MissingProvider)

	mux := http.NewServeMux()

	// JSON API — read-only.
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/releases", h.handleReleases)
	mux.HandleFunc("GET /api/materialized", h.handleMaterialized)
	mux.HandleFunc("GET /api/metrics-summary", h.handleMetricsSummary)
	mux.HandleFunc("GET /api/config", h.handleConfig)
	mux.HandleFunc("GET /api/settings", h.handleSettingsGet)
	mux.HandleFunc("GET /api/logs", h.handleLogs)

	// JSON API — mutating (POST).
	mux.HandleFunc("POST /api/releases/{hash}/release", h.handleForceRelease)
	mux.HandleFunc("POST /api/audit/run", h.handleAuditRun)
	mux.HandleFunc("POST /api/repair/scan", h.handleRepairScan)
	mux.HandleFunc("POST /api/repair/{hash}/forget", h.handleForgetRelease)
	mux.HandleFunc("POST /api/settings", h.handleSettingsSave)
	mux.HandleFunc("POST /api/restart", h.handleRestart)

	// Repair — read-only.
	mux.HandleFunc("GET /api/repair", h.handleRepair)
	mux.HandleFunc("GET /api/orphans", h.handleOrphans)
	mux.HandleFunc("GET /api/dangling", h.handleDangling)

	// Slow lane (v1.2.9).
	mux.HandleFunc("GET /api/slowlane", h.handleSlowLane)
	mux.HandleFunc("POST /api/slowlane", h.handleSlowLaneAdd)
	mux.HandleFunc("POST /api/slowlane/{hash}/cancel", h.handleSlowLaneCancel)

	// Missing tab: what the linked arrs still want.
	mux.HandleFunc("GET /api/missing", h.handleMissing)
	mux.HandleFunc("POST /api/missing/search", h.handleMissingSearch)

	// Local cache (Workstream G).
	mux.HandleFunc("GET /api/cache", h.handleCache)
	mux.HandleFunc("GET /api/cache/files", h.handleCacheFiles)
	mux.HandleFunc("POST /api/cache/poll", h.handleCachePoll)
	mux.HandleFunc("POST /api/cache/{hash}/pull", h.handleCachePull)
	mux.HandleFunc("POST /api/cache/{hash}/evict", h.handleCacheEvict)
	mux.HandleFunc("POST /api/cache/{hash}/analyze", h.handleCacheAnalyze)
	mux.HandleFunc("POST /api/cache/analysis/backfill", h.handleCacheAnalyzeBackfill)

	// Serve the HTML shell for all other paths (SPA-style).
	mux.HandleFunc("/", h.handleIndex)

	return basicAuth(mux, username, password), nil
}

// handleIndex serves the single-page HTML shell. API paths that did not match a
// registered method+route must NOT fall through to HTML — a JS fetch would try to
// parse the shell page as JSON (and a GET of a POST-only mutation would look like
// it succeeded).
func (s *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		jsonErr(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		slog.Warn("webui: render index", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
