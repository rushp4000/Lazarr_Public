package catalog

import (
	"database/sql"
	"errors"
	"fmt"
)

const replacementColumns = `category, item_id, kind, state, queued_at, searched_at, attempts, result`

var _ ReplacementStore = (*sqliteStore)(nil)

// GetReplacement returns the row or ErrNotFound.
func (s *sqliteStore) GetReplacement(category string, itemID int) (*ReplacementItem, error) {
	row := s.db.QueryRow(`SELECT `+replacementColumns+` FROM replacement_search
		WHERE category = ? AND item_id = ?`, category, itemID)
	it, err := scanReplacement(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("replacement %s/%d: %w", category, itemID, ErrNotFound)
	}
	return it, err
}

// UpsertReplacement inserts or replaces the row keyed by (Category, ItemID).
func (s *sqliteStore) UpsertReplacement(it *ReplacementItem) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO replacement_search (`+replacementColumns+`)
		VALUES (?,?,?,?,?,?,?,?)`,
		it.Category, it.ItemID, it.Kind, string(it.State), it.QueuedAt, it.SearchedAt, it.Attempts, it.Result)
	if err != nil {
		return fmt.Errorf("catalog: upsert replacement %s/%d: %w", it.Category, it.ItemID, err)
	}
	return nil
}

// ListPendingReplacements returns pending rows queued at or before ts, oldest first.
func (s *sqliteStore) ListPendingReplacements(queuedBefore int64, limit int) ([]*ReplacementItem, error) {
	rows, err := s.db.Query(`SELECT `+replacementColumns+` FROM replacement_search
		WHERE state = ? AND queued_at <= ? ORDER BY queued_at, category, item_id LIMIT ?`,
		string(ReplacementPending), queuedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("catalog: list replacements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*ReplacementItem
	for rows.Next() {
		it, err := scanReplacement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate replacements: %w", err)
	}
	return out, nil
}

// CountPendingReplacements counts pending rows.
func (s *sqliteStore) CountPendingReplacements() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM replacement_search WHERE state = ?`,
		string(ReplacementPending)).Scan(&n); err != nil {
		return 0, fmt.Errorf("catalog: count pending replacements: %w", err)
	}
	return n, nil
}

// CountReplacementSearchesSince counts rows whose last search was at or after ts.
func (s *sqliteStore) CountReplacementSearchesSince(ts int64) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM replacement_search WHERE searched_at > 0 AND searched_at >= ?`,
		ts).Scan(&n); err != nil {
		return 0, fmt.Errorf("catalog: count replacement searches: %w", err)
	}
	return n, nil
}

// PruneReplacements deletes done rows last touched (queued or searched) before ts.
func (s *sqliteStore) PruneReplacements(before int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM replacement_search
		WHERE state = ? AND queued_at < ? AND searched_at < ?`, string(ReplacementDone), before, before)
	if err != nil {
		return 0, fmt.Errorf("catalog: prune replacements: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("catalog: prune replacements: rows affected: %w", err)
	}
	return int(n), nil
}

func scanReplacement(r interface{ Scan(dest ...any) error }) (*ReplacementItem, error) {
	var it ReplacementItem
	var st string
	if err := r.Scan(&it.Category, &it.ItemID, &it.Kind, &st, &it.QueuedAt, &it.SearchedAt,
		&it.Attempts, &it.Result); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("catalog: scan replacement: %w", err)
	}
	it.State = ReplacementState(st)
	return &it, nil
}
