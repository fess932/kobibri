package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrSourceNotFound = errors.New("source not found")

const sourceColumns = `SELECT id, name, library_path, priority, enabled, share_all, kind,
	scan_interval_sec, COALESCE(last_scan_at, ''), COALESCE(last_ok_scan_at, ''),
	last_status, last_error, consecutive_fails, book_count, created_at`

func scanSource(row rowScanner) (*Source, error) {
	var s Source
	err := row.Scan(&s.ID, &s.Name, &s.LibraryPath, &s.Priority, &s.Enabled, &s.ShareAll,
		&s.Kind, &s.ScanIntervalSec, &s.LastScanAt, &s.LastOKScanAt, &s.LastStatus, &s.LastError,
		&s.ConsecutiveFails, &s.BookCount, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateSource registers a Calibre library.
func CreateSource(ctx context.Context, x Execer, s *Source) (int64, error) {
	if s.ScanIntervalSec <= 0 {
		s.ScanIntervalSec = 900
	}
	if s.Priority == 0 {
		s.Priority = 100
	}

	res, err := x.ExecContext(ctx, `
		INSERT INTO sources (name, library_path, priority, enabled, share_all,
		                     scan_interval_sec, last_status, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		s.Name, s.LibraryPath, s.Priority, s.Enabled, s.ShareAll,
		s.ScanIntervalSec, SourceStatusNever, Now())
	if err != nil {
		return 0, fmt.Errorf("create source %q: %w", s.Name, err)
	}
	id, err := res.LastInsertId()
	s.ID = id
	return id, err
}

func GetSource(ctx context.Context, q Querier, id int64) (*Source, error) {
	s, err := scanSource(q.QueryRowContext(ctx, sourceColumns+` FROM sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrSourceNotFound, id)
	}
	return s, err
}

// ListSources returns every source, ordered the way the winner rules read them.
func ListSources(ctx context.Context, q Querier) ([]*Source, error) {
	rows, err := q.QueryContext(ctx, sourceColumns+` FROM sources ORDER BY priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetSourceStatus records the outcome of a scan. An unreachable source keeps
// its previous book count and, critically, its rows: an unmounted NAS must
// never look like a library that lost every book.
func SetSourceStatus(ctx context.Context, x Execer, id int64, status, errMsg string) error {
	now := Now()
	switch status {
	case SourceStatusOK:
		_, err := x.ExecContext(ctx, `
			UPDATE sources SET last_status = ?, last_error = '', last_scan_at = ?,
				last_ok_scan_at = ?, consecutive_fails = 0,
				book_count = (SELECT count(*) FROM source_books
				              WHERE source_id = sources.id AND missing = 0)
			WHERE id = ?`, status, now, now, id)
		return err
	case SourceStatusRunning:
		_, err := x.ExecContext(ctx,
			`UPDATE sources SET last_status = ? WHERE id = ?`, status, id)
		return err
	default:
		_, err := x.ExecContext(ctx, `
			UPDATE sources SET last_status = ?, last_error = ?, last_scan_at = ?,
				consecutive_fails = consecutive_fails + 1
			WHERE id = ?`, status, errMsg, now, id)
		return err
	}
}

// SetSourceEnabled turns a source on or off. Disabling it withdraws its rows
// from winner selection without deleting anything.
func SetSourceEnabled(ctx context.Context, x Execer, id int64, enabled bool) error {
	_, err := x.ExecContext(ctx, `UPDATE sources SET enabled = ? WHERE id = ?`, enabled, id)
	return err
}

// StartScanRun opens a journal entry for a scan.
func StartScanRun(ctx context.Context, x Execer, sourceID int64) (int64, error) {
	res, err := x.ExecContext(ctx,
		`INSERT INTO scan_runs (source_id, started_at, status) VALUES (?,?,?)`,
		sourceID, Now(), SourceStatusRunning)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ScanCounts summarises what one scan did.
type ScanCounts struct {
	Seen     int
	Added    int
	Updated  int
	Vanished int
}

// FinishScanRun closes a journal entry.
func FinishScanRun(ctx context.Context, x Execer, runID int64, status, errMsg string, c ScanCounts) error {
	_, err := x.ExecContext(ctx, `
		UPDATE scan_runs SET finished_at = ?, status = ?, error = ?,
			seen = ?, added = ?, updated = ?, vanished = ?
		WHERE id = ?`,
		Now(), status, errMsg, c.Seen, c.Added, c.Updated, c.Vanished, runID)
	return err
}
