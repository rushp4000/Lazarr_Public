package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/your-username/lazarr/internal/playback"
)

// Endpoints verified against PMS 1.43.4 on 2026-10-09 and the community OpenAPI spec
// (LukeHagar/plex-api-spec): each is PUT /library/metadata/{ids}/<path> and returns
// 200/202 immediately; the work runs as a background activity (GET /activities).
var actionPaths = map[string]string{
	"analyze":        "analyze",
	"intro":          "intro",   // + force=1 (verified: without it PMS does nothing)
	"credits":        "credits", // + force=1&manual=1 (verified: without them PMS does nothing)
	"voice_activity": "voiceActivity",
	"chapter_thumbs": "chapterThumbs",
	"index":          "index",
}

// actionQuery is the extra query each action needs to actually run (live, 2026-10-09).
var actionQuery = map[string]url.Values{
	"intro":   {"force": {"1"}},
	"credits": {"force": {"1"}, "manual": {"1"}},
}

// actionPrefs maps an action to the server-wide preference that gates it. With the
// preference at "never", PMS accepts the request (202) and silently does nothing.
var actionPrefs = map[string]string{
	"intro":          "GenerateIntroMarkerBehavior",
	"credits":        "GenerateCreditsMarkerBehavior",
	"voice_activity": "GenerateVADBehavior",
	"chapter_thumbs": "GenerateChapterThumbBehavior",
	"index":          "GenerateBIFBehavior",
	"analyze":        "LoudnessAnalysisBehavior",
}

var _ playback.Analyzer = (*Client)(nil)

func numericID(id string) error {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("plex: item id %q is not numeric", id)
	}
	return nil
}

// FindItems implements playback.Analyzer via /library/all?type=…&file=…. PMS matches
// `file` as a substring, so only exact Part.file matches are returned.
func (c *Client) FindItems(ctx context.Context, path string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, typ := range []string{"1", "4"} { // movie, episode
		q := url.Values{"type": {typ}, "file": {path}}
		req, err := c.newReq(ctx, http.MethodGet, "/library/all", q)
		if err != nil {
			return nil, err
		}
		body, ct, err := playback.Do(ctx, c.hc, req)
		if err != nil {
			return nil, fmt.Errorf("plex: find items: %w", err)
		}
		vids, err := decodeVideos(body, ct)
		if err != nil {
			return nil, err
		}
		for _, v := range vids {
			if seen[v.RatingKey] || numericID(v.RatingKey) != nil {
				continue
			}
			for _, m := range v.Media {
				for _, p := range m.Part {
					if p.File == path && !seen[v.RatingKey] {
						seen[v.RatingKey] = true
						ids = append(ids, v.RatingKey)
					}
				}
			}
		}
	}
	return ids, nil
}

// Preflight implements playback.Analyzer: Plex Pass + the gating server preferences.
func (c *Client) Preflight(ctx context.Context, actions []string) ([]string, error) {
	var warns []string
	req, err := c.newReq(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return nil, err
	}
	body, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("plex: preflight: %w", err)
	}
	var root struct {
		MediaContainer struct {
			MyPlexSubscription bool `json:"myPlexSubscription"`
		} `json:"MediaContainer"`
	}
	if json.Unmarshal(body, &root) == nil && !root.MediaContainer.MyPlexSubscription {
		warns = append(warns, "no active Plex Pass: intro/credits detection is unavailable")
	}
	req, err = c.newReq(ctx, http.MethodGet, "/:/prefs", nil)
	if err != nil {
		return nil, err
	}
	body, _, err = playback.Do(ctx, c.hc, req)
	if err != nil {
		return warns, fmt.Errorf("plex: preflight prefs: %w", err)
	}
	var prefs struct {
		MediaContainer struct {
			Setting []struct {
				ID    string          `json:"id"`
				Value json.RawMessage `json:"value"`
			} `json:"Setting"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &prefs); err != nil {
		return warns, fmt.Errorf("plex: decode prefs: %w", err)
	}
	vals := map[string]string{}
	for _, s := range prefs.MediaContainer.Setting {
		vals[s.ID] = strings.Trim(string(s.Value), `"`)
	}
	for _, a := range actions {
		if p := actionPrefs[a]; p != "" && vals[p] == "never" {
			warns = append(warns, fmt.Sprintf("action %q is ignored by Plex while %s=never", a, p))
		}
	}
	return warns, nil
}

// StartAnalysis implements playback.Analyzer.
func (c *Client) StartAnalysis(ctx context.Context, itemID string, actions []string) error {
	if err := numericID(itemID); err != nil {
		return err
	}
	for _, a := range actions {
		p, ok := actionPaths[a]
		if !ok {
			return fmt.Errorf("plex: unknown analysis action %q", a)
		}
		req, err := c.newReq(ctx, http.MethodPut, "/library/metadata/"+url.PathEscape(itemID)+"/"+p, actionQuery[a])
		if err != nil {
			return err
		}
		if _, _, err := playback.Do(ctx, c.hc, req); err != nil {
			return fmt.Errorf("plex: %s: %w", a, err)
		}
	}
	return nil
}

