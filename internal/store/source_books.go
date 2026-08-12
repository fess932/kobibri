package store

import (
	"context"
	"database/sql"
	"fmt"
)

// SourceStub is the minimal per-source row a scan compares against, so change
// and vanish detection does not read the whole table.
type SourceStub struct {
	ID                  int64
	CalibreID           int64
	CalibreLastModified string
	MetaHash            string
	BookID              string
	Missing             bool
}

// SourceStubs lists every stored row for a source, keyed by Calibre id.
func SourceStubs(ctx context.Context, q Querier, sourceID int64) (map[int64]SourceStub, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, calibre_id, calibre_last_modified, meta_hash, COALESCE(book_id, ''), missing
		 FROM source_books WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]SourceStub{}
	for rows.Next() {
		var s SourceStub
		if err := rows.Scan(&s.ID, &s.CalibreID, &s.CalibreLastModified,
			&s.MetaHash, &s.BookID, &s.Missing); err != nil {
			return nil, err
		}
		out[s.CalibreID] = s
	}
	return out, rows.Err()
}

// UpsertSourceBook inserts or updates one source row and returns its id. The
// row is keyed on (source_id, calibre_id); it is never deleted by ingest.
func UpsertSourceBook(ctx context.Context, x Execer, sb *SourceBook) (int64, error) {
	now := Now()
	if sb.FirstSeenAt == "" {
		sb.FirstSeenAt = now
	}
	sb.LastSeenAt = now

	_, err := x.ExecContext(ctx, `
		INSERT INTO source_books (
			source_id, calibre_id, calibre_uuid, title, sort_title, authors_json, author_sort,
			series_name, series_index, description_html, publisher, published_at, language,
			isbn13, identifiers_json, tags_json, rel_path, cover_rel_path, cover_mtime,
			calibre_last_modified, meta_hash, book_id, missing, first_seen_at, last_seen_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)
		ON CONFLICT(source_id, calibre_id) DO UPDATE SET
			calibre_uuid = excluded.calibre_uuid, title = excluded.title,
			sort_title = excluded.sort_title, authors_json = excluded.authors_json,
			author_sort = excluded.author_sort, series_name = excluded.series_name,
			series_index = excluded.series_index, description_html = excluded.description_html,
			publisher = excluded.publisher, published_at = excluded.published_at,
			language = excluded.language, isbn13 = excluded.isbn13,
			identifiers_json = excluded.identifiers_json, tags_json = excluded.tags_json,
			rel_path = excluded.rel_path, cover_rel_path = excluded.cover_rel_path,
			cover_mtime = excluded.cover_mtime,
			calibre_last_modified = excluded.calibre_last_modified,
			meta_hash = excluded.meta_hash,
			book_id = COALESCE(excluded.book_id, source_books.book_id),
			missing = 0, last_seen_at = excluded.last_seen_at`,
		sb.SourceID, sb.CalibreID, sb.CalibreUUID, sb.Title, sb.SortTitle, sb.AuthorsJSON,
		sb.AuthorSort, sb.SeriesName, sb.SeriesIndex, sb.DescriptionHTML, sb.Publisher,
		sb.PublishedAt, sb.Language, sb.ISBN13, sb.IdentifiersJSON, sb.TagsJSON, sb.RelPath,
		sb.CoverRelPath, sb.CoverMtime, sb.CalibreLastModified, sb.MetaHash,
		nullString(sb.BookID), sb.FirstSeenAt, sb.LastSeenAt)
	if err != nil {
		return 0, fmt.Errorf("upsert source book %d/%d: %w", sb.SourceID, sb.CalibreID, err)
	}

	var id int64
	err = x.QueryRowContext(ctx,
		`SELECT id FROM source_books WHERE source_id = ? AND calibre_id = ?`,
		sb.SourceID, sb.CalibreID).Scan(&id)
	if err != nil {
		return 0, err
	}
	sb.ID = id
	return id, nil
}

// SetSourceBookBookID attaches a source row to a canonical book.
func SetSourceBookBookID(ctx context.Context, x Execer, sourceBookID int64, bookID string) error {
	_, err := x.ExecContext(ctx,
		`UPDATE source_books SET book_id = ? WHERE id = ?`, bookID, sourceBookID)
	return err
}

