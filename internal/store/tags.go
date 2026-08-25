package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var ErrTagNotFound = errors.New("collection not found")

// Where a collection came from.
const (
	TagOriginDevice  = "device"
	TagOriginCalibre = "calibre"
	TagOriginServer  = "server"
)

// Tag is a collection: a named set of books, owned by a user and shared across
// that user's devices.
type Tag struct {
	ID           string
	UserID       int64
	Name         string
	Origin       string
	Rev          int64
	CreatedAt    string
	LastModified string
	DeletedAt    string
}

const tagColumns = `SELECT id, user_id, name, origin, rev, created_at, last_modified,
	COALESCE(deleted_at, '')`

func scanTag(row rowScanner) (*Tag, error) {
	var t Tag
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Origin, &t.Rev, &t.CreatedAt,
		&t.LastModified, &t.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func GetTag(ctx context.Context, q Querier, id string) (*Tag, error) {
	t, err := scanTag(q.QueryRowContext(ctx, tagColumns+` FROM tags WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrTagNotFound, id)
	}
	return t, err
}

// ListTags returns a user's live collections.
func ListTags(ctx context.Context, q Querier, userID int64) ([]*Tag, error) {
	rows, err := q.QueryContext(ctx,
		tagColumns+` FROM tags WHERE user_id = ? AND deleted_at IS NULL ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*Tag
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTag makes a collection and returns its id.
//
// A name that was deleted earlier is revived rather than colliding: the device
// has no idea we soft-delete, and refusing the create would leave it with a
// collection it cannot make.
func CreateTag(ctx context.Context, x Execer, userID int64, name, origin string) (string, error) {
	now := Now()

	var existingID string
	err := x.QueryRowContext(ctx,
		`SELECT id FROM tags WHERE user_id = ? AND name = ?`, userID, name).Scan(&existingID)
	if err == nil {
		_, err = x.ExecContext(ctx,
			`UPDATE tags SET deleted_at = NULL, rev = rev + 1, last_modified = ? WHERE id = ?`,
			now, existingID)
		return existingID, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	id := uuid.NewString()
	_, err = x.ExecContext(ctx, `
		INSERT INTO tags (id, user_id, name, origin, rev, created_at, last_modified)
		VALUES (?,?,?,?,1,?,?)`, id, userID, name, origin, now, now)
	if err != nil {
		return "", fmt.Errorf("create collection %q: %w", name, err)
	}
	return id, nil
}

// RenameTag changes a collection's name and bumps its revision, so other
// devices pick the change up on their next sync.
func RenameTag(ctx context.Context, x Execer, id, name string) error {
	res, err := x.ExecContext(ctx,
		`UPDATE tags SET name = ?, rev = rev + 1, last_modified = ? WHERE id = ?`,
		name, Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrTagNotFound, id)
	}
	return nil
}

// DeleteTag soft-deletes a collection.
//
// The row survives because the sync diff needs it: a collection that simply
// vanished from the table could never be announced as deleted, and would linger
// on every device forever.
func DeleteTag(ctx context.Context, x Execer, id string) error {
	res, err := x.ExecContext(ctx,
		`UPDATE tags SET deleted_at = ?, rev = rev + 1, last_modified = ?
		 WHERE id = ? AND deleted_at IS NULL`, Now(), Now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrTagNotFound, id)
	}
	return nil
}

// AddTagItems puts books into a collection. Ids are resolved through merge
// aliases first, so a device holding a pre-merge id still lands on the right
// book. Unknown ids are skipped rather than failing the request.
func AddTagItems(ctx context.Context, x Execer, tagID string, bookIDs []string) error {
	for _, raw := range bookIDs {
		resolved, err := ResolveBookID(ctx, x, raw)
		if err != nil {
			continue
		}
		if _, err := x.ExecContext(ctx,
			`INSERT OR IGNORE INTO tag_books (tag_id, book_id) VALUES (?,?)`,
			tagID, resolved); err != nil {
			return err
		}
	}
	return touchTag(ctx, x, tagID)
}

// RemoveTagItems takes books out of a collection.
func RemoveTagItems(ctx context.Context, x Execer, tagID string, bookIDs []string) error {
	for _, raw := range bookIDs {
		resolved, err := ResolveBookID(ctx, x, raw)
		if err != nil {
			resolved = raw // remove by the id we were given, whatever it is
		}
		if _, err := x.ExecContext(ctx,
			`DELETE FROM tag_books WHERE tag_id = ? AND book_id = ?`, tagID, resolved); err != nil {
			return err
		}
	}
	return touchTag(ctx, x, tagID)
}

// touchTag bumps the revision so a membership change is picked up by the diff,
// which compares revisions rather than contents.
func touchTag(ctx context.Context, x Execer, tagID string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE tags SET rev = rev + 1, last_modified = ? WHERE id = ?`, Now(), tagID)
	return err
}

// TagBookIDs lists a collection's members.
func TagBookIDs(ctx context.Context, q Querier, tagID string) ([]string, error) {
	return queryIDs(ctx, q,
		`SELECT book_id FROM tag_books WHERE tag_id = ? ORDER BY book_id`, tagID)
}