// AnalysisStatus implements playback.Analyzer. Busy is server-wide: any running
// media-generation or analysis activity counts (Lazarr analyzes one item at a time).
func (c *Client) AnalysisStatus(ctx context.Context, itemID string) (playback.AnalysisStatus, error) {
	var st playback.AnalysisStatus
	if err := numericID(itemID); err != nil {
		return st, err
	}
	req, err := c.newReq(ctx, http.MethodGet, "/activities", nil)
	if err != nil {
		return st, err
	}
	body, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return st, fmt.Errorf("plex: activities: %w", err)
	}
	var acts struct {
		MediaContainer struct {
			Activity []struct {
				Type string `json:"type"`
			} `json:"Activity"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &acts); err != nil {
		return st, fmt.Errorf("plex: decode activities: %w", err)
	}
	for _, a := range acts.MediaContainer.Activity {
		if strings.HasPrefix(a.Type, "media.generate") || strings.Contains(a.Type, "analy") {
			st.Busy = true
		}
	}
	req, err = c.newReq(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(itemID), url.Values{"includeMarkers": {"1"}})
	if err != nil {
		return st, err
	}
	body, _, err = playback.Do(ctx, c.hc, req)
	if err != nil {
		return st, fmt.Errorf("plex: item markers: %w", err)
	}
	var meta struct {
		MediaContainer struct {
			Metadata []struct {
				Marker []struct {
					Type string `json:"type"`
				} `json:"Marker"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return st, fmt.Errorf("plex: decode markers: %w", err)
	}
	for _, m := range meta.MediaContainer.Metadata {
		for _, mk := range m.Marker {
			switch mk.Type {
			case "intro":
				st.Intro++
			case "credits":
				st.Credits++
			}
		}
	}
	return st, nil
}

// gatePrefs are the server preferences OpenGates may flip, and only these two: the
// intro and credits detectors. Others (BIF, chapter thumbs, loudness) are left to the
// operator, because "scheduled" also arms the nightly butler over the whole library.
var gatePrefs = map[string]string{
	"intro":   "GenerateIntroMarkerBehavior",
	"credits": "GenerateCreditsMarkerBehavior",
}

func (c *Client) prefs(ctx context.Context) (map[string]string, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/:/prefs", nil)
	if err != nil {
		return nil, err
	}
	body, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("plex: prefs: %w", err)
	}
	var prefs struct {
		MediaContainer struct {
			Setting []struct {
				ID    string          `json:"id"`
				Value json.RawMessage `json:"value"`
			} `json:"Setting"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &prefs); err != nil {
		return nil, fmt.Errorf("plex: decode prefs: %w", err)
	}
	vals := map[string]string{}
	for _, s := range prefs.MediaContainer.Setting {
		vals[s.ID] = strings.Trim(string(s.Value), `"`)
	}
	return vals, nil
}

func (c *Client) setPrefs(ctx context.Context, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	q := url.Values{}
	for k, v := range kv {
		q.Set(k, v)
	}
	req, err := c.newReq(ctx, http.MethodPut, "/:/prefs", q)
	if err != nil {
		return err
	}
	if _, _, err := playback.Do(ctx, c.hc, req); err != nil {
		return fmt.Errorf("plex: set prefs: %w", err)
	}
	return nil
}

// OpenGates implements playback.Analyzer: every gate pref of the actions that is
// "never" becomes "scheduled" (not "asap", which would also fire on every new item)
// until restore puts "never" back.
func (c *Client) OpenGates(ctx context.Context, actions []string) (func(context.Context) error, error) {
	vals, err := c.prefs(ctx)
	if err != nil {
		return nil, err
	}
	opened := map[string]string{}
	closed := map[string]string{}
	for _, a := range actions {
		if p := gatePrefs[a]; p != "" && vals[p] == "never" {
			opened[p] = "scheduled"
			closed[p] = "never"
		}
	}
	if err := c.setPrefs(ctx, opened); err != nil {
		return nil, err
	}
	return func(rctx context.Context) error { return c.setPrefs(rctx, closed) }, nil
}

// ResetGates implements playback.Analyzer: the gate prefs of the actions go back to
// "never" unconditionally (called at start, so a crash mid-analysis can never leave
// the nightly butler armed over the FUSE library).
func (c *Client) ResetGates(ctx context.Context, actions []string) error {
	kv := map[string]string{}
	for _, a := range actions {
		if p := gatePrefs[a]; p != "" {
			kv[p] = "never"
		}
	}
	return c.setPrefs(ctx, kv)
}

// SeasonPaths implements playback.Analyzer via the item's parentRatingKey children.
func (c *Client) SeasonPaths(ctx context.Context, itemID string) ([]string, error) {
	if err := numericID(itemID); err != nil {
		return nil, err
	}
	req, err := c.newReq(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(itemID), nil)
	if err != nil {
		return nil, err
	}
	body, ct, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("plex: item: %w", err)
	}
	vids, err := decodeVideos(body, ct)
	if err != nil || len(vids) == 0 {
		return nil, err
	}
	if vids[0].Type != "episode" || numericID(vids[0].ParentRatingKey) != nil {
		return nil, nil
	}
	req, err = c.newReq(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(vids[0].ParentRatingKey)+"/children", nil)
	if err != nil {
		return nil, err
	}
	body, ct, err = playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("plex: season children: %w", err)
	}
	eps, err := decodeVideos(body, ct)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, v := range eps {
		if p := v.file(); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
