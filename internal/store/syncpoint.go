package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var ErrSyncPointNotFound = errors.New("sync point not found")

// Sync point states.
const (
	SyncStateOngoing   = "ongoing"
	SyncStateCompleted = "completed"
	SyncStateAbandoned = "abandoned"
)

// SyncCategory is one stage of the drain. The order is fixed and must not
// interleave: Komga established against real hardware that a device wants each
// category exhausted before the next begins.
type SyncCategory int

const (
	CatNewBooks SyncCategory = iota
	CatChangedBooks
	CatRemovedBooks
	CatReadingStates
	CatNewTags
	CatChangedTags
	CatDeletedTags
	CatDone
)

// SyncPoint is an immutable snapshot of what a device should hold.
type SyncPoint struct {
	ID           string
	DeviceID     int64
	ParentID     string
	State        string
	CursorCat    SyncCategory
	CursorKey    string
	RawKoboToken string
	ItemsSent    int
	CreatedAt    string
	UpdatedAt    string
	CompletedAt  string
	// Generation is what the library looked like when this snapshot was taken,
	// so a later sync can tell at a glance that nothing has changed.
	Generation Generation
}

const syncPointColumns = `SELECT id, device_id, COALESCE(parent_id, ''), state, cursor_cat,
	cursor_key, raw_kobo_token, items_sent, created_at, updated_at, COALESCE(completed_at, ''),
	generation`

