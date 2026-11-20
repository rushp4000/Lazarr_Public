// Package torbox provides a concrete HTTP client satisfying the [Client] interface.
// Auth: "Authorization: Bearer <key>" on every request.
// The API key is never written to any log or error string.
package torbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/config"
	"github.com/your-username/lazarr/internal/constants"
	"github.com/your-username/lazarr/internal/metrics"
	"github.com/your-username/lazarr/internal/version"
)

// defaultTimeout is used on all outbound HTTP requests.
const defaultTimeout = 30 * time.Second

// maxRetries for transient 5xx / network errors.
const maxRetries = 3

// retryBackoff is the base backoff duration (doubles each attempt).
const retryBackoff = 500 * time.Millisecond

// ----------------------------------------------------------------------------
// Internal option type for tests.
// ----------------------------------------------------------------------------

type option func(*client)

// withHTTPClient overrides the *http.Client (used in tests).
//
//nolint:unused // test-injection seam, wired through NewForTest in export_test.go
func withHTTPClient(hc *http.Client) option {
	return func(c *client) { c.hc = hc }
}

// withClock replaces the governor's clock and sleep (used in tests).
//
//nolint:unused // test-injection seam, wired through NewForTest in export_test.go
func withClock(now func() time.Time, sleep func(context.Context, time.Duration) error) option {
	return func(c *client) {
		c.gov.now = now
		c.gov.sleep = sleep
	}
}

// withBaseURL overrides the API base URL (used in tests).
//
//nolint:unused // test-injection seam, wired through NewForTest in export_test.go
func withBaseURL(base string) option {
	return func(c *client) { c.base = base }
}

// ----------------------------------------------------------------------------
// Constructor.
// ----------------------------------------------------------------------------

// New constructs a production [Client] from the supplied TorBox config.
// For tests, call newClient instead, which accepts functional options.
func New(cfg config.TorBox) Client {
	return newClient(cfg)
}

// newClient is the internal constructor; it accepts functional options for testability.
func newClient(cfg config.TorBox, opts ...option) *client {
	base := cfg.APIBase
	if base == "" {
		base = constants.TorBoxAPIBase
	}
	c := &client{
		apiKey: cfg.APIKey,
		base:   base,
		hc: &http.Client{
			Timeout: defaultTimeout,
		},
		gov: newGovernor(cfg.MaxRequestsPerMinute),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// ----------------------------------------------------------------------------
// client struct.
// ----------------------------------------------------------------------------

type client struct {
	apiKey string
	base   string
	hc     *http.Client
	gov    *governor // per-key request governor (token bucket + TorBox-armed backoff)
}

// BackoffUntil reports the TorBox-armed API backoff currently in force, if any.
func (c *client) BackoffUntil() (time.Time, bool) { return c.gov.active() }

// ----------------------------------------------------------------------------
// Generic envelope + request helpers.
// ----------------------------------------------------------------------------

// envelope is the outer TorBox response wrapper.
// success may be bool or null in JSON (use json.RawMessage).
// detail may be a string or an array of strings depending on the endpoint.
type envelope struct {
	Success json.RawMessage `json:"success"`
	Detail  json.RawMessage `json:"detail"`
	Data    json.RawMessage `json:"data"`
}

// isSuccess returns true when the Success field is the JSON literal `true`.
func (e envelope) isSuccess() bool {
	return string(e.Success) == "true"
}

// detail extracts the Detail field as a string. TorBox may return detail as
// either a JSON string or an array of strings (observed on controltorrent
// delete responses). Both forms are handled; arrays are joined with "; ".
func (e envelope) detail() string {
	if e.Detail == nil || string(e.Detail) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(e.Detail, &s) == nil {
		return s
	}
	var arr []string
	if json.Unmarshal(e.Detail, &arr) == nil {
		return strings.Join(arr, "; ")
	}
	return string(e.Detail)
}

// apiError is returned on non-2xx HTTP or success!=true.
// It intentionally omits the API key from any string representation.
type apiError struct {
	StatusCode int    // 0 if envelope-level error on a 2xx body
	Detail     string // TorBox detail string, never contains the key
}

func (e *apiError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("torbox: HTTP %d: %s", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("torbox: api error: %s", e.Detail)
}

// redactURLError strips the query string from a *url.Error's URL so the API key
// (passed to requestdl as a `token` query param) never reaches logs. The inner
// error is preserved for errors.Is/As (e.g. context.DeadlineExceeded). Non
// *url.Error values are returned unchanged.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := "[redacted]"
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery = ""
		redacted = u.String()
	}
	return fmt.Errorf("%s %q: %w", ue.Op, redacted, ue.Err)
}

// doGET executes a GET with retries. The env is populated on success.
// On HTTP 4xx in constants.LinkRefreshStatuses, it returns a sentinel *apiError
// with StatusCode set — callers that need ErrLinkExpired check for that.
func (c *client) doGET(ctx context.Context, path string, params url.Values) (envelope, int, error) {
	return c.doRequest(ctx, http.MethodGet, path, params, nil, "")
}

// doPOSTJSON executes a POST with a JSON body.
func (c *client) doPOSTJSON(ctx context.Context, path string, body any) (envelope, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return envelope{}, 0, fmt.Errorf("torbox: marshal: %w", err)
	}
	return c.doRequest(ctx, http.MethodPost, path, nil, bytes.NewReader(b), "application/json")
}

