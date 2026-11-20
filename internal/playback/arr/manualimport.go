package arr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/playback"
)

// Automatic manual import (slow_lane.auto_import, v1.2.10).
//
// A slow-lane item the arr never grabbed is handed off as a new completed download, so
// the arr's Completed Download Handling only has the release NAME to go on. That fails
// for multi-season packs (Sonarr's MultiSeasonSpecification rejects "S01-S02", or it
// imports only the season it parsed: Lindy Mercer S01-S02 imported S01 only),
// foreign-language folder names around English file names, 5x01-style folders, ...
// The arr's own manual import parses each FILE on its own and gets those right:
//
//	GET  /api/v3/manualimport?folder=<download folder>&filterExistingFiles=true
//	POST /api/v3/command {"name":"ManualImport","importMode":"move","files":[...]}
//
// The GET must NOT carry seriesId: with it Sonarr lists the series' library folder
// instead of the download. Verified by hand on 34 files in 1.9 s (Lindy S02, 2026-10).
//
// ImportMissing is deliberately conservative: it imports only files the arr reports
// without rejections, that map to ONE series/movie for the whole folder (the target when
// one is given), and only into monitored episodes/movies that have NO file yet. It never
// replaces a file, so a re-run is a no-op for everything already imported.

// ImportTarget restricts ImportMissing to one series (optionally one season) or one
// movie. The zero value means "whatever the folder unambiguously maps to".
type ImportTarget struct {
	SeriesID int // Sonarr
	Season   int // Sonarr; 0 = any season
	MovieID  int // Radarr
}

// EpisodeNum is one season/episode number pair.
type EpisodeNum struct{ Season, Episode int }

// ImportResult is what ImportMissing found and did.
type ImportResult struct {
	Candidates int  // files the arr listed for the folder
	Ambiguous  bool // the files map to several series/movies and there is no target
	// Mismatch is set when the target cannot apply to this arr (a series target on
	// Radarr, a movie target on Sonarr).
	Mismatch bool
	SeriesID int    // the series imported into (Sonarr)
	MovieID  int    // the movie imported into (Radarr)
	Title    string // its title
	Selected int    // files sent to ManualImport
	Imported int    // of those, verified to have a file afterwards
	// Episodes are the episodes of the verified imported files (Sonarr), sorted.
	Episodes []EpisodeNum
	// Skipped counts files not imported, by reason (bounded set: rejected, unmatched,
	// other series, other movie, other season, has file, unmonitored, unknown episode,
	// duplicate, no quality, several files). A file the arr rejected only because its
	// episodes/movie already have a file counts as "has file", not "rejected".
	Skipped map[string]int
	// Command is the ManualImport command's final status ("" = none was sent;
	// "timeout" = still running when Lazarr stopped waiting) and Message its message.
	Command string
	Message string
}

// manualImportHTTPTimeout bounds the manualimport GET: the arr probes every file (media
// info), which takes far longer than the 15 s the other calls get.
const manualImportHTTPTimeout = 5 * time.Minute

// commandWait / commandPollInterval bound the wait for the ManualImport command.
// Variables so tests can shorten them.
var (
	commandWait         = 3 * time.Minute
	commandPollInterval = 2 * time.Second
)

// ---- wire shapes (subset) ------------------------------------------------------

type miRef struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

type miEpisode struct {
	ID            int `json:"id"`
	SeasonNumber  int `json:"seasonNumber"`
	EpisodeNumber int `json:"episodeNumber"`
}

type miRejection struct {
	Reason string `json:"reason"`
	Type   string `json:"type"`
}

// miItem is one ManualImportResource (Sonarr v4 / Radarr v5). Quality, languages,
// indexer flags and release type are passed back to the command exactly as the arr
// returned them.
type miItem struct {
	Path         string          `json:"path"`
	FolderName   string          `json:"folderName"`
	Series       *miRef          `json:"series"`
	Movie        *miRef          `json:"movie"`
	Episodes     []miEpisode     `json:"episodes"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages"`
	ReleaseGroup string          `json:"releaseGroup"`
	IndexerFlags json.RawMessage `json:"indexerFlags"`
	ReleaseType  json.RawMessage `json:"releaseType"`
	Rejections   []miRejection   `json:"rejections"`
}

// seriesEpisode is the slice of an EpisodeResource the missing-only rule reads.
type seriesEpisode struct {
	ID            int  `json:"id"`
	SeasonNumber  int  `json:"seasonNumber"`
	EpisodeNumber int  `json:"episodeNumber"`
	HasFile       bool `json:"hasFile"`
	Monitored     bool `json:"monitored"`
}