func scanSyncPoint(row rowScanner) (*SyncPoint, error) {
	var sp SyncPoint
	err := row.Scan(&sp.ID, &sp.DeviceID, &sp.ParentID, &sp.State, &sp.CursorCat,
		&sp.CursorKey, &sp.RawKoboToken, &sp.ItemsSent, &sp.CreatedAt, &sp.UpdatedAt,
		&sp.CompletedAt, &sp.Generation)
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

func GetSyncPoint(ctx context.Context, q Querier, id string) (*SyncPoint, error) {
	sp, err := scanSyncPoint(q.QueryRowContext(ctx, syncPointColumns+` FROM sync_points WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrSyncPointNotFound, id)
	}
	return sp, err
}

// LastCompletedSyncPoint returns the snapshot the device is known to hold.
func LastCompletedSyncPoint(ctx context.Context, q Querier, deviceID int64) (*SyncPoint, error) {
	sp, err := scanSyncPoint(q.QueryRowContext(ctx,
		syncPointColumns+` FROM sync_points WHERE device_id = ? AND state = ?
		 ORDER BY completed_at DESC LIMIT 1`, deviceID, SyncStateCompleted))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sp, err
}

// OngoingSyncPoint returns the snapshot a paused sync was draining, if any.
func OngoingSyncPoint(ctx context.Context, q Querier, deviceID int64) (*SyncPoint, error) {
	sp, err := scanSyncPoint(q.QueryRowContext(ctx,
		syncPointColumns+` FROM sync_points WHERE device_id = ? AND state = ?
		 ORDER BY created_at DESC LIMIT 1`, deviceID, SyncStateOngoing))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sp, err
}

// CreateSyncPoint materialises what a device should hold right now.
//
// The set is the currently syncable, visible books UNION whatever the parent
// snapshot held, MINUS this device's tombstones. That union is the whole
// mechanism behind the project's headline property: a book that has vanished
// from every source is still in the parent, so it is still here, so the diff
// produces nothing for it — the device is never told to remove a file it is
// happily holding. The tombstone subtraction happens after the union, so a book
// the user deleted on the device can never come back, not even on a full sync.
//
// Hidden books are excluded from the carry-forward, which is the one deliberate
// exception. The union exists to absorb *accidental* disappearance — an
// unmounted share, a deleted file, a disabled source. Hiding a book is an
// operator saying "take this off the device", so it must fall out of the
// snapshot and be retracted.
//
// Once written the snapshot is never modified. That is what lets an interrupted
// sync resume exactly where it stopped even while a scan rewrites the library
// underneath it.
func CreateSyncPoint(ctx context.Context, x Execer, deviceID, userID int64, parentID, rawToken string) (*SyncPoint, error) {
	id := uuid.NewString()
	now := Now()

	generation, err := LibraryGeneration(ctx, x, userID, deviceID)
	if err != nil {
		return nil, err
	}

	_, err = x.ExecContext(ctx, `
		INSERT INTO sync_points (id, device_id, parent_id, state, cursor_cat, cursor_key,
		                         raw_kobo_token, created_at, updated_at, generation)
		VALUES (?,?,?,?,0,'',?,?,?,?)`,
		id, deviceID, nullString(parentID), SyncStateOngoing, rawToken, now, now, string(generation))
	if err != nil {
		return nil, fmt.Errorf("create sync point: %w", err)
	}

	_, err = x.ExecContext(ctx, `
		INSERT INTO sync_point_books (sync_point_id, book_id, metadata_rev, reading_state_rev)
		SELECT ?, b.id, b.metadata_rev, COALESCE(rs.rev, 0)
		FROM books b
		LEFT JOIN reading_states rs ON rs.book_id = b.id AND rs.user_id = ?
		WHERE b.merged_into IS NULL
		  AND (
		        ( b.syncable = 1
		          AND EXISTS (SELECT 1 FROM source_books sb
		                      JOIN sources s ON s.id = sb.source_id
		                      LEFT JOIN source_acl a ON a.source_id = s.id AND a.user_id = ?
		                      WHERE sb.book_id = b.id AND sb.missing = 0 AND s.enabled = 1
		                        AND (s.share_all = 1 OR a.user_id IS NOT NULL)) )
		     OR ( b.hidden = 0
		          AND b.id IN (SELECT book_id FROM sync_point_books WHERE sync_point_id = ?) )
		      )
		  AND b.id NOT IN (SELECT book_id FROM device_tombstones WHERE device_id = ?)`,
		id, userID, userID, parentID, deviceID)
	if err != nil {
		return nil, fmt.Errorf("materialise sync point books: %w", err)
	}

	_, err = x.ExecContext(ctx, `
		INSERT INTO sync_point_tags (sync_point_id, tag_id, tag_rev)
		SELECT ?, t.id, t.rev FROM tags t WHERE t.user_id = ? AND t.deleted_at IS NULL`,
		id, userID)
	if err != nil {
		return nil, fmt.Errorf("materialise sync point tags: %w", err)
	}

	return GetSyncPoint(ctx, x, id)
}

// SaveSyncCursor records how far a paginated drain has got.
func SaveSyncCursor(ctx context.Context, x Execer, id string, cat SyncCategory, key string, itemsSent int) error {
	_, err := x.ExecContext(ctx,
		`UPDATE sync_points SET cursor_cat = ?, cursor_key = ?, items_sent = ?, updated_at = ?
		 WHERE id = ?`, cat, key, itemsSent, Now(), id)
	return err
}

// CompleteSyncPoint marks a sync finished and drops the parent snapshot.
//
// The parent is deleted only here, once the child is known to have been fully
// delivered. Until then it is the fallback for a device that reconnects with a
// stale token, so nothing can be lost by an interrupted sync.
func CompleteSyncPoint(ctx context.Context, x Execer, sp *SyncPoint) error {
	now := Now()
	if _, err := x.ExecContext(ctx,
		`UPDATE sync_points SET state = ?, completed_at = ?, cursor_cat = ?, updated_at = ?
		 WHERE id = ?`, SyncStateCompleted, now, CatDone, now, sp.ID); err != nil {
		return err
	}
	if sp.ParentID != "" {
		if _, err := x.ExecContext(ctx, `DELETE FROM sync_points WHERE id = ?`, sp.ParentID); err != nil {
			return err
		}
	}
	_, err := x.ExecContext(ctx,
		`UPDATE devices SET last_sync_at = ?, last_sync_status = 'ok' WHERE id = ?`, now, sp.DeviceID)
	return err
}

// AbandonSyncPoint retires a snapshot a device stopped draining.
func AbandonSyncPoint(ctx context.Context, x Execer, id string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE sync_points SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		SyncStateAbandoned, Now(), id, SyncStateOngoing)
	return err
}

// TouchDeviceSync records that a device checked in, for a sync that had nothing
// to say and therefore wrote no snapshot at all.
func TouchDeviceSync(ctx context.Context, x Execer, deviceID int64) error {
	_, err := x.ExecContext(ctx,
		`UPDATE devices SET last_sync_at = ?, last_sync_status = 'ok' WHERE id = ?`,
		Now(), deviceID)
	return err
}

// ResetDeviceSyncState drops every snapshot for a device, so its next sync is a
// full reconcile. Tombstones survive: this is a repair tool, not a way to
// resurrect books the user deleted on the device.
func ResetDeviceSyncState(ctx context.Context, x Execer, deviceID int64) error {
	_, err := x.ExecContext(ctx, `DELETE FROM sync_points WHERE device_id = ?`, deviceID)
	return err
}

// The diff queries. Each is keyset-paginated over two immutable snapshots, so
// it is deterministic and can be resumed from a cursor.

// NewBookIDs are in the new snapshot but not the old one.
func NewBookIDs(ctx context.Context, q Querier, from, to, cursor string, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT t.book_id FROM sync_point_books t
		WHERE t.sync_point_id = ? AND t.book_id > ?
		  AND NOT EXISTS (SELECT 1 FROM sync_point_books f
		                  WHERE f.sync_point_id = ? AND f.book_id = t.book_id)
		ORDER BY t.book_id LIMIT ?`, to, cursor, from, limit)
}

// ChangedBookIDs are in both snapshots with a different metadata revision.
func ChangedBookIDs(ctx context.Context, q Querier, from, to, cursor string, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT t.book_id FROM sync_point_books t
		JOIN sync_point_books f ON f.sync_point_id = ? AND f.book_id = t.book_id
		WHERE t.sync_point_id = ? AND t.book_id > ? AND t.metadata_rev <> f.metadata_rev
		ORDER BY t.book_id LIMIT ?`, from, to, cursor, limit)
}

// RemovedBookIDs were in the old snapshot and are not in the new one.
//
// In practice this fires only for a book the device itself just deleted (a
// harmless echo) or one an operator hid. A book that merely vanished from its
// source is carried forward by CreateSyncPoint and never appears here.
func RemovedBookIDs(ctx context.Context, q Querier, from, to, cursor string, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT f.book_id FROM sync_point_books f
		WHERE f.sync_point_id = ? AND f.book_id > ?
		  AND NOT EXISTS (SELECT 1 FROM sync_point_books t
		                  WHERE t.sync_point_id = ? AND t.book_id = f.book_id)
		ORDER BY f.book_id LIMIT ?`, from, cursor, to, limit)
}

// ChangedReadingStateIDs are books whose progress moved, excluding progress
// this very device reported: echoing it back would fight the device.
func ChangedReadingStateIDs(ctx context.Context, q Querier, from, to, cursor string, userID, deviceID int64, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT t.book_id FROM sync_point_books t
		JOIN sync_point_books f ON f.sync_point_id = ? AND f.book_id = t.book_id
		LEFT JOIN reading_states rs ON rs.user_id = ? AND rs.book_id = t.book_id
		WHERE t.sync_point_id = ? AND t.book_id > ?
		  AND t.reading_state_rev <> f.reading_state_rev
		  AND COALESCE(rs.last_writer_device_id, -1) <> ?
		ORDER BY t.book_id LIMIT ?`, from, userID, to, cursor, deviceID, limit)
}

func NewTagIDs(ctx context.Context, q Querier, from, to, cursor string, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT t.tag_id FROM sync_point_tags t
		WHERE t.sync_point_id = ? AND t.tag_id > ?
		  AND NOT EXISTS (SELECT 1 FROM sync_point_tags f
		                  WHERE f.sync_point_id = ? AND f.tag_id = t.tag_id)
		ORDER BY t.tag_id LIMIT ?`, to, cursor, from, limit)
}

