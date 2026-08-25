package ingest

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"

	"github.com/fess932/kobibri/internal/store"
)

// backfillKey marks the one-off pass as done. It is a version rather than a
// flag: a later pass that learns to find something new can bump it and run
// again without touching what is already there.
const backfillKey = "covers:backfilled"

const backfillVersion = "1"

// BackfillCovers gives a cover to books that were filed before covers were taken
// out of the file at all.
//
// Only books that are not from Calibre: a Calibre library keeps its cover beside
// the book already, and anything from a link or uploaded by hand carries it
// inside the file and nowhere else. Those arrived as blank rectangles, and
// nothing else would ever fix them — a scan does not touch such a source, and a
// re-import would download the whole book again.
//
// It runs once. A book that genuinely has no cover would otherwise be opened on
// every start for the rest of its life.
func BackfillCovers(ctx context.Context, st *store.Store) (int, error) {
	if done, _ := store.GetKV(ctx, st.Reader(), backfillKey); done == backfillVersion {
		return 0, nil
	}

	type candidate struct {
		sourceBookID int64
		libraryPath  string
		bookPath     string
		bookID       string
	}

	rows, err := st.Reader().QueryContext(ctx, `
		SELECT sb.id, s.library_path, f.rel_path, COALESCE(sb.book_id, '')
		FROM source_books sb
		JOIN sources s ON s.id = sb.source_id
		JOIN source_book_files f ON f.source_book_id = sb.id AND f.present = 1
		WHERE s.kind <> ? AND sb.missing = 0 AND sb.cover_rel_path = ''
		  AND f.format IN ('EPUB', 'KEPUB')
		GROUP BY sb.id`, store.SourceKindCalibre)
	if err != nil {
		return 0, err
	}

	var todo []candidate
	for rows.Next() {
		var c candidate
		var rel string
		if err := rows.Scan(&c.sourceBookID, &c.libraryPath, &rel, &c.bookID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		c.bookPath = filepath.Join(c.libraryPath, filepath.FromSlash(rel))
		todo = append(todo, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var found int
	for _, c := range todo {
		relPath, mtime := store.ExtractCover(c.libraryPath, c.bookPath)
		if relPath == "" {
			continue
		}

		err := st.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`UPDATE source_books SET cover_rel_path = ?, cover_mtime = ? WHERE id = ?`,
				relPath, mtime, c.sourceBookID); err != nil {
				return err
			}
			if c.bookID == "" {
				return nil
			}
			// The canonical book has to be recomputed: its CoverImageId is built
			// from the cover's mtime, and until it moves no device asks again.
			resolved, err := store.ResolveBookID(ctx, tx, c.bookID)
			if err != nil {
				return err
			}
			return Resolve(ctx, tx, resolved)
		})
		if err != nil {
			slog.Warn("recording a recovered cover", "source_book", c.sourceBookID, "err", err)
			continue
		}
		found++
	}

	if err := store.SetKV(ctx, st.Writer(), backfillKey, backfillVersion); err != nil {
		return found, err
	}
	if found > 0 {
		slog.Info("recovered covers from books that had none", "books", found, "looked_at", len(todo))
	}
	return found, nil
}

// reresolveKey marks the one-off pass that reapplies the download-format rules
// after they changed. Like backfillKey it is a version, so a later change to
// the rules can bump it and sweep again.
const reresolveKey = "download:reresolved"

const reresolveVersion = "2"

// ReresolveEverything recomputes every canonical book once, after a change to
// how a book is derived rather than to anything in Calibre. A scan will not do
// it: nothing changed in the library, so the books never enter the changed set.
//
// Idempotent, and cheap to be wrong about: metadata_rev only moves where
// serving_hash actually changed. Version 1 was the KEPUB rule, version 2
// Calibre's undefined publication date.
func ReresolveEverything(ctx context.Context, st *store.Store) (int, error) {
	if done, _ := store.GetKV(ctx, st.Reader(), reresolveKey); done == reresolveVersion {
		return 0, nil
	}

	rows, err := st.Reader().QueryContext(ctx, `
		SELECT id FROM books WHERE merged_into IS NULL`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	err = st.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			resolved, err := store.ResolveBookID(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := Resolve(ctx, tx, resolved); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	if err := store.SetKV(ctx, st.Writer(), reresolveKey, reresolveVersion); err != nil {
		return len(ids), err
	}
	if len(ids) > 0 {
		slog.Info("re-resolved every book after a derivation change", "books", len(ids))
	}
	return len(ids), nil
}
