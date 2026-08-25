package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SeriesOverride is a series set here instead of in Calibre.
//
// Found reports whether the book has one at all, which is not the same as an
// empty Name: a row with an empty name says "this book is in no series", while
// no row at all says "whatever the library says".
type SeriesOverride struct {
	Found bool
	Name  string
	Index sql.NullFloat64
}

// GetSeriesOverride reads one book's override, if it has one.
func GetSeriesOverride(ctx context.Context, q Querier, bookID string) (SeriesOverride, error) {
	var o SeriesOverride
	err := q.QueryRowContext(ctx,
		`SELECT series_name, series_index FROM book_series_overrides WHERE book_id = ?`,
		bookID).Scan(&o.Name, &o.Index)
	if err == sql.ErrNoRows {
		return SeriesOverride{}, nil
	}
	if err != nil {
		return SeriesOverride{}, fmt.Errorf("series override for %s: %w", bookID, err)
	}
	o.Found = true
	return o, nil
}

// SetSeriesOverride records a series chosen here. An empty name is meaningful:
// it takes the book out of every series, which is why it is not the same as
// calling ClearSeriesOverride.
func SetSeriesOverride(ctx context.Context, x Execer, bookID, name string, index sql.NullFloat64) error {
	_, err := x.ExecContext(ctx, `
		INSERT INTO book_series_overrides (book_id, series_name, series_index, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(book_id) DO UPDATE SET
			series_name = excluded.series_name,
			series_index = excluded.series_index,
			updated_at = excluded.updated_at`,
		bookID, strings.TrimSpace(name), index, Now())
	if err != nil {
		return fmt.Errorf("set series override for %s: %w", bookID, err)
	}
	return nil
}

// ClearSeriesOverride hands the book back to whatever its library says.
func ClearSeriesOverride(ctx context.Context, x Execer, bookID string) error {
	_, err := x.ExecContext(ctx,
		`DELETE FROM book_series_overrides WHERE book_id = ?`, bookID)
	if err != nil {
		return fmt.Errorf("clear series override for %s: %w", bookID, err)
	}
	return nil
}

// SeriesRow is one series in the listing.
type SeriesRow struct {
	Name string
	UUID string
	// Books counts only what the asking person may see.
	Books int
	// Syncable counts how many of those will actually reach a reader.
	Syncable int
	// CoverImageID is the first book's cover, for the card. Empty if the first
	// book has none.
	CoverImageID string
	// FirstBookID backs the cover URL, which is addressed by book.
	FirstBookID string
	// Overridden reports that at least one book in the series was put here by
	// hand rather than read out of a library.
	Overridden bool
}

// SeriesQuery filters the series listing.
type SeriesQuery struct {
	Search string
	// UserID limits the listing to series with a book this person may see, by
	// the same rule the sync snapshot uses. Zero means no restriction.
	UserID int64
}