func ChangedTagIDs(ctx context.Context, q Querier, from, to, cursor string, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT t.tag_id FROM sync_point_tags t
		JOIN sync_point_tags f ON f.sync_point_id = ? AND f.tag_id = t.tag_id
		WHERE t.sync_point_id = ? AND t.tag_id > ? AND t.tag_rev <> f.tag_rev
		ORDER BY t.tag_id LIMIT ?`, from, to, cursor, limit)
}

func DeletedTagIDs(ctx context.Context, q Querier, from, to, cursor string, limit int) ([]string, error) {
	return queryIDs(ctx, q, `
		SELECT f.tag_id FROM sync_point_tags f
		WHERE f.sync_point_id = ? AND f.tag_id > ?
		  AND NOT EXISTS (SELECT 1 FROM sync_point_tags t
		                  WHERE t.sync_point_id = ? AND t.tag_id = f.tag_id)
		ORDER BY f.tag_id LIMIT ?`, from, cursor, to, limit)
}

func queryIDs(ctx context.Context, q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GCSyncPoints removes stale snapshots. A device's single completed snapshot is
// never touched: it is the baseline its next sync diffs against.
func GCSyncPoints(ctx context.Context, x Execer, olderThan string) (int64, error) {
	res, err := x.ExecContext(ctx,
		`DELETE FROM sync_points WHERE state IN (?, ?) AND updated_at < ?`,
		SyncStateOngoing, SyncStateAbandoned, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