// doPOSTMultipart executes a multipart/form-data POST.
func (c *client) doPOSTMultipart(ctx context.Context, path string, fields map[string]string) (envelope, int, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return envelope{}, 0, fmt.Errorf("torbox: multipart: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return envelope{}, 0, fmt.Errorf("torbox: multipart close: %w", err)
	}
	return c.doRequest(ctx, http.MethodPost, path, nil, &buf, mw.FormDataContentType())
}

// doRequest is the core transport with retry logic.
// Returns (envelope, httpStatusCode, error).
// httpStatusCode is 0 on transport errors.
func (c *client) doRequest(
	ctx context.Context,
	method, path string,
	params url.Values,
	body io.Reader,
	contentType string,
) (envelope, int, error) {
	rawURL := c.base + path
	if len(params) > 0 {
		rawURL += "?" + params.Encode()
	}

	// Buffer the body once so every attempt sends it in full. Reusing the caller's
	// reader sent an EMPTY body on any retry after a 5xx/network error (the first
	// attempt had already drained it) — TorBox then answered 422 "body: Field
	// required", seen live on ~2% of controltorrent deletes.
	var payload []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return envelope{}, 0, fmt.Errorf("torbox: read body: %w", err)
		}
		payload = b
	}

	var (
		lastErr    error
		lastStatus int
	)
	ep := endpointLabel(path)
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			wait := retryBackoff * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return envelope{}, 0, ctx.Err()
			case <-time.After(wait):
			}
		}

		// Governor: every HTTP attempt costs one token; during a TorBox-armed backoff
		// nothing is sent at all.
		if err := c.gov.acquire(ctx); err != nil {
			if errors.Is(err, ErrAPIBackoff) {
				metrics.IncTorBoxRequest(ep, "backoff")
			}
			return envelope{}, 0, err
		}

		var bodyR io.Reader
		if body != nil {
			bodyR = bytes.NewReader(payload) // fresh full copy per attempt
		}

		req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyR)
		if err != nil {
			return envelope{}, 0, fmt.Errorf("torbox: new request: %w", redactURLError(err))
		}
		// Authorization header: never log or expose c.apiKey.
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("User-Agent", version.UserAgent())
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			// The requestdl URL carries the API key as a `token` query param; a
			// *url.Error stringifies the full URL, so redact it before wrapping.
			metrics.IncTorBoxRequest(ep, "transport_error")
			lastErr = fmt.Errorf("torbox: request: %w", redactURLError(err))
			continue // retry on network error
		}
		defer func() { _ = resp.Body.Close() }()

		statusCode := resp.StatusCode
		metrics.IncTorBoxRequest(ep, strconv.Itoa(statusCode))
		c.gov.observe(resp.Header)

		// The per-key API limiter. Back off EVERYTHING until the window resets — unless
		// the body names the uncached-createtorrent hourly cap, which is endpoint-local
		// (CreateTorrent maps it to ErrRateLimited and the engine backs off creates only).
		if statusCode == http.StatusTooManyRequests {
			var env envelope
			_ = json.NewDecoder(resp.Body).Decode(&env)
			if detailHourlyLimit(env.detail()) {
				return envelope{}, statusCode, &apiError{StatusCode: statusCode, Detail: env.detail()}
			}
			until := c.gov.trip(resp.Header, "HTTP 429 from "+ep)
			return envelope{}, statusCode, &BackoffError{Until: until, Reason: "HTTP 429 from " + ep}
		}

		// 4xx link-refresh statuses — return immediately, no retry. Decode the body so the
		// REAL API detail is preserved (S6): a gone torrent can be reported as a 400/403/410
		// whose detail says "not found", and RequestDL must tell that dead-cache case apart
		// from a genuine stale presigned link. Fall back to a synthetic detail when the body
		// is empty/unparseable (the common stale-link case carries no useful detail).
		if slices.Contains(constants.LinkRefreshStatuses, statusCode) {
			detail := "presigned link returned HTTP " + strconv.Itoa(statusCode)
			var env envelope
			if json.NewDecoder(resp.Body).Decode(&env) == nil && env.detail() != "" {
				detail = env.detail()
			}
			return envelope{}, statusCode, &apiError{StatusCode: statusCode, Detail: detail}
		}

		// 5xx — retry.
		if statusCode >= 500 {
			lastErr = &apiError{StatusCode: statusCode, Detail: "server error"}
			lastStatus = statusCode
			continue
		}

		// Parse the envelope.
		var env envelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			return envelope{}, statusCode, fmt.Errorf("torbox: decode: %w", err)
		}

		// Non-2xx (but not in retry/link-refresh ranges).
		if statusCode >= 400 {
			if detailRateLimited(env.detail()) {
				until := c.gov.trip(resp.Header, "rate-limit detail from "+ep)
				return envelope{}, statusCode, &BackoffError{Until: until, Reason: "rate-limit detail from " + ep}
			}
			return envelope{}, statusCode, &apiError{StatusCode: statusCode, Detail: env.detail()}
		}

		// 2xx but success != true (e.g. rate limit: success:null).
		if !env.isSuccess() {
			if detailRateLimited(env.detail()) {
				until := c.gov.trip(resp.Header, "rate-limit detail from "+ep)
				return envelope{}, statusCode, &BackoffError{Until: until, Reason: "rate-limit detail from " + ep}
			}
			return env, statusCode, nil // caller inspects envelope.Detail for specific errors
		}

		return env, statusCode, nil
	}

	if lastErr != nil {
		return envelope{}, lastStatus, lastErr
	}
	return envelope{}, 0, fmt.Errorf("torbox: exhausted retries")
}