// MarkSourceBooksMissing flags rows that vanished from the library. Rows are
// never deleted: the canonical book must keep resolving, and a book that
// disappears from the server is deliberately left alone on the device.
func MarkSourceBooksMissing(ctx context.Context, x Execer, ids []int64) error {
	for _, id := range ids {
		if _, err := x.ExecContext(ctx,
			`UPDATE source_books SET missing = 1 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("mark source book %d missing: %w", id, err)
		}
	}
	return nil
}

// ReplaceSourceBookFiles rewrites the format rows for one source book,
// preserving the EPUB probe results when the file has not changed.
func ReplaceSourceBookFiles(ctx context.Context, x Execer, sourceBookID int64, files []SourceBookFile) error {
	type probe struct {
		layout      string
		epubVersion string
		probedMtime int64
	}
	known := map[string]probe{}

	rows, err := x.QueryContext(ctx,
		`SELECT format, layout, epub_version, probed_mtime FROM source_book_files WHERE source_book_id = ?`,
		sourceBookID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var f string
		var p probe
		if err := rows.Scan(&f, &p.layout, &p.epubVersion, &p.probedMtime); err != nil {
			rows.Close()
			return err
		}
		known[f] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := x.ExecContext(ctx,
		`DELETE FROM source_book_files WHERE source_book_id = ?`, sourceBookID); err != nil {
		return err
	}

	for _, f := range files {
		// Carry the probe forward only while the file itself is unchanged;
		// re-probing every scan would open every EPUB in the library.
		if p, ok := known[f.Format]; ok && p.probedMtime != 0 && p.probedMtime == f.FileMtime {
			f.Layout, f.EPUBVersion, f.ProbedMtime = p.layout, p.epubVersion, p.probedMtime
		}
		_, err := x.ExecContext(ctx, `
			INSERT INTO source_book_files
				(source_book_id, format, rel_path, size, file_mtime, layout, epub_version, probed_mtime, present)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			sourceBookID, f.Format, f.RelPath, f.Size, f.FileMtime,
			f.Layout, f.EPUBVersion, f.ProbedMtime, f.Present)
		if err != nil {
			return fmt.Errorf("insert file %s for source book %d: %w", f.Format, sourceBookID, err)
		}
	}
	return nil
}

// SetFileProbe records the result of an EPUB layout probe.
func SetFileProbe(ctx context.Context, x Execer, sourceBookID int64, format, layout, version string, mtime int64) error {
	_, err := x.ExecContext(ctx,
		`UPDATE source_book_files SET layout = ?, epub_version = ?, probed_mtime = ?
		 WHERE source_book_id = ? AND format = ?`,
		layout, version, mtime, sourceBookID, format)
	return err
}

// Candidate is one source row competing to represent a canonical book, already
// ordered by the winner rules.
type Candidate struct {
	SourceBook SourceBook
	SourceID   int64
	Priority   int
	Files      []SourceBookFile
}

// Candidates returns the live source rows for a canonical book, best first.
//
// Ordering: a row with a readable EPUB on disk beats one without (a book we
// cannot actually serve must not win), then source priority, then source id and
// Calibre id so the result is deterministic.
func Candidates(ctx context.Context, q Querier, bookID string) ([]Candidate, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT sb.id, sb.source_id, sb.calibre_id, sb.calibre_uuid, sb.title, sb.sort_title,
		       sb.authors_json, sb.author_sort, sb.series_name, sb.series_index,
		       sb.description_html, sb.publisher, sb.published_at, sb.language, sb.isbn13,
		       sb.identifiers_json, sb.tags_json, sb.rel_path, sb.cover_rel_path,
		       sb.cover_mtime, sb.calibre_last_modified, sb.meta_hash, s.priority
		FROM source_books sb
		JOIN sources s ON s.id = sb.source_id
		WHERE sb.book_id = ? AND sb.missing = 0 AND s.enabled = 1
		ORDER BY
			(EXISTS (SELECT 1 FROM source_book_files f
			         WHERE f.source_book_id = sb.id AND f.format = 'EPUB' AND f.present = 1)) DESC,
			s.priority ASC, s.id ASC, sb.calibre_id ASC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		sb := &c.SourceBook
		if err := rows.Scan(&sb.ID, &sb.SourceID, &sb.CalibreID, &sb.CalibreUUID, &sb.Title,
			&sb.SortTitle, &sb.AuthorsJSON, &sb.AuthorSort, &sb.SeriesName, &sb.SeriesIndex,
			&sb.DescriptionHTML, &sb.Publisher, &sb.PublishedAt, &sb.Language, &sb.ISBN13,
			&sb.IdentifiersJSON, &sb.TagsJSON, &sb.RelPath, &sb.CoverRelPath, &sb.CoverMtime,
			&sb.CalibreLastModified, &sb.MetaHash, &c.Priority); err != nil {
			return nil, err
		}
		sb.BookID = bookID
		c.SourceID = sb.SourceID
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		files, err := sourceBookFiles(ctx, q, out[i].SourceBook.ID)
		if err != nil {
			return nil, err
		}
		out[i].Files = files
	}
	return out, nil
}

func sourceBookFiles(ctx context.Context, q Querier, sourceBookID int64) ([]SourceBookFile, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, source_book_id, format, rel_path, size, file_mtime, layout,
		        epub_version, probed_mtime, present
		 FROM source_book_files WHERE source_book_id = ? ORDER BY format`, sourceBookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SourceBookFile
	for rows.Next() {
		var f SourceBookFile
		if err := rows.Scan(&f.ID, &f.SourceBookID, &f.Format, &f.RelPath, &f.Size,
			&f.FileMtime, &f.Layout, &f.EPUBVersion, &f.ProbedMtime, &f.Present); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// BooksTouchedBySource lists the canonical books a source contributes to, which
// is the set that must be re-resolved after that source is scanned or disabled.
func BooksTouchedBySource(ctx context.Context, q Querier, sourceID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT book_id FROM source_books WHERE source_id = ? AND book_id IS NOT NULL`,
		sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var _ = sql.ErrNoRows
