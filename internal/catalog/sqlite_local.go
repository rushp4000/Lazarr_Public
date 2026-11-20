package catalog

import (
	"database/sql"
	"errors"
	"fmt"
)

// ---- Workstream G: local_file ------------------------------------------------

const localFileCols = `hash, file_id, state, linked, size, frontier, reason, class,
	pulled_at, last_read, error`

func (s *sqliteStore) UpsertLocalFile(lf *LocalFile) error {
	if lf.State == "" {
		lf.State = LocalPulling
	}
	_, err := s.db.Exec(`
		INSERT INTO local_file (`+localFileCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash, file_id) DO UPDATE SET
			state     = excluded.state,
			linked    = excluded.linked,
			size      = excluded.size,
			frontier  = excluded.frontier,
			reason    = excluded.reason,
			class     = excluded.class,
			pulled_at = excluded.pulled_at,
			last_read = excluded.last_read,
			error     = excluded.error`,
		lf.Hash, lf.FileID, string(lf.State), boolToInt(lf.Linked), lf.Size, lf.Frontier,
		lf.Reason, lf.Class, lf.PulledAt, lf.LastRead, lf.Error,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert local file (%q,%d): %w", lf.Hash, lf.FileID, err)
	}
	return nil
}

func (s *sqliteStore) GetLocalFile(hash string, fileID int) (*LocalFile, error) {
	row := s.db.QueryRow(`SELECT `+localFileCols+` FROM local_file WHERE hash = ? AND file_id = ?`, hash, fileID)
	lf, err := scanLocalFile(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalog: get local file (%q,%d): %w", hash, fileID, err)
	}
	return lf, nil
}

func (s *sqliteStore) ListLocalFiles(state LocalState) ([]*LocalFile, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if state == "" {
		rows, err = s.db.Query(`SELECT ` + localFileCols + ` FROM local_file ORDER BY hash, file_id`)
	} else {
		rows, err = s.db.Query(`SELECT `+localFileCols+` FROM local_file WHERE state = ? ORDER BY hash, file_id`, string(state))
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: list local files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectLocalFiles(rows)
}

func (s *sqliteStore) ListLocalFilesByHash(hash string) ([]*LocalFile, error) {
	rows, err := s.db.Query(`SELECT `+localFileCols+` FROM local_file WHERE hash = ? ORDER BY file_id`, hash)
	if err != nil {
		return nil, fmt.Errorf("catalog: list local files by hash %q: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	return collectLocalFiles(rows)
}

func (s *sqliteStore) DeleteLocalFile(hash string, fileID int) error {
	if _, err := s.db.Exec(`DELETE FROM local_file WHERE hash = ? AND file_id = ?`, hash, fileID); err != nil {
		return fmt.Errorf("catalog: delete local file (%q,%d): %w", hash, fileID, err)
	}
	return nil
}

func (s *sqliteStore) LocalBytes() (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`
		SELECT SUM(CASE WHEN state = 'complete' THEN size ELSE frontier END) FROM local_file`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("catalog: local bytes: %w", err)
	}
	return n.Int64, nil
}

func scanLocalFile(r rowScanner) (*LocalFile, error) {
	var lf LocalFile
	var state string
	var linked int
	if err := r.Scan(&lf.Hash, &lf.FileID, &state, &linked, &lf.Size, &lf.Frontier,
		&lf.Reason, &lf.Class, &lf.PulledAt, &lf.LastRead, &lf.Error); err != nil {
		return nil, err
	}
	lf.State = LocalState(state)
	lf.Linked = linked != 0
	return &lf, nil
}

func collectLocalFiles(rows *sql.Rows) ([]*LocalFile, error) {
	var out []*LocalFile
	for rows.Next() {
		lf, err := scanLocalFile(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: scan local file: %w", err)
		}
		out = append(out, lf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate local files: %w", err)
	}
	return out, nil
}

// ---- Workstream G: watch -------------------------------------------------------

const watchCols = `server, user_id, user_name, hash, file_id, series_key, season, episode,
	progress_pct, completed, first_seen, last_seen`

func (s *sqliteStore) UpsertWatch(w *Watch) error {
	_, err := s.db.Exec(`
		INSERT INTO watch (`+watchCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(server, user_id, hash, file_id) DO UPDATE SET
			user_name    = excluded.user_name,
			series_key   = excluded.series_key,
			season       = excluded.season,
			episode      = excluded.episode,
			progress_pct = excluded.progress_pct,
			completed    = MAX(watch.completed, excluded.completed),
			first_seen   = MIN(watch.first_seen, excluded.first_seen),
			last_seen    = MAX(watch.last_seen, excluded.last_seen)`,
		w.Server, w.UserID, w.UserName, w.Hash, w.FileID, w.SeriesKey, w.Season, w.Episode,
		w.ProgressPct, boolToInt(w.Completed), w.FirstSeen, w.LastSeen,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert watch (%s/%s %q,%d): %w", w.Server, w.UserID, w.Hash, w.FileID, err)
	}
	return nil
}

