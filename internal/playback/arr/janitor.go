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

// QueueRecord is the slice of a Sonarr v4 / Radarr v5 queue resource the pack
// janitor (and the Missing tab) reads (QueueResource: downloadClient, trackedDownloadState, statusMessages).
// Sonarr lists one record PER EPISODE of a multi-episode download, all sharing the
// download id.
type QueueRecord struct {
	ID                   int             `json:"id"`
	DownloadID           string          `json:"downloadId"`
	Title                string          `json:"title"`
	DownloadClient       string          `json:"downloadClient"`
	TrackedDownloadState string          `json:"trackedDownloadState"`
	Status               string          `json:"status"`
	StatusMessages       []StatusMessage `json:"statusMessages"`
	// What the record is for (the Missing tab's "downloading" flag): Sonarr fills
	// SeriesID/EpisodeID/SeasonNumber, Radarr MovieID.
	SeriesID     int `json:"seriesId,omitempty"`
	EpisodeID    int `json:"episodeId,omitempty"`
	SeasonNumber int `json:"seasonNumber,omitempty"`
	MovieID      int `json:"movieId,omitempty"`
}

// StatusMessage is one TrackedDownloadStatusMessage: a title (usually a file name) and
// the import rejections for it.
type StatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

type queueRecordPage struct {
	TotalRecords int           `json:"totalRecords"`
	Records      []QueueRecord `json:"records"`
}

// Queue returns every queue record (paged; bounded like FindQueueItem).
func (c *Client) Queue(ctx context.Context) ([]QueueRecord, error) {
	unknown := "includeUnknownMovieItems"
	if c.kind == KindSonarr {
		unknown = "includeUnknownSeriesItems"
	}
	var out []QueueRecord
	for page := 1; page <= queueMaxPages; page++ {
		q := url.Values{
			"page":     {strconv.Itoa(page)},
			"pageSize": {strconv.Itoa(queuePageSize)},
			unknown:    {"false"},
		}
		var qp queueRecordPage
		if err := c.get(ctx, "/api/v3/queue", q, &qp); err != nil {
			return nil, err
		}
		out = append(out, qp.Records...)
		if len(qp.Records) < queuePageSize || (qp.TotalRecords > 0 && len(out) >= qp.TotalRecords) {
			break
		}
	}
	return out, nil
}

// ImportedFromDownload reports whether the arr's history shows at least one file of
// this download imported (eventType downloadFolderImported).
func (c *Client) ImportedFromDownload(ctx context.Context, hash string) (bool, error) {
	q := url.Values{
		"downloadId": {strings.ToUpper(hash)},
		"pageSize":   {"100"},
		"page":       {"1"},
	}
	var hp historyPage
	if err := c.get(ctx, "/api/v3/history", q, &hp); err != nil {
		return false, err
	}
	for _, r := range hp.Records {
		if strings.EqualFold(r.DownloadID, hash) && strings.EqualFold(r.EventType, "downloadFolderImported") {
			return true, nil
		}
	}
	return false, nil
}

// RemoveQueueItemKeep deletes one queue item WITHOUT failing the release:
//
//	DELETE /api/v3/queue/{id}?removeFromClient=true&blocklist=false&skipRedownload=true
//
// No blocklist and no re-search; removeFromClient makes the arr call the client's
// torrents/delete, which Lazarr (policy.detach_on_delete) turns into a detach while
// imported library links exist. A 404 returns ErrNotInQueue.
func (c *Client) RemoveQueueItemKeep(ctx context.Context, id int) error {
	q := url.Values{
		"removeFromClient": {"true"},
		"blocklist":        {"false"},
		"skipRedownload":   {"true"},
	}
	_, err := c.send(ctx, http.MethodDelete, "/api/v3/queue/"+strconv.Itoa(id), q)
	var se *playback.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return ErrNotInQueue
	}
	return err
}
