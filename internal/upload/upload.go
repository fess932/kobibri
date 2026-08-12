// Package upload takes books straight from a person rather than from a Calibre
// library.
//
// Everything uploaded lands in one source of its own, which outranks every
// Calibre library. That is the point of it: when the same book exists in both,
// the copy someone chose to put here by hand is the one that should reach a
// reader.
package upload

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/reader"
	"github.com/fess932/kobibri/internal/store"
	"github.com/google/uuid"
)

// SourceKind marks the one source these books belong to.
const SourceKind = store.SourceKindUpload

// SourceName is what it is called in the interface. It is created on the first
// upload and never by hand.
const SourceName = "Uploaded by hand"

// Priority puts this source above every Calibre library. Lower wins, and 0 is
// as low as it goes.
const Priority = 0

// MaxSize bounds one file. Well past any sane book, short of filling a disk.
const MaxSize = 512 << 20

var (
	ErrUnsupported = errors.New("this kind of file cannot be sent to a Kobo")
	ErrTooLarge    = errors.New("this file is too large")
	ErrEmpty       = errors.New("this file is empty")
)

// Accepted lists what may be uploaded: what a Kobo reads directly, plus what
// Calibre's converter can turn into an EPUB. PDF and comics are refused on
// purpose — Kobo does not sync them.
var Accepted = append([]string{"EPUB", "KEPUB"}, ebookconv.Convertible...)

type Store struct {
	store *store.Store
	dir   string
}

func New(st *store.Store, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("uploads directory: %w", err)
	}
	return &Store{store: st, dir: dir}, nil
}

func (u *Store) Dir() string { return u.dir }

// Add stores one file and files it as a book.
//
// The reader is copied to disk before anything is written to the database, so a
// transfer that dies halfway leaves a stray file rather than a book pointing at
// nothing.
func (u *Store) Add(ctx context.Context, filename string, r io.Reader) (bookID string, err error) {
	format, err := formatOf(filename)
	if err != nil {
		return "", err
	}

	dir := filepath.Join(u.dir, uuid.NewString())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Anything written before an error is removed: a half-copied book is worse
	// than no book.
	defer func() {
		if err != nil {
			os.RemoveAll(dir)
		}
	}()

	path := filepath.Join(dir, safeFilename(filename, format))
	size, err := copyTo(path, r)
	if err != nil {
		return "", err
	}
	if size == 0 {
		return "", ErrEmpty
	}

	return u.record(ctx, path, format, size)
}