func (s *sqliteStore) ListWatchByFile(hash string, fileID int) ([]*Watch, error) {
	rows, err := s.db.Query(`SELECT `+watchCols+` FROM watch WHERE hash = ? AND file_id = ?`, hash, fileID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list watch by file: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectWatch(rows)
}

func (s *sqliteStore) ListWatchSince(since int64) ([]*Watch, error) {
	rows, err := s.db.Query(`SELECT `+watchCols+` FROM watch WHERE last_seen >= ?`, since)
	if err != nil {
		return nil, fmt.Errorf("catalog: list watch since: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectWatch(rows)
}

func collectWatch(rows *sql.Rows) ([]*Watch, error) {
	var out []*Watch
	for rows.Next() {
		var w Watch
		var completed int
		if err := rows.Scan(&w.Server, &w.UserID, &w.UserName, &w.Hash, &w.FileID, &w.SeriesKey,
			&w.Season, &w.Episode, &w.ProgressPct, &completed, &w.FirstSeen, &w.LastSeen); err != nil {
			return nil, fmt.Errorf("catalog: scan watch: %w", err)
		}
		w.Completed = completed != 0
		out = append(out, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate watch: %w", err)
	}
	return out, nil
}

// ---- Workstream G: library_link ------------------------------------------------

const linkCols = `link_path, hash, file_id, target_kind, last_verified`

func (s *sqliteStore) UpsertLibraryLink(l *LibraryLink) error {
	_, err := s.db.Exec(`
		INSERT INTO library_link (`+linkCols+`) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(link_path) DO UPDATE SET
			hash          = excluded.hash,
			file_id       = excluded.file_id,
			target_kind   = excluded.target_kind,
			last_verified = excluded.last_verified`,
		l.LinkPath, l.Hash, l.FileID, l.TargetKind, l.LastVerified)
	if err != nil {
		return fmt.Errorf("catalog: upsert library link %q: %w", l.LinkPath, err)
	}
	return nil
}

func (s *sqliteStore) ListLibraryLinks(hash string, fileID int) ([]*LibraryLink, error) {
	rows, err := s.db.Query(`SELECT `+linkCols+` FROM library_link WHERE hash = ? AND file_id = ? ORDER BY link_path`, hash, fileID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list library links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectLinks(rows)
}

func (s *sqliteStore) ListAllLibraryLinks() ([]*LibraryLink, error) {
	rows, err := s.db.Query(`SELECT ` + linkCols + ` FROM library_link ORDER BY link_path`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list all library links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return collectLinks(rows)
}

func (s *sqliteStore) DeleteLibraryLink(linkPath string) error {
	if _, err := s.db.Exec(`DELETE FROM library_link WHERE link_path = ?`, linkPath); err != nil {
		return fmt.Errorf("catalog: delete library link %q: %w", linkPath, err)
	}
	return nil
}

func (s *sqliteStore) PruneLibraryLinks(before int64) (int, error) {
	res, err := s.db.Exec(`DELETE FROM library_link WHERE last_verified < ?`, before)
	if err != nil {
		return 0, fmt.Errorf("catalog: prune library links: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func collectLinks(rows *sql.Rows) ([]*LibraryLink, error) {
	var out []*LibraryLink
	for rows.Next() {
		var l LibraryLink
		if err := rows.Scan(&l.LinkPath, &l.Hash, &l.FileID, &l.TargetKind, &l.LastVerified); err != nil {
			return nil, fmt.Errorf("catalog: scan library link: %w", err)
		}
		out = append(out, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate library links: %w", err)
	}
	return out, nil
}

// ---- post-pull analysis (docs/28 §16) -------------------------------------------

const analysisCols = `hash, file_id, server, state, item_ids, requested_at, started_at,
	finished_at, attempts, intro, credits, error`

func (s *sqliteStore) UpsertLocalAnalysis(a *LocalAnalysis) error {
	if a.State == "" {
		a.State = AnalysisPending
	}
	_, err := s.db.Exec(`
		INSERT INTO local_analysis (`+analysisCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash, file_id, server) DO UPDATE SET
			state        = excluded.state,
			item_ids     = excluded.item_ids,
			requested_at = excluded.requested_at,
			started_at   = excluded.started_at,
			finished_at  = excluded.finished_at,
			attempts     = excluded.attempts,
			intro        = excluded.intro,
			credits      = excluded.credits,
			error        = excluded.error`,
		a.Hash, a.FileID, a.Server, string(a.State), a.ItemIDs, a.RequestedAt, a.StartedAt,
		a.FinishedAt, a.Attempts, a.Intro, a.Credits, a.Error,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert local analysis (%q,%d,%s): %w", a.Hash, a.FileID, a.Server, err)
	}
	return nil
}

func (s *sqliteStore) ListLocalAnalysis(hash string, fileID int) ([]*LocalAnalysis, error) {
	rows, err := s.db.Query(`SELECT `+analysisCols+` FROM local_analysis WHERE hash = ? AND file_id = ? ORDER BY server`, hash, fileID)
	if err != nil {
		return nil, fmt.Errorf("catalog: list local analysis: %w", err)
	}
	return scanAnalysisRows(rows)
}

func (s *sqliteStore) ListActiveAnalysis() ([]*LocalAnalysis, error) {
	rows, err := s.db.Query(`SELECT ` + analysisCols + ` FROM local_analysis WHERE state IN ('pending','running') ORDER BY requested_at`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list active analysis: %w", err)
	}
	return scanAnalysisRows(rows)
}

func scanAnalysisRows(rows *sql.Rows) ([]*LocalAnalysis, error) {
	defer func() { _ = rows.Close() }()
	var out []*LocalAnalysis
	for rows.Next() {
		a := &LocalAnalysis{}
		var state string
		if err := rows.Scan(&a.Hash, &a.FileID, &a.Server, &state, &a.ItemIDs, &a.RequestedAt,
			&a.StartedAt, &a.FinishedAt, &a.Attempts, &a.Intro, &a.Credits, &a.Error); err != nil {
			return nil, fmt.Errorf("catalog: scan local analysis: %w", err)
		}
		a.State = AnalysisState(state)
		out = append(out, a)
	}
	return out, rows.Err()
}

var _ AnalysisStore = (*sqliteStore)(nil)
