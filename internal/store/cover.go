package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fess932/kobibri/internal/reader"
)

// CoverName is what an extracted cover is stored as, beside the book. Calibre
// uses the same name for the same reason: everything downstream then resolves a
// cover the one way, whether the book came from a library, from a link, or from
// someone's hands.
const CoverName = "cover"

// ExtractCover pulls the cover out of an EPUB and writes it beside the book,
// returning the path relative to the library root.
//
// Books from Calibre have a cover on disk already. A book from the web or one
// uploaded by hand carries it inside the file and nowhere else, and without this
// they arrive on a reader as blank rectangles.
//
// A book with no cover is not an error: it returns an empty path.
func ExtractCover(libraryPath, bookPath string) (relPath string, mtime int64) {
	data, ext, err := reader.Cover(bookPath)
	if err != nil || len(data) == 0 {
		return "", 0
	}

	dst := filepath.Join(filepath.Dir(bookPath), CoverName+ext)
	// Written to a temporary name first: a half-written cover that is picked up
	// by a device is cached by it more or less forever.
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", 0
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", 0
	}

	rel, err := filepath.Rel(libraryPath, dst)
	if err != nil {
		return "", 0
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return "", 0
	}
	return filepath.ToSlash(rel), fi.ModTime().Unix()
}

// CoverPath resolves a stored cover to a readable path, refusing anything that
// points outside the library.
func CoverPath(libraryPath, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("no cover")
	}
	abs := filepath.Join(libraryPath, filepath.FromSlash(relPath))
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// RecoverCoverFromEPUB takes a cover out of a converted EPUB for a book that has
// none.
//
// A book the library holds as FB2, AZW3 or MOBI carries its cover inside itself
// in a format only its own reader understands, so nothing can be extracted until
// it has been converted. The converted EPUB is where it becomes reachable.
//
// It refuses to touch a Calibre library. Those keep a cover.jpg beside the book
// already, and writing into someone's library is the one thing this server never
// does.
func RecoverCoverFromEPUB(ctx context.Context, x Execer, bookID, epubPath string) (bool, error) {
	var sourceBookID int64
	var libraryPath, relPath, kind, coverRelPath string
	err := x.QueryRowContext(ctx, `
		SELECT sb.id, s.library_path, sb.rel_path, s.kind, sb.cover_rel_path
		FROM books b
		JOIN source_books sb ON sb.id = b.primary_source_book_id
		JOIN sources s ON s.id = sb.source_id
		WHERE b.id = ?`, bookID).
		Scan(&sourceBookID, &libraryPath, &relPath, &kind, &coverRelPath)
	if err != nil {
		return false, err
	}
	if kind == SourceKindCalibre || coverRelPath != "" {
		return false, nil
	}

	// Beside the book, which is where every other cover lives.
	dir := filepath.Join(libraryPath, filepath.FromSlash(relPath))
	data, ext, err := reader.Cover(epubPath)
	if err != nil || len(data) == 0 {
		return false, nil
	}

	dst := filepath.Join(dir, CoverName+ext)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return false, err
	}

	rel, err := filepath.Rel(libraryPath, dst)
	if err != nil {
		return false, err
	}
	fi, err := os.Stat(dst)
	if err != nil {
		return false, err
	}

	_, err = x.ExecContext(ctx,
		`UPDATE source_books SET cover_rel_path = ?, cover_mtime = ? WHERE id = ?`,
		filepath.ToSlash(rel), fi.ModTime().Unix(), sourceBookID)
	return err == nil, err
}
