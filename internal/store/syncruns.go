package store

import (
	"context"
	"fmt"
)

// What a device was actually told, and when.
//
// The table has been in the schema since the first milestone with nothing
// writing to it. It earns its place now for one reason: when someone says a book
// did not arrive, the only useful question is what the server sent and when, and
// last_sync_at alone cannot answer it.
//
// A sync that had nothing to say writes no row. Recording thousands of "nothing
// happened" entries would bury the handful that matter.

// SyncRun is one sync, from the device's first request to its last.
type SyncRun struct {
	DeviceID      int64
	SyncPointID   string
	StartedAt     string
	FinishedAt    string
	Requests      int
	NewBooks      int
	ChangedBooks  int
	RemovedBooks  int
	ReadingStates int
	Tags          int
	Status        string
}

// Total is everything sent, which is what a person reads first.
func (r SyncRun) Total() int {
	return r.NewBooks + r.ChangedBooks + r.RemovedBooks + r.ReadingStates + r.Tags
}

// StartSyncRun opens the record for a snapshot being drained.
func StartSyncRun(ctx context.Context, x Execer, deviceID int64, syncPointID string) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO sync_runs (device_id, sync_point_id, started_at, status)
		VALUES (?,?,?,'running')`, deviceID, syncPointID, Now())
	if err != nil {
		return fmt.Errorf("start sync run: %w", err)
	}
	return nil
}

// SyncCounts is what one request sent.
type SyncCounts struct {
	NewBooks      int
	ChangedBooks  int
	RemovedBooks  int
	ReadingStates int
	Tags          int
}

// AddSyncCounts adds one request's worth to the record.
func AddSyncCounts(ctx context.Context, x Execer, syncPointID string, c SyncCounts) error {
	_, err := x.ExecContext(ctx, `
		UPDATE sync_runs SET
			requests       = requests + 1,
			new_books      = new_books + ?,
			changed_books  = changed_books + ?,
			removed_books  = removed_books + ?,
			reading_states = reading_states + ?,
			tags           = tags + ?
		WHERE sync_point_id = ?`,
		c.NewBooks, c.ChangedBooks, c.RemovedBooks, c.ReadingStates, c.Tags, syncPointID)
	return err
}

// FinishSyncRun closes the record.
//
// A sync that sent nothing at all leaves no trace: a device checks in every few
// minutes, and a history of empty rows is a history nobody can read.
func FinishSyncRun(ctx context.Context, x Execer, syncPointID, status string) error {
	if _, err := x.ExecContext(ctx,
		`DELETE FROM sync_runs WHERE sync_point_id = ? AND requests <= 1
		   AND new_books + changed_books + removed_books + reading_states + tags = 0`,
		syncPointID); err != nil {
		return err
	}
	_, err := x.ExecContext(ctx,
		`UPDATE sync_runs SET finished_at = ?, status = ? WHERE sync_point_id = ?`,
		Now(), status, syncPointID)
	return err
}

// RecentSyncRuns lists what a device was told, newest first.
func RecentSyncRuns(ctx context.Context, q Querier, deviceID int64, limit int) ([]SyncRun, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT device_id, sync_point_id, started_at, COALESCE(finished_at, ''), requests,
		       new_books, changed_books, removed_books, reading_states, tags, status
		FROM sync_runs WHERE device_id = ?
		ORDER BY id DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SyncRun
	for rows.Next() {
		var r SyncRun
		if err := rows.Scan(&r.DeviceID, &r.SyncPointID, &r.StartedAt, &r.FinishedAt,
			&r.Requests, &r.NewBooks, &r.ChangedBooks, &r.RemovedBooks,
			&r.ReadingStates, &r.Tags, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TrimSyncRuns keeps the history from growing without bound. Called by the
// janitor, like every other cache here.
func TrimSyncRuns(ctx context.Context, x Execer, keepPerDevice int) error {
	_, err := x.ExecContext(ctx, `
		DELETE FROM sync_runs WHERE id NOT IN (
			SELECT id FROM sync_runs r
			WHERE (SELECT count(*) FROM sync_runs newer
			       WHERE newer.device_id = r.device_id AND newer.id > r.id) < ?
		)`, keepPerDevice)
	return err
}