// ListSeries returns every series with at least one visible book, by name.
//
// Series are grouped by name rather than by series_uuid even though the uuid is
// derived from the name: the uuid is a wire detail owed to the device, and
// grouping by the thing a person reads keeps the listing honest if that
// derivation ever changes.
func ListSeries(ctx context.Context, q Querier, f SeriesQuery) ([]SeriesRow, error) {
	where := []string{"b.merged_into IS NULL", "b.series_name <> ''"}
	var args []any

	if s := strings.TrimSpace(f.Search); s != "" {
		where = append(where, "b.series_name LIKE ?")
		args = append(args, "%"+s+"%")
	}
	if f.UserID > 0 {
		where = append(where, visibleToUser)
		args = append(args, f.UserID)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT b.series_name,
		       max(b.series_uuid),
		       count(*),
		       sum(b.syncable),
		       -- The cover comes from the book that opens the series, which is
		       -- the one a person pictures when they think of it.
		       (SELECT b2.cover_image_id FROM books b2
		         WHERE b2.merged_into IS NULL AND b2.series_name = b.series_name
		         ORDER BY b2.series_index IS NULL, b2.series_index, b2.sort_title
		         LIMIT 1),
		       (SELECT b2.id FROM books b2
		         WHERE b2.merged_into IS NULL AND b2.series_name = b.series_name
		         ORDER BY b2.series_index IS NULL, b2.series_index, b2.sort_title
		         LIMIT 1),
		       EXISTS (SELECT 1 FROM book_series_overrides o
		                JOIN books b3 ON b3.id = o.book_id
		               WHERE b3.series_name = b.series_name AND b3.merged_into IS NULL)
		FROM books b
		WHERE `+strings.Join(where, " AND ")+`
		GROUP BY b.series_name
		ORDER BY b.series_name COLLATE NOCASE`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []SeriesRow
	for rows.Next() {
		var r SeriesRow
		var cover, first sql.NullString
		if err := rows.Scan(&r.Name, &r.UUID, &r.Books, &r.Syncable,
			&cover, &first, &r.Overridden); err != nil {
			return nil, err
		}
		r.CoverImageID, r.FirstBookID = cover.String, first.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// visibleToUser is the sharing rule, written once so the series listing and the
// series page cannot drift from each other or from the library listing.
const visibleToUser = `EXISTS (SELECT 1 FROM source_books sb
	JOIN sources s ON s.id = sb.source_id
	LEFT JOIN source_acl a ON a.source_id = s.id AND a.user_id = ?
	WHERE sb.book_id = b.id AND sb.missing = 0 AND s.enabled = 1
	  AND (s.share_all = 1 OR a.user_id IS NOT NULL))`

// SeriesBooks returns the books of one series in reading order.
//
// A book with no index sorts last rather than first: an unnumbered extra is
// almost always a companion volume, and putting it before book one is worse
// than putting it after the last.
func SeriesBooks(ctx context.Context, q Querier, name string, userID, progressFor int64) ([]LibraryRow, error) {
	where := []string{"b.merged_into IS NULL", "b.series_name = ?"}
	args := []any{progressFor, name}

	if userID > 0 {
		where = append(where, visibleToUser)
		args = append(args, userID)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT b.id, b.title, b.authors_json, b.series_name, b.series_index,
		       b.download_format, b.available, b.hidden, b.syncable, b.cover_image_id,
		       (SELECT count(*) FROM source_books sb WHERE sb.book_id = b.id AND sb.missing = 0),
		       EXISTS (SELECT 1 FROM kepub_cache c WHERE c.book_id = b.id),
		       `+AwaitingConversionSQL("b")+`,
		       COALESCE(rs.status, ''), COALESCE(rs.bookmark_json, ''),
		       COALESCE(rs.last_modified, ''),
		       EXISTS (SELECT 1 FROM book_series_overrides o WHERE o.book_id = b.id)
		FROM books b
		LEFT JOIN reading_states rs ON rs.book_id = b.id AND rs.user_id = ?
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY b.series_index IS NULL, b.series_index, b.sort_title`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []LibraryRow
	for rows.Next() {
		var r LibraryRow
		var bookmark string
		if err := rows.Scan(&r.ID, &r.Title, &r.Authors, &r.SeriesName, &r.SeriesIndex,
			&r.Format, &r.Available, &r.Hidden, &r.Syncable, &r.CoverImageID,
			&r.SourceCount, &r.Converted, &r.Converting,
			&r.Progress.Status, &bookmark, &r.Progress.LastRead,
			&r.SeriesOverridden); err != nil {
			return nil, err
		}
		r.Progress.Percent = percentOf(bookmark)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeriesNames returns every series name in use, for the datalist behind the
// edit box: choosing an existing series by typing its exact name is what merges
// a stray book back into it.
func SeriesNames(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT DISTINCT series_name FROM books
		WHERE merged_into IS NULL AND series_name <> ''
		ORDER BY series_name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
