package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const slowLaneColumns = `hash, magnet, name, category, origin, state, torbox_id,
	queued_at, started_at, cached_at, finished_at, updated_at,
	progress, eta, speed, seeds, peers, size, attempts, last_error, note,
	target_series_id, target_season, target_movie_id, target_title, handoff,
	auto_import_attempts, auto_import_at, auto_import_done, auto_import_note`

// UpsertSlowLane inserts or replaces the item keyed by Hash.
func (s *sqliteStore) UpsertSlowLane(it *SlowLaneItem) error {
	var tg SlowLaneTarget
	if it.Target != nil {
		tg = *it.Target
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO slow_lane (`+slowLaneColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.Hash, it.Magnet, it.Name, it.Category, it.Origin, string(it.State), it.TorBoxID,
		it.QueuedAt, it.StartedAt, it.CachedAt, it.FinishedAt, it.UpdatedAt,
		it.Progress, it.ETA, it.Speed, it.Seeds, it.Peers, it.Size, it.Attempts, it.LastError, it.Note,
		tg.SeriesID, tg.Season, tg.MovieID, tg.Title, it.HandOff,
		it.AutoImportAttempts, it.AutoImportAt, it.AutoImportDone, it.AutoImportNote)
	if err != nil {
		return fmt.Errorf("catalog: upsert slow lane %q: %w", it.Hash, err)
	}
	return nil
}

// GetSlowLane returns the item or ErrNotFound.
func (s *sqliteStore) GetSlowLane(hash string) (*SlowLaneItem, error) {
	row := s.db.QueryRow(`SELECT `+slowLaneColumns+` FROM slow_lane WHERE hash = ?`, hash)
	it, err := scanSlowLane(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("slow lane %s: %w", hash, ErrNotFound)
	}
	return it, err
}

// ListSlowLane returns items in the given states (none = all), oldest queued first.
func (s *sqliteStore) ListSlowLane(states ...SlowLaneState) ([]*SlowLaneItem, error) {
	q := `SELECT ` + slowLaneColumns + ` FROM slow_lane`
	args := make([]any, 0, len(states))
	if len(states) > 0 {
		q += ` WHERE state IN (?` + strings.Repeat(",?", len(states)-1) + `)`
		for _, st := range states {
			args = append(args, string(st))
		}
	}
	rows, err := s.db.Query(q+` ORDER BY queued_at, hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: list slow lane: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*SlowLaneItem
	for rows.Next() {
		it, err := scanSlowLane(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate slow lane: %w", err)
	}
	return out, nil
}

// PruneSlowLane deletes finished items (handed off, imported, failed, cancelled) whose
// FinishedAt is before ts. Active items are never pruned.
func (s *sqliteStore) PruneSlowLane(before int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM slow_lane
		WHERE state NOT IN (?,?,?) AND finished_at > 0 AND finished_at < ?`,
		string(SlowQueued), string(SlowDownloading), string(SlowCached), before)
	if err != nil {
		return 0, fmt.Errorf("catalog: prune slow lane: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("catalog: prune slow lane: rows affected: %w", err)
	}
	return int(n), nil
}

func scanSlowLane(r interface{ Scan(dest ...any) error }) (*SlowLaneItem, error) {
	var it SlowLaneItem
	var st string
	var tg SlowLaneTarget
	if err := r.Scan(&it.Hash, &it.Magnet, &it.Name, &it.Category, &it.Origin, &st, &it.TorBoxID,
		&it.QueuedAt, &it.StartedAt, &it.CachedAt, &it.FinishedAt, &it.UpdatedAt,
		&it.Progress, &it.ETA, &it.Speed, &it.Seeds, &it.Peers, &it.Size, &it.Attempts, &it.LastError, &it.Note,
		&tg.SeriesID, &tg.Season, &tg.MovieID, &tg.Title, &it.HandOff,
		&it.AutoImportAttempts, &it.AutoImportAt, &it.AutoImportDone, &it.AutoImportNote); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("catalog: scan slow lane: %w", err)
	}
	it.State = SlowLaneState(st)
	if !tg.IsZero() {
		it.Target = &tg
	}
	return &it, nil
}