// Remove takes an uploaded book away.
//
// The file goes and the source row is marked missing, exactly as if a Calibre
// library had lost it. The canonical book stays — its id is what every reader
// holds, and reissuing one would make the book arrive again as a stranger.
func (u *Store) Remove(ctx context.Context, sourceBookID int64) error {
	var libraryPath, relPath string
	err := u.store.Reader().QueryRowContext(ctx, `
		SELECT s.library_path, sb.rel_path
		FROM source_books sb JOIN sources s ON s.id = sb.source_id
		WHERE sb.id = ? AND s.kind = ?`, sourceBookID, SourceKind).
		Scan(&libraryPath, &relPath)
	if err != nil {
		return err
	}

	var bookID sql.NullString
	err = u.store.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT book_id FROM source_books WHERE id = ?`, sourceBookID).Scan(&bookID); err != nil {
			return err
		}
		if err := store.MarkSourceBooksMissing(ctx, tx, []int64{sourceBookID}); err != nil {
			return err
		}
		if !bookID.Valid || bookID.String == "" {
			return nil
		}
		resolved, err := store.ResolveBookID(ctx, tx, bookID.String)
		if err != nil {
			return err
		}
		return ingest.Resolve(ctx, tx, resolved)
	})
	if err != nil {
		return err
	}

	// Only once the database no longer points at it.
	if relPath != "" {
		os.RemoveAll(filepath.Join(libraryPath, filepath.FromSlash(relPath)))
	}
	return nil
}

// List returns what has been uploaded, newest first.
type Item struct {
	SourceBookID int64
	BookID       string
	Title        string
	Authors      string
	Format       string
	Size         int64
	AddedAt      string
	Missing      bool
	// Converted says whether a KEPUB is ready. Conversion starts the moment a
	// file lands, but it is worth being able to see that it finished.
	Converted bool
	// Syncable says whether the book will reach a reader at all — a format
	// nothing here can convert never will.
	Syncable bool
}

func (u *Store) List(ctx context.Context) ([]Item, error) {
	rows, err := u.store.Reader().QueryContext(ctx, `
		SELECT sb.id, COALESCE(sb.book_id, ''), sb.title, sb.authors_json,
		       COALESCE(f.format, ''), COALESCE(f.size, 0), sb.first_seen_at, sb.missing,
		       EXISTS (SELECT 1 FROM kepub_cache c WHERE c.book_id = sb.book_id),
		       COALESCE((SELECT b.syncable FROM books b WHERE b.id = sb.book_id), 0)
		FROM source_books sb
		JOIN sources s ON s.id = sb.source_id
		LEFT JOIN source_book_files f ON f.source_book_id = sb.id
		WHERE s.kind = ?
		ORDER BY sb.first_seen_at DESC, sb.id DESC`, SourceKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Item
	for rows.Next() {
		var it Item
		var authorsJSON string
		if err := rows.Scan(&it.SourceBookID, &it.BookID, &it.Title, &authorsJSON,
			&it.Format, &it.Size, &it.AddedAt, &it.Missing,
			&it.Converted, &it.Syncable); err != nil {
			return nil, err
		}
		var authors []string
		json.Unmarshal([]byte(authorsJSON), &authors)
		it.Authors = strings.Join(authors, ", ")
		out = append(out, it)
	}
	return out, rows.Err()
}

// record files a stored file as a source book and attaches it to a canonical
// book, merging with a Calibre copy of the same book when there is one.
func (u *Store) record(ctx context.Context, path, format string, size int64) (string, error) {
	sourceID, err := u.source(ctx)
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(u.dir, path)
	if err != nil {
		return "", err
	}

	sb := &store.SourceBook{
		SourceID: sourceID,
		// Uploads have no Calibre id. A hash of the stored path gives each one a
		// distinct slot in the (source, calibre_id) key, and re-uploading the
		// same book makes a new one rather than overwriting the old.
		CalibreID:       idFor(rel),
		IdentifiersJSON: `{}`,
		TagsJSON:        `[]`,
		RelPath:         filepath.ToSlash(filepath.Dir(rel)),
	}
	layout := ""

	// An EPUB says what it is. A file exported from Calibre carries that
	// library's own uuid, so it merges with the library's copy instead of
	// arriving as a second book.
	if meta, err := reader.Metadata(path); err == nil && meta.Title != "" {
		authors, _ := json.Marshal(meta.Authors)
		sb.Title = meta.Title
		sb.SortTitle = meta.Title
		sb.AuthorsJSON = string(authors)
		sb.AuthorSort = sortName(meta.Authors)
		sb.CalibreUUID = meta.UUID
		sb.ISBN13 = ingest.NormalizeISBN(meta.ISBN)
		sb.DescriptionHTML = meta.Description
		sb.Publisher = meta.Publisher
		sb.Language = meta.Language
		sb.SeriesName = meta.Series
		if meta.SeriesIndex > 0 {
			sb.SeriesIndex = sql.NullFloat64{Float64: meta.SeriesIndex, Valid: true}
		}
	} else {
		// Anything else has only its filename to go on. "Title - Author.fb2" is
		// the shape almost every downloaded book arrives in.
		title, author := splitFilename(filepath.Base(path))
		sb.Title = title
		sb.SortTitle = title
		if author != "" {
			authors, _ := json.Marshal([]string{author})
			sb.AuthorsJSON = string(authors)
			sb.AuthorSort = sortName([]string{author})
		} else {
			sb.AuthorsJSON = `[]`
		}
	}

	if format == "EPUB" || format == store.FormatKEPUB {
		if info, err := ingest.Probe(path); err == nil {
			layout = info.Layout
		}
	}

	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	// An uploaded book carries its cover inside itself; written out beside it,
	// everything downstream resolves it exactly as it does a Calibre one.
	sb.CoverRelPath, sb.CoverMtime = store.ExtractCover(u.dir, path)
	sb.MetaHash = fmt.Sprintf("upload|%s|%d", rel, size)

	var bookID string
	err = u.store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.UpsertSourceBook(ctx, tx, sb); err != nil {
			return err
		}
		files := []store.SourceBookFile{{
			Format:      format,
			RelPath:     filepath.ToSlash(rel),
			Size:        size,
			FileMtime:   fi.ModTime().UnixNano(),
			Present:     true,
			Layout:      layout,
			ProbedMtime: fi.ModTime().UnixNano(),
		}}
		if err := store.ReplaceSourceBookFiles(ctx, tx, sb.ID, files); err != nil {
			return err
		}

		if bookID, err = ingest.Attach(ctx, tx, sb); err != nil {
			return err
		}
		if err := store.SetSourceBookBookID(ctx, tx, sb.ID, bookID); err != nil {
			return err
		}

		resolved, err := store.ResolveBookID(ctx, tx, bookID)
		if err != nil {
			return err
		}
		return ingest.Resolve(ctx, tx, resolved)
	})
	return bookID, err
}

// source finds the single upload source, creating it on the first upload.
func (u *Store) source(ctx context.Context) (int64, error) {
	var id int64
	err := u.store.Reader().QueryRowContext(ctx,
		`SELECT id FROM sources WHERE kind = ? LIMIT 1`, SourceKind).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	src := &store.Source{
		Name: SourceName, LibraryPath: u.dir,
		Enabled: true, ShareAll: true, ScanIntervalSec: 24 * 3600,
	}
	if id, err = store.CreateSource(ctx, u.store.Writer(), src); err != nil {
		return 0, err
	}
	// The priority is set here rather than passed in: CreateSource reads 0 as
	// "not given" and substitutes its default, and 0 is exactly the value this
	// source needs. The form for a Calibre library will not go below 1, so
	// nothing can tie with it.
	if _, err := u.store.Writer().ExecContext(ctx,
		`UPDATE sources SET kind = ?, priority = ?, last_status = ? WHERE id = ?`,
		SourceKind, Priority, store.SourceStatusOK, id); err != nil {
		return 0, err
	}
	return id, nil
}

// formatOf names the format from the extension, and refuses anything a Kobo
// could not be given.
func formatOf(filename string) (string, error) {
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".kepub.epub") {
		return store.FormatKEPUB, nil
	}

	ext := strings.ToUpper(strings.TrimPrefix(filepath.Ext(filename), "."))
	for _, ok := range Accepted {
		if ext == ok {
			return ext, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrUnsupported, ext)
}

// copyTo writes the upload out, refusing anything absurd rather than filling the
// disk to find out.
func copyTo(path string, r io.Reader) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	size, err := io.Copy(f, io.LimitReader(r, MaxSize+1))
	if err != nil {
		return 0, err
	}
	if size > MaxSize {
		return 0, ErrTooLarge
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return size, nil
}

// splitFilename reads "Title - Author.fb2", which is the shape almost every
// downloaded book arrives in.
func splitFilename(name string) (title, author string) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	stem = strings.TrimSuffix(stem, ".kepub")
	stem = strings.Join(strings.Fields(strings.ReplaceAll(stem, "_", " ")), " ")

	if before, after, found := strings.Cut(stem, " - "); found {
		before, after = strings.TrimSpace(before), strings.TrimSpace(after)
		if before != "" && after != "" {
			return before, after
		}
	}
	return stem, ""
}

// safeFilename keeps the name recognisable on disk without letting it name
// somewhere else.
func safeFilename(name, format string) string {
	name = filepath.Base(filepath.FromSlash(name))

	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(strings.TrimSpace(b.String()), ".")
	if len(out) > 120 {
		out = strings.TrimSpace(out[:120]) + strings.ToLower(filepath.Ext(name))
	}
	if out == "" {
		out = "book." + strings.ToLower(format)
	}
	return out
}

// sortName turns "Jane Author" into "Author, Jane".
//
// This is not cosmetic. Identity compares the sort form, and Calibre stores one;
// a display name normalises to the same words in the other order and would never
// match, so the same book uploaded next to a Calibre copy would arrive as a
// second book.
func sortName(authors []string) string {
	if len(authors) == 0 {
		return ""
	}
	name := strings.TrimSpace(authors[0])
	if name == "" || strings.Contains(name, ",") {
		return name // already a sort form
	}

	fields := strings.Fields(name)
	if len(fields) < 2 {
		return name
	}
	last := fields[len(fields)-1]
	return last + ", " + strings.Join(fields[:len(fields)-1], " ")
}

// idFor derives a stable positive number from a string, for the slot an upload
// occupies where a Calibre book would have its id.
func idFor(s string) int64 {
	sum := sha256.Sum256([]byte(s))
	n := int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
	if n == 0 {
		n = 1
	}
	return n
}
