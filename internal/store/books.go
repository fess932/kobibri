package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrBookNotFound is returned when a canonical id does not resolve.
var ErrBookNotFound = errors.New("book not found")

// maxAliasDepth bounds merged_into chasing. Chains are normally one link deep;
// the bound exists so a cycle introduced by a bug cannot hang a request.
const maxAliasDepth = 8

// ResolveBookID follows merged_into to the surviving canonical book. Old ids
// stay resolvable forever, because a device may still hold one.
func ResolveBookID(ctx context.Context, q Querier, id string) (string, error) {
	seen := make(map[string]bool, maxAliasDepth)
	for range maxAliasDepth {
		if seen[id] {
			return "", fmt.Errorf("merged_into cycle at book %s", id)
		}
		seen[id] = true

		var merged sql.NullString
		err := q.QueryRowContext(ctx, `SELECT merged_into FROM books WHERE id = ?`, id).Scan(&merged)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrBookNotFound, id)
		}
		if err != nil {
			return "", err
		}
		if !merged.Valid || merged.String == "" {
			return id, nil
		}
		id = merged.String
	}
	return "", fmt.Errorf("merged_into chain deeper than %d at book %s", maxAliasDepth, id)
}

// LookupIdentities returns the distinct canonical book ids the given identity
// keys resolve to, in a stable order.
func LookupIdentities(ctx context.Context, q Querier, keys []IdentityRow) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	var (
		clauses string
		args    []any
	)
	for i, k := range keys {
		if i > 0 {
			clauses += " OR "
		}
		clauses += "(kind = ? AND key = ?)"
		args = append(args, k.Kind, k.Key)
	}

	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT book_id FROM book_identities WHERE `+clauses, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		resolved, err := ResolveBookID(ctx, q, id)
		if err != nil {
			// A dangling identity row points at a book that no longer exists;
			// ignore it rather than failing the whole scan.
			if errors.Is(err, ErrBookNotFound) {
				continue
			}
			return nil, err
		}
		if !seen[resolved] {
			seen[resolved] = true
			ids = append(ids, resolved)
		}
	}
	return ids, rows.Err()
}

// IdentityRow is one (kind, key) pair pointing at a canonical book.
type IdentityRow struct {
	Kind string
	Key  string
}

// CreateBook issues a brand new canonical id. This is the only place a book id
// comes into existence.
func CreateBook(ctx context.Context, x Execer) (string, error) {
	id := uuid.NewString()
	now := Now()
	_, err := x.ExecContext(ctx,
		`INSERT INTO books (id, created_at, updated_at) VALUES (?, ?, ?)`, id, now, now)
	if err != nil {
		return "", fmt.Errorf("create book: %w", err)
	}
	return id, nil
}

// AddIdentities records identity keys for a book. Keys already claimed by
// another book are left alone: the first claim wins, and a conflicting key is
// resolved by MergeBooks instead.
func AddIdentities(ctx context.Context, x Execer, bookID string, keys []IdentityRow) error {
	for _, k := range keys {
		_, err := x.ExecContext(ctx,
			`INSERT OR IGNORE INTO book_identities (kind, key, book_id) VALUES (?, ?, ?)`,
			k.Kind, k.Key, bookID)
		if err != nil {
			return fmt.Errorf("add identity %s=%s: %w", k.Kind, k.Key, err)
		}
	}
	return nil
}

// GetBook reads one canonical book by its exact id, without alias resolution.
func GetBook(ctx context.Context, q Querier, id string) (*Book, error) {
	row := q.QueryRowContext(ctx, bookColumns+` FROM books WHERE id = ?`, id)
	b, err := scanBook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrBookNotFound, id)
	}
	return b, err
}

const bookColumns = `SELECT id, merged_into, title, sort_title, authors_json, author_sort,
	series_name, series_index, series_uuid, description_html, publisher, published_at,
	language, isbn13, primary_source_book_id, cover_source_book_id, cover_image_id,
	download_format, convert_from, download_size, available, hidden, syncable, serving_hash,
	metadata_rev, created_at, updated_at, last_available_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanBook(row rowScanner) (*Book, error) {
	var (
		b      Book
		merged sql.NullString
	)
	err := row.Scan(&b.ID, &merged, &b.Title, &b.SortTitle, &b.AuthorsJSON, &b.AuthorSort,
		&b.SeriesName, &b.SeriesIndex, &b.SeriesUUID, &b.DescriptionHTML, &b.Publisher,
		&b.PublishedAt, &b.Language, &b.ISBN13, &b.PrimarySourceBookID, &b.CoverSourceBookID,
		&b.CoverImageID, &b.DownloadFormat, &b.ConvertFrom, &b.DownloadSize, &b.Available, &b.Hidden,
		&b.Syncable, &b.ServingHash, &b.MetadataRev, &b.CreatedAt, &b.UpdatedAt,
		&b.LastAvailableAt)
	if err != nil {
		return nil, err
	}
	b.MergedInto = merged.String
	return &b, nil
}

