package calibre

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"
)

// idBatch bounds how many ids go into one `IN (...)` list.
const idBatch = 500

// Stubs reads the cheap id/uuid/last_modified row for every book, ordered by
// id. This is phase A of a scan: it detects what is new, what changed and what
// vanished without reading the rest of the schema.
func (d *DB) Stubs(ctx context.Context) ([]Stub, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id, uuid, last_modified FROM books ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read book stubs: %w", err)
	}
	defer rows.Close()

	var out []Stub
	for rows.Next() {
		var (
			s    Stub
			uuid sql.NullString
			lm   sql.NullString
		)
		if err := rows.Scan(&s.ID, &uuid, &lm); err != nil {
			return nil, err
		}
		s.UUID = strings.TrimSpace(uuid.String)
		s.LastModified = parseTime(lm.String)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Books reads the full record for the given ids. This is phase B of a scan and
// runs only for books that are new or whose last_modified changed.
//
// Each linked table is fetched with one query per batch and joined in Go. We
// deliberately avoid `group_concat(... ORDER BY ...)`: ordering inside an
// aggregate is only guaranteed from SQLite 3.44, and the bundled version varies.
// Books reads full records for the given ids.
//
// Columns names the custom columns to read alongside them; anything not listed
// is not touched, since reading every column of a large library on every scan
// would cost more than it is worth.
func (d *DB) Books(ctx context.Context, ids []int64, columns ...CustomColumn) ([]*Book, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	byID := make(map[int64]*Book, len(ids))
	order := make([]int64, 0, len(ids))

	for chunk := range batches(ids, idBatch) {
		if err := d.readCore(ctx, chunk, byID, &order); err != nil {
			return nil, err
		}
		if err := d.readAuthors(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readSeries(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readPublishers(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readLanguages(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readTags(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readComments(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readIdentifiers(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readFormats(ctx, chunk, byID); err != nil {
			return nil, err
		}
		if err := d.readColumns(ctx, chunk, byID, columns); err != nil {
			return nil, err
		}
	}

	out := make([]*Book, 0, len(order))
	for _, id := range order {
		b := byID[id]
		d.resolveFiles(b)
		out = append(out, b)
	}
	return out, nil
}

func (d *DB) readColumns(ctx context.Context, ids []int64, byID map[int64]*Book, columns []CustomColumn) error {
	for _, col := range columns {
		values, err := d.ColumnValues(ctx, col, ids)
		if err != nil {
			return err
		}
		for bookID, vs := range values {
			b, ok := byID[bookID]
			if !ok {
				continue
			}
			if b.Columns == nil {
				b.Columns = map[string][]string{}
			}
			b.Columns[col.Label] = vs
		}
	}
	return nil
}

func (d *DB) readCore(ctx context.Context, ids []int64, byID map[int64]*Book, order *[]int64) error {
	q := `SELECT id, uuid, title, sort, author_sort, path, has_cover,
	             pubdate, timestamp, last_modified, series_index
	      FROM books WHERE id IN (` + placeholders(len(ids)) + `) ORDER BY id`
	rows, err := d.db.QueryContext(ctx, q, args(ids)...)
	if err != nil {
		return fmt.Errorf("read books: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			b                                  Book
			uuid, sortTitle, authorSort, bpath sql.NullString
			pubdate, timestamp, lastMod        sql.NullString
			hasCover                           sql.NullBool
			seriesIndex                        sql.NullFloat64
		)
		if err := rows.Scan(&b.ID, &uuid, &b.Title, &sortTitle, &authorSort, &bpath,
			&hasCover, &pubdate, &timestamp, &lastMod, &seriesIndex); err != nil {
			return err
		}
		b.UUID = strings.TrimSpace(uuid.String)
		b.SortTitle = sortTitle.String
		b.AuthorSort = authorSort.String
		b.RelPath = strings.Trim(bpath.String, "/")
		b.HasCover = hasCover.Bool
		b.PubDate = parseTime(pubdate.String)
		b.Timestamp = parseTime(timestamp.String)
		b.LastModified = parseTime(lastMod.String)
		b.SeriesIndex = seriesIndex.Float64
		b.Identifiers = map[string]string{}

		byID[b.ID] = &b
		*order = append(*order, b.ID)
	}
	return rows.Err()
}

func (d *DB) readAuthors(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT bal.book, a.name, a.sort
	      FROM books_authors_link bal JOIN authors a ON a.id = bal.author
	      WHERE bal.book IN (` + placeholders(len(ids)) + `) ORDER BY bal.book, bal.id`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			name string
			sort sql.NullString
		)
		if err := rows.Scan(&book, &name, &sort); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.Authors = append(b.Authors, Author{Name: name, Sort: sort.String})
		}
		return nil
	})
}

func (d *DB) readSeries(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT bsl.book, s.name
	      FROM books_series_link bsl JOIN series s ON s.id = bsl.series
	      WHERE bsl.book IN (` + placeholders(len(ids)) + `)`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			name string
		)
		if err := rows.Scan(&book, &name); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.SeriesName = name
			b.HasSeries = true
		}
		return nil
	})
}

func (d *DB) readPublishers(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT bpl.book, p.name
	      FROM books_publishers_link bpl JOIN publishers p ON p.id = bpl.publisher
	      WHERE bpl.book IN (` + placeholders(len(ids)) + `)`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			name string
		)
		if err := rows.Scan(&book, &name); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.Publisher = name
		}
		return nil
	})
}

func (d *DB) readLanguages(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT bll.book, l.lang_code
	      FROM books_languages_link bll JOIN languages l ON l.id = bll.lang_code
	      WHERE bll.book IN (` + placeholders(len(ids)) + `) ORDER BY bll.book, bll.item_order`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			code string
		)
		if err := rows.Scan(&book, &code); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.Languages = append(b.Languages, code)
		}
		return nil
	})
}

