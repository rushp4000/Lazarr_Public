package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/your-username/lazarr/internal/missing"
)

// missingWait bounds how long GET /api/missing waits for a refresh it started or
// joined; past it the current snapshot comes back with refreshing=true.
const missingWait = 45 * time.Second

// handleMissing serves GET /api/missing[?refresh=1].
func (s *handler) handleMissing(w http.ResponseWriter, r *http.Request) {
	if s.missing == nil {
		jsonOK(w, missing.Snapshot{Arrs: []missing.ArrMissing{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), missingWait)
	defer cancel()
	refresh := r.URL.Query().Get("refresh") == "1"
	snap, err := s.missing.Missing(ctx, refresh)
	if errors.Is(err, ErrMissingDisabled) {
		jsonOK(w, missing.Snapshot{Arrs: []missing.ArrMissing{}})
		return
	}
	var rl *missing.RateLimitedError
	if errors.As(err, &rl) {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(rl.Wait.Seconds()))))
		jsonErr(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snap.Arrs == nil {
		snap.Arrs = []missing.ArrMissing{}
	}
	jsonOK(w, snap)
}

// handleMissingSearch serves POST /api/missing/search {"category","series_id","season"}
// or {"category","movie_id"}: asks the arr to search for that row.
func (s *handler) handleMissingSearch(w http.ResponseWriter, r *http.Request) {
	if s.missing == nil {
		jsonErr(w, ErrMissingDisabled.Error(), http.StatusBadRequest)
		return
	}
	var req missing.SearchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	res, err := s.missing.MissingSearch(r.Context(), req)
	var rl *missing.RateLimitedError
	switch {
	case err == nil:
		jsonOK(w, res)
	case errors.As(err, &rl):
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(rl.Wait.Seconds()))))
		jsonErr(w, err.Error(), http.StatusTooManyRequests)
	case errors.Is(err, missing.ErrInvalid), errors.Is(err, ErrMissingDisabled):
		jsonErr(w, err.Error(), http.StatusBadRequest)
	default:
		slog.Warn("webui: missing search", "category", req.Category, "err", err)
		jsonErr(w, err.Error(), http.StatusBadGateway)
	}
}