// UpdateBookDerived writes the merged view of a book. metadata_rev is bumped
// only when serving_hash changed, so a scan that re-reads unchanged books does
// not push an update to every device.
func UpdateBookDerived(ctx context.Context, x Execer, b *Book) error {
	_, err := x.ExecContext(ctx, `
		UPDATE books SET
			title = ?, sort_title = ?, authors_json = ?, author_sort = ?,
			series_name = ?, series_index = ?, series_uuid = ?, description_html = ?,
			publisher = ?, published_at = ?, language = ?, isbn13 = ?,
			primary_source_book_id = ?, cover_source_book_id = ?, cover_image_id = ?,
			download_format = ?, convert_from = ?, download_size = ?, available = ?, syncable = ?,
			serving_hash = ?, metadata_rev = ?, updated_at = ?, last_available_at = ?
		WHERE id = ?`,
		b.Title, b.SortTitle, b.AuthorsJSON, b.AuthorSort,
		b.SeriesName, b.SeriesIndex, b.SeriesUUID, b.DescriptionHTML,
		b.Publisher, b.PublishedAt, b.Language, b.ISBN13,
		b.PrimarySourceBookID, b.CoverSourceBookID, b.CoverImageID,
		b.DownloadFormat, b.ConvertFrom, b.DownloadSize, b.Available, b.Syncable,
		b.ServingHash, b.MetadataRev, Now(), b.LastAvailableAt,
		b.ID)
	if err != nil {
		return fmt.Errorf("update book %s: %w", b.ID, err)
	}
	return nil
}

// SetBookHidden toggles the admin "do not offer" flag, which is the intended
// way to push a book into a device's Archive.
func SetBookHidden(ctx context.Context, x Execer, id string, hidden bool) error {
	_, err := x.ExecContext(ctx,
		`UPDATE books SET hidden = ?, syncable = (available = 1 AND ? = 0 AND download_format <> ''),
		                  updated_at = ? WHERE id = ?`,
		hidden, hidden, Now(), id)
	return err
}