func (d *DB) readTags(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT btl.book, t.name
	      FROM books_tags_link btl JOIN tags t ON t.id = btl.tag
	      WHERE btl.book IN (` + placeholders(len(ids)) + `) ORDER BY btl.book, t.name`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			name string
		)
		if err := rows.Scan(&book, &name); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.Tags = append(b.Tags, name)
		}
		return nil
	})
}

func (d *DB) readComments(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT book, text FROM comments WHERE book IN (` + placeholders(len(ids)) + `)`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			text string
		)
		if err := rows.Scan(&book, &text); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.Description = text
		}
		return nil
	})
}

func (d *DB) readIdentifiers(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT book, type, val FROM identifiers WHERE book IN (` + placeholders(len(ids)) + `)`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book     int64
			typ, val string
		)
		if err := rows.Scan(&book, &typ, &val); err != nil {
			return err
		}
		if b := byID[book]; b != nil {
			b.Identifiers[strings.ToLower(strings.TrimSpace(typ))] = strings.TrimSpace(val)
		}
		return nil
	})
}

func (d *DB) readFormats(ctx context.Context, ids []int64, byID map[int64]*Book) error {
	q := `SELECT book, format, name, uncompressed_size
	      FROM data WHERE book IN (` + placeholders(len(ids)) + `) ORDER BY book, format`
	return d.eachRow(ctx, q, ids, func(rows *sql.Rows) error {
		var (
			book int64
			f    Format
			size sql.NullInt64
		)
		if err := rows.Scan(&book, &f.Format, &f.Name, &size); err != nil {
			return err
		}
		f.Format = strings.ToUpper(strings.TrimSpace(f.Format))
		f.Size = size.Int64
		if b := byID[book]; b != nil {
			b.Formats = append(b.Formats, f)
		}
		return nil
	})
}

// resolveFiles turns Calibre's path/name pairs into real files, recording what
// is actually on disk. A `data` row whose file is missing is common after a
// manual move, so it is flagged rather than treated as an error.
func (d *DB) resolveFiles(b *Book) {
	for i := range b.Formats {
		f := &b.Formats[i]
		rel := path.Join(b.RelPath, f.Name+"."+strings.ToLower(f.Format))
		abs, err := safeJoin(d.libraryPath, rel)
		if err != nil {
			slog.Warn("skipping book file with a suspicious path",
				"library", d.libraryPath, "book", b.ID, "path", rel, "err", err)
			continue
		}
		f.RelPath = rel
		if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() {
			f.Present = true
			f.Size = fi.Size()
			f.Mtime = fi.ModTime().UnixNano()
		}
	}

	if !b.HasCover {
		return
	}
	rel := path.Join(b.RelPath, "cover.jpg")
	abs, err := safeJoin(d.libraryPath, rel)
	if err != nil {
		slog.Warn("skipping cover with a suspicious path",
			"library", d.libraryPath, "book", b.ID, "path", rel, "err", err)
		return
	}
	// has_cover=1 with no file on disk is treated as "no cover".
	if fi, err := os.Stat(abs); err == nil && fi.Mode().IsRegular() {
		b.CoverRelPath = rel
		b.CoverMtime = fi.ModTime().Unix()
		b.CoverSize = fi.Size()
	}
}

// CustomColumns lists the library's user-defined columns.
//
// They are not mapped onto Kobo metadata: the device's Genre field holds a
// category uuid from the store's own taxonomy, not free text, so putting a
// library's own words there would be ignored at best. They earn their keep as
// shelves instead.
func (d *DB) CustomColumns(ctx context.Context) ([]CustomColumn, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, label, name, datatype, is_multiple, normalized
		 FROM custom_columns WHERE mark_for_delete = 0 ORDER BY id`)
	if err != nil {
		// Very old libraries may not have the table at all.
		slog.Debug("custom_columns unavailable", "err", err)
		return nil, nil
	}
	defer rows.Close()

	var out []CustomColumn
	for rows.Next() {
		var c CustomColumn
		var multiple, normalized sql.NullBool
		if err := rows.Scan(&c.ID, &c.Label, &c.Name, &c.Datatype, &multiple, &normalized); err != nil {
			return nil, err
		}
		c.IsMultiple, c.Normalized = multiple.Bool, normalized.Bool
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) eachRow(ctx context.Context, query string, ids []int64, scan func(*sql.Rows) error) error {
	rows, err := d.db.QueryContext(ctx, query, args(ids)...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func batches(ids []int64, size int) func(func([]int64) bool) {
	return func(yield func([]int64) bool) {
		for start := 0; start < len(ids); start += size {
			end := min(start+size, len(ids))
			if !yield(ids[start:end]) {
				return
			}
		}
	}
}

// calibreTimeLayouts covers what Calibre actually writes. Values are usually
// "2024-01-02 10:11:12.123456+00:00" but hand-edited and migrated libraries
// contain several other shapes, and an unparseable timestamp must degrade to a
// zero time rather than fail the scan.
var calibreTimeLayouts = []string{
	"2006-01-02 15:04:05.999999-07:00",
	"2006-01-02 15:04:05.999999Z07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02T15:04:05.999999-07:00",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// undefinedYear is Calibre's UNDEFINED_DATE, datetime(101, 1, 1). It means the
// field is empty, and passing it on sends a device a publication date in the
// year 101. See docs/kobo-protocol.md §3.
const undefinedYear = 101

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range calibreTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Year() <= undefinedYear {
				return time.Time{}
			}
			return t.UTC()
		}
	}
	slog.Debug("unparseable calibre timestamp", "value", s)
	return time.Time{}
}

