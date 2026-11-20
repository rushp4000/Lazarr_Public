// Package plex is the Plex Media Server client behind playback.Source. It reads
// /status/sessions (now playing) and /library/metadata/{show}/allLeaves (episode
// order) with an X-Plex-Token, preferring JSON and falling back to Plex's native XML.
// Verified against PMS 1.43 (docs/28 §3.2). Read-only except Refresh.
package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/playback"
)

// Client talks to one Plex server.
type Client struct {
	base  string
	token string
	hc    *http.Client
	now   func() time.Time
}

// New builds a client. hc may be nil (a bounded default is used).
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = playback.NewHTTPClient(15 * time.Second)
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc, now: time.Now}
}

func (c *Client) Name() string { return "plex" }

func (c *Client) newReq(ctx context.Context, method, path string, q url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, playback.JoinURL(c.base, path, q), nil)
	if err != nil {
		return nil, err
	}
	// Token goes in a header, never the query string (URLs end up in errors/logs).
	req.Header.Set("X-Plex-Token", c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", "lazarr")
	req.Header.Set("X-Plex-Product", "Lazarr")
	req.Header.Set("X-Plex-Version", "1")
	return req, nil
}

// ---- wire shapes ---------------------------------------------------------------

// flexInt tolerates Plex emitting a number or a quoted number.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		fl, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		n = int64(fl)
	}
	*f = flexInt(n)
	return nil
}

type container struct {
	MediaContainer struct {
		Metadata []video `json:"Metadata"`
	} `json:"MediaContainer"`
}

type video struct {
	RatingKey            string  `json:"ratingKey" xml:"ratingKey,attr"`
	Type                 string  `json:"type" xml:"type,attr"`
	Title                string  `json:"title" xml:"title,attr"`
	GrandparentRatingKey string  `json:"grandparentRatingKey" xml:"grandparentRatingKey,attr"`
	GrandparentTitle     string  `json:"grandparentTitle" xml:"grandparentTitle,attr"`
	ParentRatingKey      string  `json:"parentRatingKey" xml:"parentRatingKey,attr"`
	ParentIndex          flexInt `json:"parentIndex" xml:"parentIndex,attr"`
	Index                flexInt `json:"index" xml:"index,attr"`
	Duration             flexInt `json:"duration" xml:"duration,attr"`
	ViewOffset           flexInt `json:"viewOffset" xml:"viewOffset,attr"`
	SessionKey           string  `json:"sessionKey" xml:"sessionKey,attr"`
	Year                 flexInt `json:"year" xml:"year,attr"`
	OriginallyAvailable  string  `json:"originallyAvailableAt" xml:"originallyAvailableAt,attr"`
	Media                []media `json:"Media" xml:"Media"`
	User                 *user   `json:"User" xml:"User"`
	Player               *player `json:"Player" xml:"Player"`
}

type media struct {
	Part []part `json:"Part" xml:"Part"`
}

type part struct {
	File string `json:"file" xml:"file,attr"`
}

type user struct {
	ID    flexInt `json:"id" xml:"id,attr"`
	Title string  `json:"title" xml:"title,attr"`
}

type player struct {
	State             string `json:"state" xml:"state,attr"`
	MachineIdentifier string `json:"machineIdentifier" xml:"machineIdentifier,attr"`
}

type xmlContainer struct {
	XMLName xml.Name `xml:"MediaContainer"`
	Videos  []video  `xml:"Video"`
}

// UnmarshalXMLAttr lets flexInt decode from XML attributes.
func (f *flexInt) UnmarshalXMLAttr(attr xml.Attr) error {
	return f.UnmarshalJSON([]byte(`"` + attr.Value + `"`))
}

// decodeVideos handles both JSON and XML bodies.
func decodeVideos(body []byte, contentType string) ([]video, error) {
	trim := bytes.TrimSpace(body)
	if len(trim) == 0 {
		return nil, nil
	}
	if strings.Contains(contentType, "json") || trim[0] == '{' {
		var c container
		if err := json.Unmarshal(trim, &c); err != nil {
			return nil, fmt.Errorf("plex: decode json: %w", err)
		}
		return c.MediaContainer.Metadata, nil
	}
	var xc xmlContainer
	if err := xml.Unmarshal(trim, &xc); err != nil {
		return nil, fmt.Errorf("plex: decode xml: %w", err)
	}
	return xc.Videos, nil
}

