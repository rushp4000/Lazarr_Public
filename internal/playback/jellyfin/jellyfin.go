// Package jellyfin is the Jellyfin client behind playback.Source: /Sessions for now
// playing, /Shows/{id}/Episodes for series order, /Items/{id}/Refresh on request.
// Field shapes verified live against Jellyfin 12.0 (docs/28 §3.1). Ticks are 100 ns.
package jellyfin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/your-username/lazarr/internal/playback"
)

const ticksPerSecond = 10_000_000

// Client talks to one Jellyfin server.
type Client struct {
	base   string
	apiKey string
	hc     *http.Client
	now    func() time.Time
	// activeWithin bounds /Sessions to recently active ones (seconds).
	activeWithin int
}

// New builds a client. hc may be nil (a bounded default is used).
func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = playback.NewHTTPClient(15 * time.Second)
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), apiKey: apiKey, hc: hc, now: time.Now, activeWithin: 300}
}

func (c *Client) Name() string { return "jellyfin" }

func (c *Client) newReq(ctx context.Context, method, path string, q url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, playback.JoinURL(c.base, path, q), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization",
		`MediaBrowser Token="`+c.apiKey+`", Client="Lazarr", Device="Lazarr", DeviceId="lazarr", Version="1"`)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// ---- wire shapes ---------------------------------------------------------------

type session struct {
	ID             string     `json:"Id"`
	UserID         string     `json:"UserId"`
	UserName       string     `json:"UserName"`
	NowPlayingItem *item      `json:"NowPlayingItem"`
	PlayState      *playState `json:"PlayState"`
}

type playState struct {
	PositionTicks int64 `json:"PositionTicks"`
	IsPaused      bool  `json:"IsPaused"`
}

type item struct {
	ID                string        `json:"Id"`
	Type              string        `json:"Type"`
	Name              string        `json:"Name"`
	SeriesName        string        `json:"SeriesName"`
	SeriesID          string        `json:"SeriesId"`
	ParentIndexNumber *int          `json:"ParentIndexNumber"`
	IndexNumber       *int          `json:"IndexNumber"`
	Path              string        `json:"Path"`
	RunTimeTicks      int64         `json:"RunTimeTicks"`
	PremiereDate      string        `json:"PremiereDate"`
	ProductionYear    int           `json:"ProductionYear"`
	MediaSources      []mediaSource `json:"MediaSources"`
}

type mediaSource struct {
	Path string `json:"Path"`
}

type itemsPage struct {
	Items []item `json:"Items"`
}

func (it item) path() string {
	if it.Path != "" {
		return it.Path
	}
	for _, ms := range it.MediaSources {
		if ms.Path != "" {
			return ms.Path
		}
	}
	return ""
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ---- Source --------------------------------------------------------------------

// Sessions implements playback.Source.
func (c *Client) Sessions(ctx context.Context) ([]playback.Session, error) {
	q := url.Values{"ActiveWithinSeconds": {strconv.Itoa(c.activeWithin)}}
	req, err := c.newReq(ctx, http.MethodGet, "/Sessions", q)
	if err != nil {
		return nil, err
	}
	body, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: sessions: %w", err)
	}
	var raw []session
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("jellyfin: decode sessions: %w", err)
	}
	now := c.now()
	var out []playback.Session
	for _, s := range raw {
		it := s.NowPlayingItem
		if it == nil {
			continue
		}
		var kind playback.Kind
		switch it.Type {
		case "Movie":
			kind = playback.KindMovie
		case "Episode":
			kind = playback.KindEpisode
		default:
			continue
		}
		ps := playback.Session{
			Server:      "jellyfin",
			SessionID:   s.ID,
			UserID:      s.UserID,
			UserName:    s.UserName,
			ItemID:      it.ID,
			Kind:        kind,
			Title:       it.Name,
			SeriesKey:   it.SeriesID,
			SeriesTitle: it.SeriesName,
			Season:      deref(it.ParentIndexNumber),
			Episode:     deref(it.IndexNumber),
			Path:        it.path(),
			Duration:    time.Duration(it.RunTimeTicks/ticksPerSecond) * time.Second,
			State:       playback.StatePlaying,
			Year:        it.ProductionYear,
			Observed:    now,
		}
		if s.PlayState != nil {
			ps.Position = time.Duration(s.PlayState.PositionTicks/ticksPerSecond) * time.Second
			if s.PlayState.IsPaused {
				ps.State = playback.StatePaused
			}
		}
		if t, perr := time.Parse(time.RFC3339Nano, it.PremiereDate); perr == nil {
			ps.ReleaseDate = t
		} else if t, perr := time.Parse("2006-01-02T15:04:05.0000000Z", it.PremiereDate); perr == nil {
			ps.ReleaseDate = t
		}
		out = append(out, ps)
	}
	return out, nil
}

// NextEpisodes implements playback.Source via /Shows/{SeriesId}/Episodes.
func (c *Client) NextEpisodes(ctx context.Context, s playback.Session, n int) ([]playback.Item, error) {
	if s.Kind != playback.KindEpisode || s.SeriesKey == "" || n <= 0 {
		return nil, nil
	}
	q := url.Values{"Fields": {"Path"}}
	if s.UserID != "" {
		q.Set("UserId", s.UserID)
	}
	req, err := c.newReq(ctx, http.MethodGet, "/Shows/"+url.PathEscape(s.SeriesKey)+"/Episodes", q)
	if err != nil {
		return nil, err
	}
	body, _, err := playback.Do(ctx, c.hc, req)
	if err != nil {
		return nil, fmt.Errorf("jellyfin: episodes: %w", err)
	}
	var page itemsPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("jellyfin: decode episodes: %w", err)
	}
	var eps []playback.Item
	for _, it := range page.Items {
		if it.Type != "Episode" {
			continue
		}
		e := playback.Item{ItemID: it.ID, Title: it.Name, Season: deref(it.ParentIndexNumber), Episode: deref(it.IndexNumber), Path: it.path()}
		if e.Season > 0 && playback.After(s, e.Season, e.Episode) {
			eps = append(eps, e)
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

// Refresh implements playback.Source: POST /Items/{id}/Refresh.
func (c *Client) Refresh(ctx context.Context, itemID string) error {
	if itemID == "" || strings.ContainsAny(itemID, "/?#") {
		return fmt.Errorf("jellyfin: bad item id %q", itemID)
	}
	req, err := c.newReq(ctx, http.MethodPost, "/Items/"+url.PathEscape(itemID)+"/Refresh", nil)
	if err != nil {
		return err
	}
	if _, _, err := playback.Do(ctx, c.hc, req); err != nil {
		return fmt.Errorf("jellyfin: refresh: %w", err)
	}
	return nil
}