// ----------------------------------------------------------------------------
// CheckCached
// ----------------------------------------------------------------------------

// CheckCached implements [Client]. It batches hashes ≤100 per request.
func (c *client) CheckCached(hashes []string) (map[string]CachedItem, error) {
	result := make(map[string]CachedItem, len(hashes))

	for i := 0; i < len(hashes); i += constants.CheckCachedBatchMax {
		end := i + constants.CheckCachedBatchMax
		if end > len(hashes) {
			end = len(hashes)
		}
		batch := hashes[i:end]

		params := url.Values{}
		params.Set("hash", strings.Join(batch, ","))
		params.Set("format", "object")
		params.Set("list_files", "true")

		env, _, err := c.doGET(context.Background(), constants.EpCheckCached, params)
		if err != nil {
			return nil, fmt.Errorf("torbox: checkcached: %w", err)
		}

		// data may be null or {} on all misses.
		if env.Data == nil || string(env.Data) == "null" {
			continue
		}

		// data is an object keyed by lowercase infohash.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(env.Data, &raw); err != nil {
			return nil, fmt.Errorf("torbox: checkcached decode data: %w", err)
		}

		for hash, itemRaw := range raw {
			var item cachedItemRaw
			if err := json.Unmarshal(itemRaw, &item); err != nil {
				return nil, fmt.Errorf("torbox: checkcached item decode: %w", err)
			}
			result[strings.ToLower(hash)] = toCachedItem(hash, item)
		}
	}
	return result, nil
}