// ColumnValues reads one custom column for a set of books.
//
// Calibre stores a column in one of two shapes and says which by the
// `normalized` flag: a normalized column keeps its values in a table of their
// own with a link table, exactly as tags do, and a plain one keeps the value on
// the row. Guessing between them by datatype is how other readers of this schema
// get it wrong.
//
// A column that is missing, or stored in some shape this does not know, yields
// nothing rather than failing the scan: it is an optional convenience, not
// something a library should break over.
func (d *DB) ColumnValues(ctx context.Context, col CustomColumn, ids []int64) (map[int64][]string, error) {
	if len(ids) == 0 || !col.UsableForShelves() {
		return nil, nil
	}

	table := fmt.Sprintf("custom_column_%d", col.ID)
	query := fmt.Sprintf(`SELECT book, value FROM %s WHERE book IN (%%s)`, table)
	if col.Normalized {
		link := fmt.Sprintf("books_custom_column_%d_link", col.ID)
		query = fmt.Sprintf(
			`SELECT l.book, c.value FROM %s l JOIN %s c ON c.id = l.value WHERE l.book IN (%%s)`,
			link, table)
	}

	out := map[int64][]string{}
	for chunk := range batches(ids, idBatch) {
		q := fmt.Sprintf(query, placeholders(len(chunk)))
		rows, err := d.db.QueryContext(ctx, q, args(chunk)...)
		if err != nil {
			slog.Debug("custom column unreadable", "column", col.Label, "err", err)
			return nil, nil
		}
		for rows.Next() {
			var book int64
			var value sql.NullString
			if err := rows.Scan(&book, &value); err != nil {
				rows.Close()
				return nil, err
			}
			if v := strings.TrimSpace(value.String); v != "" {
				out[book] = append(out[book], v)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