func (v video) file() string {
	for _, m := range v.Media {
		for _, p := range m.Part {
			if p.File != "" {
				return p.File
			}
		}
	}
	return ""
}

// ---- Source --------------------------------------------------------------------

// Sessions implements playback.Source.
func (c *Client) Sessions(ctx context.Context) ([]playback.Session, error) {
	req, err := c.newReq(ctx, http.MethodGet, "/status/sessions", nil)
	if err != nil {
		return nil, err
	}
	body, ct, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("plex: sessions: %w", err)
	}
	vids, err := decodeVideos(body, ct)
	if err != nil {
		return nil, err
	}
	now := c.now()
	var out []playback.Session
	for _, v := range vids {
		var kind playback.Kind
		switch v.Type {
		case "movie":
			kind = playback.KindMovie
		case "episode":
			kind = playback.KindEpisode
		default:
			continue // tracks, photos, clips: not ours
		}
		s := playback.Session{
			Server:      "plex",
			SessionID:   v.SessionKey,
			ItemID:      v.RatingKey,
			Kind:        kind,
			Title:       v.Title,
			SeriesKey:   v.GrandparentRatingKey,
			SeriesTitle: v.GrandparentTitle,
			Season:      int(v.ParentIndex),
			Episode:     int(v.Index),
			Path:        v.file(),
			Position:    time.Duration(v.ViewOffset) * time.Millisecond,
			Duration:    time.Duration(v.Duration) * time.Millisecond,
			State:       playback.StatePlaying,
			Year:        int(v.Year),
			Observed:    now,
		}
		if v.User != nil {
			s.UserID = strconv.FormatInt(int64(v.User.ID), 10)
			s.UserName = v.User.Title
		}
		if v.Player != nil {
			switch v.Player.State {
			case "paused":
				s.State = playback.StatePaused
			case "buffering":
				s.State = playback.StateBuffering
			case "stopped":
				s.State = playback.StateStopped
			}
			if s.SessionID == "" {
				s.SessionID = v.Player.MachineIdentifier
			}
		}
		if t, perr := time.Parse("2006-01-02", v.OriginallyAvailable); perr == nil {
			s.ReleaseDate = t
		}
		out = append(out, s)
	}
	return out, nil
}

// NextEpisodes implements playback.Source via /library/metadata/{show}/allLeaves.
func (c *Client) NextEpisodes(ctx context.Context, s playback.Session, n int) ([]playback.Item, error) {
	if s.Kind != playback.KindEpisode || s.SeriesKey == "" || n <= 0 {
		return nil, nil
	}
	if _, err := strconv.ParseInt(s.SeriesKey, 10, 64); err != nil {
		return nil, fmt.Errorf("plex: series key %q is not numeric", s.SeriesKey)
	}
	req, err := c.newReq(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(s.SeriesKey)+"/allLeaves", nil)
	if err != nil {
		return nil, err
	}
	body, ct, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("plex: allLeaves: %w", err)
	}
	vids, err := decodeVideos(body, ct)
	if err != nil {
		return nil, err
	}
	var eps []playback.Item
	for _, v := range vids {
		if v.Type != "episode" {
			continue
		}
		it := playback.Item{ItemID: v.RatingKey, Title: v.Title, Season: int(v.ParentIndex), Episode: int(v.Index), Path: v.file()}
		if playback.After(s, it.Season, it.Episode) && it.Season > 0 {
			eps = append(eps, it)
		}
	}
	sort.SliceStable(eps, func(i, j int) bool {
		return playback.SeriesLess(eps[i].Season, eps[i].Episode, eps[j].Season, eps[j].Episode)
	})
	if len(eps) > n {
		eps = eps[:n]
	}
	return eps, nil
}

// Refresh implements playback.Source: PUT /library/metadata/{key}/refresh.
func (c *Client) Refresh(ctx context.Context, itemID string) error {
	if _, err := strconv.ParseInt(itemID, 10, 64); err != nil {
		return fmt.Errorf("plex: item id %q is not numeric", itemID)
	}
	req, err := c.newReq(ctx, http.MethodPut, "/library/metadata/"+url.PathEscape(itemID)+"/refresh", nil)
	if err != nil {
		return err
	}
	_, _, err = playback.Do(ctx, c.hc, req)
	if err != nil {
		return fmt.Errorf("plex: refresh: %w", err)
	}
	return nil
}