// MergeBooks folds loser into survivor. The loser's row is kept forever and
// marked with merged_into, so a device still holding the old id can resolve it
// for downloads, reading state and deletion.
//
// Everything attached to the loser moves across: source rows, identity keys,
// collection membership, reading progress (the newer revision wins) and
// tombstones (a device that deleted either id must not see the book return).
func MergeBooks(ctx context.Context, x Execer, survivor, loser string) error {
	if survivor == loser {
		return nil
	}

	steps := []struct {
		what  string
		query string
		args  []any
	}{
		{"source rows", `UPDATE source_books SET book_id = ? WHERE book_id = ?`, []any{survivor, loser}},
		{"identity keys", `UPDATE OR REPLACE book_identities SET book_id = ? WHERE book_id = ?`, []any{survivor, loser}},
		{"collection membership", `UPDATE OR IGNORE tag_books SET book_id = ? WHERE book_id = ?`, []any{survivor, loser}},
		{"leftover collection rows", `DELETE FROM tag_books WHERE book_id = ?`, []any{loser}},
		{"tombstones", `INSERT OR IGNORE INTO device_tombstones (device_id, book_id, created_at)
			SELECT device_id, ?, created_at FROM device_tombstones WHERE book_id = ?`, []any{survivor, loser}},
		{"old tombstones", `DELETE FROM device_tombstones WHERE book_id = ?`, []any{loser}},
		// Reading progress: keep the loser's only when it is further along.
		{"reading progress", `INSERT INTO reading_states
			(user_id, book_id, status, bookmark_json, statistics_json, rev,
			 last_writer_device_id, last_modified, priority_ts)
			SELECT user_id, ?, status, bookmark_json, statistics_json, rev,
			       last_writer_device_id, last_modified, priority_ts
			FROM reading_states WHERE book_id = ?
			ON CONFLICT(user_id, book_id) DO UPDATE SET
				status = excluded.status, bookmark_json = excluded.bookmark_json,
				statistics_json = excluded.statistics_json, rev = excluded.rev,
				last_writer_device_id = excluded.last_writer_device_id,
				last_modified = excluded.last_modified, priority_ts = excluded.priority_ts
			WHERE excluded.rev > reading_states.rev`, []any{survivor, loser}},
		{"old reading progress", `DELETE FROM reading_states WHERE book_id = ?`, []any{loser}},
		// A series set by hand follows the book it was set on. OR IGNORE, so a
		// survivor that already has one keeps it: the person editing the
		// survivor was looking at the book that survives.
		{"series override", `INSERT OR IGNORE INTO book_series_overrides
			(book_id, series_name, series_index, updated_at)
			SELECT ?, series_name, series_index, updated_at
			FROM book_series_overrides WHERE book_id = ?`, []any{survivor, loser}},
		{"old series override", `DELETE FROM book_series_overrides WHERE book_id = ?`, []any{loser}},
		{"alias", `UPDATE books SET merged_into = ?, syncable = 0, available = 0, updated_at = ?
			WHERE id = ?`, []any{survivor, Now(), loser}},
	}

	for _, s := range steps {
		if _, err := x.ExecContext(ctx, s.query, s.args...); err != nil {
			return fmt.Errorf("merge %s -> %s (%s): %w", loser, survivor, s.what, err)
		}
	}
	return nil
}

// PickSurvivor chooses which of several canonical books absorbs the others:
// oldest first, ties broken by the lexicographically smallest id. The rule is
// deterministic and independent of scan order, and it favours the id devices
// are most likely to already hold.
func PickSurvivor(ctx context.Context, q Querier, ids []string) (string, error) {
	if len(ids) == 0 {
		return "", errors.New("no candidates")
	}
	if len(ids) == 1 {
		return ids[0], nil
	}

	best, bestCreated := "", ""
	for _, id := range ids {
		var created string
		if err := q.QueryRowContext(ctx, `SELECT created_at FROM books WHERE id = ?`, id).Scan(&created); err != nil {
			return "", err
		}
		if best == "" || created < bestCreated || (created == bestCreated && id < best) {
			best, bestCreated = id, created
		}
	}
	return best, nil
}

// AwaitingConversionSQL is the one definition of "this book is queued for
// conversion and has not been converted yet", as a SQL boolean over the books
// table under the given alias.
//
// It exists because the answer was being spelled out separately in the prewarm
// queue and in two listings, and they disagreed. The listings asked only
// "syncable, offered as KEPUB, nothing in the cache" — which is true forever for
// a book whose source already holds a KEPUB, since that one is served untouched
// and never enters the queue. Such a book sat under "converting" for good, while
// its own page correctly called it ready.
//
// Anything that wants to show conversion state asks this, and the prewarmer
// selects on the same terms, so the queue and the interface cannot drift again.
func AwaitingConversionSQL(alias string) string {
	return `(` + alias + `.syncable = 1
		AND ` + alias + `.download_format = '` + FormatKEPUB + `'
		AND ` + alias + `.convert_from <> '` + FormatKEPUB + `'
		AND NOT EXISTS (SELECT 1 FROM kepub_cache kc WHERE kc.book_id = ` + alias + `.id))`
}