type movieState struct {
	Title     string `json:"title"`
	HasFile   bool   `json:"hasFile"`
	Monitored bool   `json:"monitored"`
}

type commandResource struct {
	ID      int    `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// ---- ImportMissing ---------------------------------------------------------------

// ImportMissing runs the arr's manual import for the files of one download folder (the
// path the arr sees: Lazarr's content_path for the hash), importing only what is safe
// (see the package comment above). downloadID is the infohash; it is sent upper-cased,
// as the arrs store a qBittorrent client's hash, so the import is recorded against the
// download. A transport error is returned as is; "nothing to do" is a result, not an
// error.
func (c *Client) ImportMissing(ctx context.Context, folder, downloadID string, t ImportTarget) (ImportResult, error) {
	res := ImportResult{Skipped: map[string]int{}}
	if (c.kind == KindSonarr && t.MovieID != 0 && t.SeriesID == 0) ||
		(c.kind != KindSonarr && (t.SeriesID != 0 || t.Season != 0) && t.MovieID == 0) {
		res.Mismatch = true
		return res, nil
	}
	items, err := c.manualImportList(ctx, folder)
	if err != nil {
		return res, err
	}
	res.Candidates = len(items)
	if len(items) == 0 {
		return res, nil
	}
	if c.kind == KindSonarr {
		return c.importEpisodes(ctx, items, strings.ToUpper(downloadID), t, res)
	}
	return c.importMovie(ctx, items, strings.ToUpper(downloadID), t, res)
}

// manualImportList is GET /api/v3/manualimport for a folder (no seriesId, no downloadId).
func (c *Client) manualImportList(ctx context.Context, folder string) ([]miItem, error) {
	q := url.Values{"folder": {folder}, "filterExistingFiles": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, playback.JoinURL(c.base, "/api/v3/manualimport", q), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Accept", "application/json")
	hc := *c.hc // same transport and redirect policy, longer timeout
	hc.Timeout = manualImportHTTPTimeout
	body, _, err := playback.Do(ctx, &hc, req)
	if err != nil {
		return nil, fmt.Errorf("%s: manualimport: %w", c.kind, err)
	}
	var items []miItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("%s: decode manualimport: %w", c.kind, err)
	}
	return items, nil
}

// pickOwner returns the one series/movie id the batch maps to: the target when given,
// else the only id any file maps to. ambiguous = several ids and no target.
func pickOwner(ids []int, target int) (owner int, ambiguous bool) {
	if target != 0 {
		return target, false
	}
	seen := map[int]bool{}
	for _, id := range ids {
		if id > 0 {
			seen[id] = true
		}
	}
	if len(seen) > 1 {
		return 0, true
	}
	for id := range seen {
		owner = id
	}
	return owner, false
}

func (c *Client) importEpisodes(ctx context.Context, items []miItem, downloadID string, t ImportTarget, res ImportResult) (ImportResult, error) {
	ids := make([]int, 0, len(items))
	for _, it := range items {
		if it.Series != nil {
			ids = append(ids, it.Series.ID)
		}
	}
	owner, ambiguous := pickOwner(ids, t.SeriesID)
	if ambiguous {
		res.Ambiguous = true
		return res, nil
	}
	if owner == 0 {
		res.Skipped["unmatched"] = len(items)
		return res, nil
	}
	res.SeriesID = owner
	for _, it := range items {
		if it.Series != nil && it.Series.ID == owner && res.Title == "" {
			res.Title = it.Series.Title
		}
	}
	eps, err := c.seriesEpisodes(ctx, owner)
	if err != nil {
		return res, err
	}

	claimed := map[int]int{} // episode id -> index into picked
	var picked []miItem
	dup := map[int]bool{} // indexes into picked claimed twice
	for _, it := range items {
		reason := ""
		switch {
		case len(it.Rejections) > 0:
			reason = rejectionReason(it.Rejections)
		case it.Series == nil || len(it.Episodes) == 0:
			reason = "unmatched"
		case it.Series.ID != owner:
			reason = "other series"
		case len(it.Quality) == 0 || string(it.Quality) == "null":
			reason = "no quality"
		}
		for _, e := range it.Episodes {
			if reason != "" {
				break
			}
			cur, ok := eps[e.ID]
			switch {
			case !ok:
				reason = "unknown episode"
			case t.Season != 0 && cur.SeasonNumber != t.Season:
				reason = "other season"
			case cur.HasFile:
				reason = "has file"
			case !cur.Monitored:
				reason = "unmonitored"
			}
		}
		if reason != "" {
			res.Skipped[reason]++
			continue
		}
		idx := len(picked)
		picked = append(picked, it)
		for _, e := range it.Episodes {
			if prev, ok := claimed[e.ID]; ok {
				dup[prev], dup[idx] = true, true
			}
			claimed[e.ID] = idx
		}
	}
	// Two files for the same episode: the arr would pick one arbitrarily. Import neither.
	var files []map[string]any
	var sel []miItem
	for i, it := range picked {
		if dup[i] {
			res.Skipped["duplicate"]++
			continue
		}
		epIDs := make([]int, 0, len(it.Episodes))
		for _, e := range it.Episodes {
			epIDs = append(epIDs, e.ID)
		}
		f := commandFile(it, downloadID)
		f["seriesId"] = owner
		f["episodeIds"] = epIDs
		files = append(files, f)
		sel = append(sel, it)
	}
	res.Selected = len(files)
	if len(files) == 0 {
		return res, nil
	}
	if err := c.runManualImport(ctx, files, &res); err != nil {
		return res, err
	}

	// Verify: a file counts as imported once every episode it maps to has a file.
	after, err := c.seriesEpisodes(ctx, owner)
	if err != nil {
		return res, err
	}
	for _, it := range sel {
		ok := true
		for _, e := range it.Episodes {
			ok = ok && after[e.ID].HasFile
		}
		if !ok {
			continue
		}
		res.Imported++
		for _, e := range it.Episodes {
			res.Episodes = append(res.Episodes, EpisodeNum{after[e.ID].SeasonNumber, after[e.ID].EpisodeNumber})
		}
	}
	sort.Slice(res.Episodes, func(i, j int) bool {
		a, b := res.Episodes[i], res.Episodes[j]
		return a.Season < b.Season || (a.Season == b.Season && a.Episode < b.Episode)
	})
	return res, nil
}

func (c *Client) importMovie(ctx context.Context, items []miItem, downloadID string, t ImportTarget, res ImportResult) (ImportResult, error) {
	ids := make([]int, 0, len(items))
	for _, it := range items {
		if it.Movie != nil {
			ids = append(ids, it.Movie.ID)
		}
	}
	owner, ambiguous := pickOwner(ids, t.MovieID)
	if ambiguous {
		res.Ambiguous = true
		return res, nil
	}
	if owner == 0 {
		res.Skipped["unmatched"] = len(items)
		return res, nil
	}
	res.MovieID = owner
	var m movieState
	if err := c.get(ctx, "/api/v3/movie/"+strconv.Itoa(owner), nil, &m); err != nil {
		return res, err
	}
	res.Title = m.Title
	var picked []miItem
	for _, it := range items {
		reason := ""
		switch {
		case len(it.Rejections) > 0:
			reason = rejectionReason(it.Rejections)
		case it.Movie == nil:
			reason = "unmatched"
		case it.Movie.ID != owner:
			reason = "other movie"
		case len(it.Quality) == 0 || string(it.Quality) == "null":
			reason = "no quality"
		case m.HasFile:
			reason = "has file"
		case !m.Monitored:
			reason = "unmonitored"
		}
		if reason != "" {
			res.Skipped[reason]++
			continue
		}
		picked = append(picked, it)
	}
	if len(picked) > 1 {
		// Several importable files for one movie (cuts, parts, an extra the arr did not
		// reject): which one is the movie is a human decision.
		res.Skipped["several files"] += len(picked)
		return res, nil
	}
	if len(picked) == 0 {
		return res, nil
	}
	f := commandFile(picked[0], downloadID)
	f["movieId"] = owner
	res.Selected = 1
	if err := c.runManualImport(ctx, []map[string]any{f}, &res); err != nil {
		return res, err
	}
	var after movieState
	if err := c.get(ctx, "/api/v3/movie/"+strconv.Itoa(owner), nil, &after); err != nil {
		return res, err
	}
	if after.HasFile {
		res.Imported = 1
	}
	return res, nil
}

// existingFileRejections are the arr's rejections that only say "the episode/movie
// already has a file at least this good" (Sonarr/Radarr UpgradeSpecification and
// AlreadyImportedSpecification wording, matched case-insensitively). Such a file will
// never import, however often the import is retried. Same list as the janitor's
// allowedRejections, plus "already has a file".
var existingFileRejections = []string{
	"not an upgrade",
	"not a custom format upgrade",
	"not a quality revision upgrade",
	"already imported",
	"existing file",
	"already has a file",
}

// rejectionReason classifies a rejected file: "has file" when every rejection is of the
// existing-file kind (final), else "rejected" (may clear on a later attempt).
func rejectionReason(rs []miRejection) string {
	for _, r := range rs {
		m := strings.ToLower(r.Reason)
		if !slices.ContainsFunc(existingFileRejections, func(a string) bool { return strings.Contains(m, a) }) {
			return "rejected"
		}
	}
	return "has file"
}

// commandFile builds one ManualImport file entry from what the arr returned.
func commandFile(it miItem, downloadID string) map[string]any {
	f := map[string]any{
		"path":         it.Path,
		"quality":      it.Quality,
		"releaseGroup": it.ReleaseGroup,
		"downloadId":   downloadID,
	}
	if it.FolderName != "" {
		f["folderName"] = it.FolderName
	}
	if len(it.Languages) > 0 && string(it.Languages) != "null" {
		f["languages"] = it.Languages
	} else {
		f["languages"] = []map[string]any{{"id": 0, "name": "Unknown"}}
	}
	if len(it.IndexerFlags) > 0 && string(it.IndexerFlags) != "null" {
		f["indexerFlags"] = it.IndexerFlags
	}
	if len(it.ReleaseType) > 0 && string(it.ReleaseType) != "null" {
		f["releaseType"] = it.ReleaseType
	}
	return f
}

// runManualImport posts the ManualImport command and waits (bounded) for it to finish.
func (c *Client) runManualImport(ctx context.Context, files []map[string]any, res *ImportResult) error {
	body := map[string]any{"name": "ManualImport", "importMode": "move", "files": files}
	var cmd commandResource
	if err := c.postJSONResult(ctx, "/api/v3/command", body, &cmd); err != nil {
		return fmt.Errorf("%s: ManualImport: %w", c.kind, err)
	}
	res.Command, res.Message = cmd.Status, cmd.Message
	if cmd.ID == 0 {
		return nil
	}
	deadline := time.Now().Add(commandWait)
	for !commandDone(res.Command) {
		if time.Now().After(deadline) {
			res.Command = "timeout"
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(commandPollInterval):
		}
		if err := c.get(ctx, "/api/v3/command/"+strconv.Itoa(cmd.ID), nil, &cmd); err != nil {
			var se *playback.StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				res.Command = "gone" // pruned from the arr's command list: verify decides
				return nil
			}
			return err
		}
		res.Command, res.Message = cmd.Status, cmd.Message
	}
	return nil
}

func commandDone(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "failed", "aborted", "cancelled", "orphaned":
		return true
	}
	return false
}

// seriesEpisodes returns every episode of a series by id.
func (c *Client) seriesEpisodes(ctx context.Context, seriesID int) (map[int]seriesEpisode, error) {
	var eps []seriesEpisode
	if err := c.get(ctx, "/api/v3/episode", url.Values{"seriesId": {strconv.Itoa(seriesID)}}, &eps); err != nil {
		return nil, err
	}
	out := make(map[int]seriesEpisode, len(eps))
	for _, e := range eps {
		out[e.ID] = e
	}
	return out, nil
}

// postJSONResult sends a JSON body and decodes the 2xx response into out.
func (c *Client) postJSONResult(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, playback.JoinURL(c.base, path, nil), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(resp)) == 0 {
		return nil
	}
	return json.Unmarshal(resp, out)
}

// Summary renders a result for the slow-lane note, e.g. "34 file(s) S02E01–E34" or
// "65 file(s) S01E01–E31, S02E01–E34". Ranges are min–max per season.
func (r ImportResult) Summary() string {
	if r.MovieID != 0 {
		if r.Title != "" {
			return fmt.Sprintf("%d file(s) %s", r.Imported, r.Title)
		}
		return fmt.Sprintf("%d file(s)", r.Imported)
	}
	var parts []string
	for i := 0; i < len(r.Episodes); {
		j := i
		for j+1 < len(r.Episodes) && r.Episodes[j+1].Season == r.Episodes[i].Season {
			j++
		}
		a, b := r.Episodes[i], r.Episodes[j]
		if a.Episode == b.Episode {
			parts = append(parts, fmt.Sprintf("S%02dE%02d", a.Season, a.Episode))
		} else {
			parts = append(parts, fmt.Sprintf("S%02dE%02d–E%02d", a.Season, a.Episode, b.Episode))
		}
		i = j + 1
	}
	s := fmt.Sprintf("%d file(s)", r.Imported)
	if len(parts) > 0 {
		s += " " + strings.Join(parts, ", ")
	}
	return s
}

// SkipSummary renders Skipped, e.g. "3 rejected, 2 has file" (sorted by reason).
func (r ImportResult) SkipSummary() string {
	keys := make([]string, 0, len(r.Skipped))
	for k, n := range r.Skipped {
		if n > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", r.Skipped[k], k))
	}
	return strings.Join(parts, ", ")
}
