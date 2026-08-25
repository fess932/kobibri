package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fess932/kobibri/internal/store"
)

// Purging is the one operation that breaks the rule everything else here obeys.
//
// A canonical id is issued once and never withdrawn, because it is what every
// reader holds. Purging withdraws one. It exists because "delete it and let me
// import it again" is a thing an operator genuinely needs, and there is no way
// to grant it and keep the id: the same book imported afresh gets a new
// identity, and a reader that already had the old one will be handed the new
// book as a second copy.
//
// That is stated in the interface rather than hidden, and it is why this is a
// separate, explicit action instead of part of anything automatic.

// PurgeResult says what went.
type PurgeResult struct {
	Title string
	// Files is how many files were deleted from disk. Books in a Calibre
	// library are never among them.
	Files int
	// KeptInCalibre is true when the book still exists in a Calibre library, so
	// the next scan will bring it straight back.
	KeptInCalibre bool
}

// PurgeBook removes a book from the database and deletes the files this server
// owns.
//
// Files inside a Calibre library are left alone — writing there is the one thing
// this server never does — so purging such a book only forgets it until the next
// scan. The caller is told so.
func PurgeBook(ctx context.Context, st *store.Store, cacheDir, bookID string) (PurgeResult, error) {
	var res PurgeResult

	resolved, err := store.ResolveBookID(ctx, st.Reader(), bookID)
	if err != nil {
		return res, err
	}

	book, err := store.GetBook(ctx, st.Reader(), resolved)
	if err != nil {
		return res, err
	}
	res.Title = book.Title

	// Every id that resolves to this book: aliases from earlier merges are part
	// of what has to go, or a device holding a pre-merge id keeps resolving to a
	// book that is no longer there.
	ids, err := aliasIDs(ctx, st.Reader(), resolved)
	if err != nil {
		return res, err
	}

	dirs, kept, err := ownedPaths(ctx, st.Reader(), ids)
	if err != nil {
		return res, err
	}
	res.KeptInCalibre = kept

	// The database first: a file removed while a row still points at it is a
	// broken book, whereas a row removed while the file lingers is only litter.
	err = st.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if err := purgeRows(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("removing a purged book's files", "path", dir, "err", err)
			continue
		}
		res.Files++
	}
	res.Files += removeCachedFiles(cacheDir, ids)

	slog.Info("purged a book", "book", resolved, "title", res.Title,
		"ids", len(ids), "files", res.Files, "kept_in_calibre", res.KeptInCalibre)
	return res, nil
}

// aliasIDs collects the canonical id and every id merged into it.
func aliasIDs(ctx context.Context, q store.Querier, bookID string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id FROM books WHERE id = ? OR merged_into = ?`, bookID, bookID)
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

// ownedPaths lists the directories holding files this server may delete, and
// reports whether any contributor is a Calibre library.
func ownedPaths(ctx context.Context, q store.Querier, ids []string) ([]string, bool, error) {
	var dirs []string
	var kept bool

	for _, id := range ids {
		rows, err := q.QueryContext(ctx, `
			SELECT s.kind, s.library_path, sb.rel_path
			FROM source_books sb JOIN sources s ON s.id = sb.source_id
			WHERE sb.book_id = ?`, id)
		if err != nil {
			return nil, false, err
		}
		for rows.Next() {
			var kind, libraryPath, relPath string
			if err := rows.Scan(&kind, &libraryPath, &relPath); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			if kind == store.SourceKindCalibre {
				kept = true
				continue
			}
			if relPath == "" || relPath == "." {
				// Refusing rather than deleting a whole library directory.
				continue
			}
			dirs = append(dirs, filepath.Join(libraryPath, filepath.FromSlash(relPath)))
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
	}
	return dirs, kept, nil
}

// purgeRows deletes everything keyed on one book id.
//
// sync_point_books is included deliberately. Leaving it would make the next sync
// diff see a book that vanished from the snapshot and tell the device to archive
// it — and a purge is "forget this here", not "take it off the reader". What is
// already on a reader stays there.
func purgeRows(ctx context.Context, tx *sql.Tx, id string) error {
	statements := []string{
		`DELETE FROM source_book_columns WHERE source_book_id IN
			(SELECT id FROM source_books WHERE book_id = ?)`,
		`DELETE FROM source_book_files WHERE source_book_id IN
			(SELECT id FROM source_books WHERE book_id = ?)`,
		`DELETE FROM web_imports WHERE source_book_id IN
			(SELECT id FROM source_books WHERE book_id = ?)`,
		`DELETE FROM source_books WHERE book_id = ?`,
		`DELETE FROM book_identities WHERE book_id = ?`,
		`DELETE FROM reading_states WHERE book_id = ?`,
		`DELETE FROM tag_books WHERE book_id = ?`,
		`DELETE FROM device_tombstones WHERE book_id = ?`,
		`DELETE FROM sync_point_books WHERE book_id = ?`,
		`DELETE FROM kepub_cache WHERE book_id = ?`,
		`DELETE FROM kepub_failures WHERE book_id = ?`,
		`DELETE FROM epub_cache WHERE book_id = ?`,
		`DELETE FROM epub_failures WHERE book_id = ?`,
		`DELETE FROM book_series_overrides WHERE book_id = ?`,
		`DELETE FROM books WHERE id = ?`,
	}
	for _, q := range statements {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("purge %s: %w", id, err)
		}
	}
	return nil
}

// removeCachedFiles deletes converted files and scaled covers, which are named
// after the book and rebuildable from nothing.
func removeCachedFiles(cacheDir string, ids []string) int {
	if cacheDir == "" {
		return 0
	}

	var removed int
	for _, id := range ids {
		if len(id) < 2 {
			continue
		}
		shard := id[:2]
		for _, dir := range []string{"kepub", "epub"} {
			matches, _ := filepath.Glob(filepath.Join(cacheDir, dir, shard, id+".*"))
			for _, path := range matches {
				if os.Remove(path) == nil {
					removed++
				}
			}
		}
		for _, bucket := range []string{"small", "medium", "large"} {
			matches, _ := filepath.Glob(filepath.Join(cacheDir, "covers", bucket, "*", id+"*"))
			for _, path := range matches {
				if os.Remove(path) == nil {
					removed++
				}
			}
		}
	}
	return removed
}
