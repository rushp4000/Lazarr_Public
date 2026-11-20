package arr

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/your-username/lazarr/internal/playback"
)

// ErrNotInQueue means the arr's queue has no item for the download id. Right after
// a state change this can be a lag (the arr refreshes its queue from the client
// about once a minute); later it usually means someone already removed it.
var ErrNotInQueue = errors.New("arr: download not in queue")

// queuePageSize is the page size for the queue scan. Queues are small (tens to a
// few hundred items); 200 keeps it to one request in practice.
const queuePageSize = 200

// queueMaxPages bounds the scan (200 x 25 = 5000 items) so a misbehaving arr that
// never reports totalRecords cannot keep us paging forever.
const queueMaxPages = 25

type queuePage struct {
	Page         int         `json:"page"`
	PageSize     int         `json:"pageSize"`
	TotalRecords int         `json:"totalRecords"`
	Records      []queueItem `json:"records"`
}

type queueItem struct {
	ID         int    `json:"id"`
	DownloadID string `json:"downloadId"`
	Title      string `json:"title"`
}

// FindQueueItem returns the queue item id for the download id (infohash), matched
// case-insensitively (arrs store the client's hash upper-cased). Sonarr lists one
// item per episode of a multi-episode grab, all with the same download id; the
// first match is returned — removing any one of them fails the whole download.
// Returns ErrNotInQueue when nothing matches.
func (c *Client) FindQueueItem(ctx context.Context, hash string) (int, error) {
	// Items whose series/movie the arr no longer knows are left out (the default):
	// blocklisting and re-searching them is meaningless.
	unknown := "includeUnknownMovieItems"
	if c.kind == KindSonarr {
		unknown = "includeUnknownSeriesItems"
	}
	seen := 0
	for page := 1; page <= queueMaxPages; page++ {
		q := url.Values{
			"page":     {strconv.Itoa(page)},
			"pageSize": {strconv.Itoa(queuePageSize)},
			unknown:    {"false"},
		}
		var qp queuePage
		if err := c.get(ctx, "/api/v3/queue", q, &qp); err != nil {
			return 0, err
		}
		for _, it := range qp.Records {
			if it.ID != 0 && strings.EqualFold(it.DownloadID, hash) {
				return it.ID, nil
			}
		}
		seen += len(qp.Records)
		if len(qp.Records) < queuePageSize || (qp.TotalRecords > 0 && seen >= qp.TotalRecords) {
			break
		}
	}
	return 0, ErrNotInQueue
}

// RemoveQueueItemAsFailed deletes one queue item the way the arr UI's "Remove ->
// Blocklist release" does (Sonarr v4 / Radarr v5 QueueController.RemoveAction):
//
//	DELETE /api/v3/queue/{id}?removeFromClient=true&blocklist=true&skipRedownload=false
//
// blocklist=true runs the arr's failed-download handling (MarkAsFailed): the release
// is blocklisted and, with skipRedownload=false, a search for a replacement starts.
// removeFromClient=true makes the arr call the client's torrents/delete for the hash.
// A 404 (the item vanished between lookup and delete) returns ErrNotInQueue.
func (c *Client) RemoveQueueItemAsFailed(ctx context.Context, id int) error {
	q := url.Values{
		"removeFromClient": {"true"},
		"blocklist":        {"true"},
		"skipRedownload":   {"false"},
	}
	_, err := c.send(ctx, http.MethodDelete, "/api/v3/queue/"+strconv.Itoa(id), q)
	var se *playback.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return ErrNotInQueue
	}
	return err
}

// FailDownload finds the download's queue item and removes it as failed
// (blocklist + re-search). Returns ErrNotInQueue when the arr has no such item.
func (c *Client) FailDownload(ctx context.Context, hash string) error {
	id, err := c.FindQueueItem(ctx, hash)
	if err != nil {
		return err
	}
	return c.RemoveQueueItemAsFailed(ctx, id)
}