// ----------------------------------------------------------------------------
// TorrentInfo
// ----------------------------------------------------------------------------

// TorrentInfo implements [Client].
func (c *client) TorrentInfo(hash string) (*CachedItem, error) {
	params := url.Values{}
	params.Set("hash", hash)

	env, _, err := c.doGET(context.Background(), constants.EpTorrentInfo, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: torrentinfo: %w", err)
	}

	if env.Data == nil || string(env.Data) == "null" {
		return nil, nil
	}

	var item cachedItemRaw
	if err := json.Unmarshal(env.Data, &item); err != nil {
		return nil, fmt.Errorf("torbox: torrentinfo decode: %w", err)
	}

	h := item.Hash
	if h == "" {
		h = hash
	}
	ci := toCachedItem(h, item)
	return &ci, nil
}

// ----------------------------------------------------------------------------
// CreateTorrent
// ----------------------------------------------------------------------------

// CreateTorrent implements [Client].
func (c *client) CreateTorrent(magnet string, addOnlyIfCached bool) (int64, string, error) {
	fields := map[string]string{
		"magnet": magnet,
	}
	if addOnlyIfCached {
		fields["add_only_if_cached"] = "true"
	}

	env, _, err := c.doPOSTMultipart(context.Background(), constants.EpCreateTorrent, fields)
	if err != nil {
		// Queued upstream arrives as HTTP 400 "Download already queued." — which the
		// transport surfaces as an apiError because 400 is in the link-refresh status
		// set. Map it before the generic wrap so the engine defers instead of
		// hot-retrying on every read.
		var ae *apiError
		if errors.As(err, &ae) && detailAlreadyQueued(ae.Detail) {
			return 0, "", ErrAlreadyQueued
		}
		// The uncached-add hourly cap can arrive as an HTTP 429 with the same detail.
		if errors.As(err, &ae) && detailHourlyLimit(ae.Detail) {
			return 0, "", ErrRateLimited
		}
		return 0, "", fmt.Errorf("torbox: createtorrent: %w", err)
	}

	// Rate-limited: success == null and detail contains "60 per 1 hour".
	if !env.isSuccess() {
		if strings.Contains(env.detail(), "60 per 1 hour") {
			return 0, "", ErrRateLimited
		}
		// Queued upstream reported in a 2xx envelope (success != true) — same meaning
		// as the HTTP 400 form mapped above.
		if detailAlreadyQueued(env.detail()) {
			return 0, "", ErrAlreadyQueued
		}
		// Dead-cache: a cached-only add whose hash is no longer cached / not found.
		// Surface the typed sentinel so the engine can mark the release errored.
		if addOnlyIfCached && detailNotFound(env.detail()) {
			return 0, "", ErrNotFound
		}
		return 0, "", &apiError{Detail: env.detail()}
	}

	var data struct {
		TorrentID int64  `json:"torrent_id"`
		Hash      string `json:"hash"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return 0, "", fmt.Errorf("torbox: createtorrent decode data: %w", err)
	}

	return data.TorrentID, data.Hash, nil
}

// ----------------------------------------------------------------------------
// RequestDL
// ----------------------------------------------------------------------------

// RequestDL implements [Client].
func (c *client) RequestDL(torrentID int64, fileID int) (string, error) {
	params := url.Values{}
	params.Set("token", c.apiKey)
	params.Set("torrent_id", strconv.FormatInt(torrentID, 10))
	params.Set("file_id", strconv.Itoa(fileID))
	params.Set("redirect", "false")

	env, statusCode, err := c.doGET(context.Background(), constants.EpRequestDL, params)
	if err != nil {
		// Dead-cache FIRST (S6): a gone torrent can be reported as HTTP 404 OR as a
		// 400/403/410 whose detail says "not found". It must map to ErrNotFound so the
		// engine errors the release and the arr re-grabs — NOT to ErrLinkExpired, which
		// would drive an endless refresh→fail→EIO retry loop. This check therefore precedes
		// the link-refresh-status branch below.
		var ae *apiError
		if statusCode == http.StatusNotFound ||
			(errors.As(err, &ae) && detailNotFound(ae.Detail)) {
			return "", ErrNotFound
		}
		// Link-expired sentinel from a 4xx whose detail is NOT a not-found (recoverable:
		// re-request the presigned link and retry).
		if slices.Contains(constants.LinkRefreshStatuses, statusCode) {
			return "", ErrLinkExpired
		}
		return "", fmt.Errorf("torbox: requestdl: %w", err)
	}

	// data is the URL string.
	var dlURL string
	if err := json.Unmarshal(env.Data, &dlURL); err != nil {
		return "", fmt.Errorf("torbox: requestdl decode url: %w", err)
	}
	return dlURL, nil
}

// detailNotFound reports whether a TorBox detail string indicates the torrent/cache
// is gone (not found / not cached / does not exist). Matched conservatively so the
// dead-cache path triggers only on a genuine "gone" signal.
// detailAlreadyQueued reports whether a TorBox detail string indicates the add was
// parked in the server-side download queue (account cooldown / slots full) — observed
// live as HTTP 400 "Download already queued." during an account cooldown window.
func detailAlreadyQueued(detail string) bool {
	return strings.Contains(strings.ToLower(detail), "already queued")
}

func detailNotFound(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "not found") ||
		strings.Contains(d, "not cached") ||
		strings.Contains(d, "does not exist") ||
		strings.Contains(d, "no longer")
}

// ----------------------------------------------------------------------------
// ControlDelete
// ----------------------------------------------------------------------------

// ControlDelete implements [Client]. Uses POST controltorrent {operation:"delete"}.
func (c *client) ControlDelete(torrentID int64) error {
	body := struct {
		TorrentID int64  `json:"torrent_id"`
		Operation string `json:"operation"`
	}{
		TorrentID: torrentID,
		Operation: "delete",
	}

	env, _, err := c.doPOSTJSON(context.Background(), constants.EpControl, body)
	if err != nil {
		return fmt.Errorf("torbox: controltorrent delete: %w", err)
	}
	if !env.isSuccess() {
		return &apiError{Detail: env.detail()}
	}
	return nil
}

// ----------------------------------------------------------------------------
// MyList / MyListByID
// ----------------------------------------------------------------------------

// MyList implements [Client].
func (c *client) MyList(offset int) ([]TorrentDetail, error) {
	params := url.Values{}
	params.Set("offset", strconv.Itoa(offset))
	params.Set("limit", strconv.Itoa(constants.MyListPageMax))
	params.Set("bypass_cache", "true")

	env, _, err := c.doGET(context.Background(), constants.EpMyList, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: mylist: %w", err)
	}

	if env.Data == nil || string(env.Data) == "null" {
		return nil, nil
	}

	var raw []torrentDetailRaw
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("torbox: mylist decode: %w", err)
	}

	out := make([]TorrentDetail, len(raw))
	for i, r := range raw {
		out[i] = toTorrentDetail(r)
	}
	return out, nil
}

// MyListByID implements [Client].
func (c *client) MyListByID(id int64) (*TorrentDetail, error) {
	params := url.Values{}
	params.Set("id", strconv.FormatInt(id, 10))

	env, _, err := c.doGET(context.Background(), constants.EpMyList, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: mylist byid: %w", err)
	}

	if env.Data == nil || string(env.Data) == "null" {
		return nil, nil
	}

	// When id is specified, data is a single object (not array).
	var raw torrentDetailRaw
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("torbox: mylist byid decode: %w", err)
	}
	td := toTorrentDetail(raw)
	return &td, nil
}

// ----------------------------------------------------------------------------
// UserMe
// ----------------------------------------------------------------------------

// UserMe implements [Client].
func (c *client) UserMe() (*Account, error) {
	params := url.Values{}
	params.Set("settings", "true")

	env, _, err := c.doGET(context.Background(), constants.EpUserMe, params)
	if err != nil {
		return nil, fmt.Errorf("torbox: userme: %w", err)
	}

	var raw struct {
		Plan                      int    `json:"plan"`
		AdditionalConcurrentSlots int    `json:"additional_concurrent_slots"`
		CooldownUntil             string `json:"cooldown_until"`
		LongTermStorage           bool   `json:"long_term_storage"`
	}
	if err := json.Unmarshal(env.Data, &raw); err != nil {
		return nil, fmt.Errorf("torbox: userme decode: %w", err)
	}

	baseSlots := constants.EssentialActiveSlots // default (plan 1 = Essential)
	// Additional plans may have different base slots, but we only know Essential=3.
	// Per spec: derive base from plan; Essential=3. Others default same.
	acc := &Account{
		Plan:          raw.Plan,
		ActiveSlots:   baseSlots + raw.AdditionalConcurrentSlots,
		CooldownUntil: raw.CooldownUntil,
		LongTermStore: raw.LongTermStorage,
	}
	return acc, nil
}

// ----------------------------------------------------------------------------
// Raw JSON shapes for decoding.
// ----------------------------------------------------------------------------

type cachedFileRaw struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type cachedItemRaw struct {
	Name  string          `json:"name"`
	Hash  string          `json:"hash"`
	Size  int64           `json:"size"`
	Files []cachedFileRaw `json:"files"`
}

type torrentDetailRaw struct {
	ID               int64           `json:"id"`
	Hash             string          `json:"hash"`
	Name             string          `json:"name"`
	Files            []cachedFileRaw `json:"files"`
	DownloadFinished bool            `json:"download_finished"`
	DownloadState    string          `json:"download_state"`
	Progress         float64         `json:"progress"`
	ETA              int64           `json:"eta"`
	DownloadSpeed    int64           `json:"download_speed"`
	Seeds            int             `json:"seeds"`
	Peers            int             `json:"peers"`
	CreatedAt        string          `json:"created_at"`
}

// ----------------------------------------------------------------------------
// Converters.
// ----------------------------------------------------------------------------

func toCachedItem(hash string, r cachedItemRaw) CachedItem {
	files := make([]CachedFile, len(r.Files))
	for i, f := range r.Files {
		files[i] = CachedFile(f)
	}
	return CachedItem{
		Hash:  strings.ToLower(hash),
		Name:  r.Name,
		Size:  r.Size,
		Files: files,
	}
}

func toTorrentDetail(r torrentDetailRaw) TorrentDetail {
	files := make([]CachedFile, len(r.Files))
	for i, f := range r.Files {
		files[i] = CachedFile(f)
	}
	return TorrentDetail{
		ID:               r.ID,
		Hash:             r.Hash,
		Name:             r.Name,
		Files:            files,
		DownloadFinished: r.DownloadFinished,
		DownloadState:    r.DownloadState,
		Progress:         r.Progress,
		ETA:              r.ETA,
		DownloadSpeed:    r.DownloadSpeed,
		Seeds:            r.Seeds,
		Peers:            r.Peers,
		CreatedAt:        parseTorBoxTime(r.CreatedAt),
	}
}

// parseTorBoxTime parses mylist timestamps ("2026-06-17T21:47:56Z", sometimes with
// fractional seconds or an offset). Zero on failure.
func parseTorBoxTime(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
