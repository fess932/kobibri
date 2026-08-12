package upload_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
	"github.com/fess932/kobibri/internal/upload"
)

type env struct {
	store   *store.Store
	uploads *upload.Store
	scanner *ingest.Scanner
	dir     string
	ctx     context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	up, err := upload.New(st, filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	return &env{store: st, uploads: up,
		scanner: ingest.NewScanner(st, filepath.Join(dir, "tmp")), dir: dir, ctx: ctx}
}

// epub writes a minimal but real EPUB, optionally carrying a Calibre uuid — the
// mark a file exported from a library keeps.
func epub(t *testing.T, title, author, uuid string) []byte {
	t.Helper()

	identifier := ""
	if uuid != "" {
		identifier = `<dc:identifier opf:scheme="uuid">` + uuid + `</dc:identifier>`
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"content.opf": `<package xmlns:dc="http://purl.org/dc/elements/1.1/"
			  xmlns:opf="http://www.idpf.org/2007/opf" version="2.0">
			  <metadata>
			    <dc:title>` + title + `</dc:title>
			    <dc:creator opf:role="aut">` + author + `</dc:creator>
			    <dc:language>en</dc:language>
			    ` + identifier + `
			  </metadata>
			  <manifest><item id="c1" href="one.xhtml" media-type="application/xhtml+xml"/></manifest>
			  <spine><itemref idref="c1"/></spine>
			</package>`,
		"one.xhtml": `<html><body><p>Words.</p></body></html>`,
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (e *env) add(t *testing.T, name string, body []byte) string {
	t.Helper()
	id, err := e.uploads.Add(e.ctx, name, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Add(%s): %v", name, err)
	}
	return id
}

func (e *env) book(t *testing.T, id string) *store.Book {
	t.Helper()
	b, err := store.GetBook(e.ctx, e.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An uploaded EPUB has to become an ordinary book: metadata from the file, a
// canonical id, ready to sync.
func TestAnUploadedBookIsAnOrdinaryBook(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "Whatever.epub", epub(t, "The Uploaded One", "Jane Author", ""))

	book := e.book(t, id)
	if book.Title != "The Uploaded One" {
		t.Errorf("Title = %q — the metadata was not read from the file", book.Title)
	}
	// The sort form, not the display name: that is what identity compares, and
	// what Calibre stores.
	if book.AuthorSort != "Author, Jane" {
		t.Errorf("AuthorSort = %q, want the sort form", book.AuthorSort)
	}
	if !book.Syncable {
		t.Error("an uploaded EPUB is not offered to any device")
	}
	if book.DownloadFormat != store.FormatKEPUB {
		t.Errorf("DownloadFormat = %q, want KEPUB", book.DownloadFormat)
	}
}

// The whole point: when the same book is in a Calibre library too, the copy
// someone put here by hand is the one that reaches a reader.
func TestAnUploadOutranksACalibreLibrary(t *testing.T) {
	e := newEnv(t)

	lib := calibretest.New(t, calibretest.BookSpec{
		Title: "Shared Book", Authors: []string{"Jane Author"},
	})
	sourceID, err := store.CreateSource(e.ctx, e.store.Writer(), &store.Source{
		Name: "main", LibraryPath: lib.Path, Priority: 1, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.scanner.Scan(e.ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatal(err)
	}

	id := e.add(t, "Shared Book.epub", epub(t, "Shared Book", "Jane Author", ""))

	// One book, not two: the upload merged with the library's copy.
	var books int
	e.store.Reader().QueryRowContext(e.ctx,
		`SELECT count(*) FROM books WHERE merged_into IS NULL`).Scan(&books)
	if books != 1 {
		t.Fatalf("%d canonical books, want 1 — the upload did not merge with the library's copy", books)
	}

	book := e.book(t, id)
	var winner int64
	e.store.Reader().QueryRowContext(e.ctx,
		`SELECT source_id FROM source_books WHERE id = ?`, book.PrimarySourceBookID.Int64).Scan(&winner)
	if winner == sourceID {
		t.Error("the Calibre library won; an uploaded copy is meant to outrank it")
	}
}

// A file exported from Calibre keeps the library's own uuid, which is a far
// stronger match than the title.
func TestACalibreUUIDMergesWithThatLibrary(t *testing.T) {
	e := newEnv(t)

	const uuid = "3f2e1d0c-1111-2222-3333-444455556666"
	lib := calibretest.New(t, calibretest.BookSpec{
		Title: "Exported", Authors: []string{"Someone Else"}, UUID: uuid,
	})
	sourceID, err := store.CreateSource(e.ctx, e.store.Writer(), &store.Source{
		Name: "main", LibraryPath: lib.Path, Priority: 1, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.scanner.Scan(e.ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatal(err)
	}

	// A different title on purpose: only the uuid can join these two.
	e.add(t, "renamed.epub", epub(t, "Quite A Different Title", "Nobody", uuid))

	var books int
	e.store.Reader().QueryRowContext(e.ctx,
		`SELECT count(*) FROM books WHERE merged_into IS NULL`).Scan(&books)
	if books != 1 {
		t.Errorf("%d canonical books, want 1 — the uuid in the file was not used", books)
	}
}

// A file a Kobo could never read must be refused at the door, not stored and
// then found wanting.
func TestUnreadableFormatsAreRefused(t *testing.T) {
	e := newEnv(t)

	for _, name := range []string{"scan.pdf", "comic.cbz", "notes.md", "book"} {
		if _, err := e.uploads.Add(e.ctx, name, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	entries, _ := os.ReadDir(e.uploads.Dir())
	if len(entries) != 0 {
		t.Errorf("%d directories left behind by refused uploads", len(entries))
	}
}

// An empty file is a failed transfer, not a book.
func TestAnEmptyFileIsRefused(t *testing.T) {
	e := newEnv(t)
	if _, err := e.uploads.Add(e.ctx, "empty.epub", bytes.NewReader(nil)); err == nil {
		t.Error("an empty file was accepted")
	}
	entries, _ := os.ReadDir(e.uploads.Dir())
	if len(entries) != 0 {
		t.Errorf("%d directories left behind", len(entries))
	}
}

// Removing takes the file away but keeps the canonical book: its id is what a
// reader holds, and reissuing one would make the book arrive again as a stranger.
func TestRemovingKeepsTheCanonicalBook(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "Gone Soon.epub", epub(t, "Gone Soon", "Jane Author", ""))

	items, err := e.uploads.List(e.ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("List: %v (%d items)", err, len(items))
	}
	if err := e.uploads.Remove(e.ctx, items[0].SourceBookID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	book := e.book(t, id)
	if book.Syncable {
		t.Error("a removed book is still offered to devices")
	}
	if book.Available {
		t.Error("a removed book still counts as available")
	}

	// And the file really is gone.
	entries, _ := os.ReadDir(e.uploads.Dir())
	if len(entries) != 0 {
		t.Errorf("%d directories left on disk after removal", len(entries))
	}
}

// Nothing in the uploads source may be scanned: it has no metadata.db, and a
// scan would find no books and mark every one of them as vanished.
func TestTheUploadSourceIsNeverScanned(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "Kept.epub", epub(t, "Kept", "Jane Author", ""))

	var sourceID int64
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM sources WHERE kind = ?`, store.SourceKindUpload).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}

	res, err := e.scanner.Scan(e.ctx, sourceID, ingest.ScanOptions{Force: true})
	if err != nil {
		t.Fatalf("scanning the uploads source failed instead of being skipped: %v", err)
	}
	if !res.Skipped {
		t.Error("the uploads source was scanned")
	}
	if !e.book(t, id).Syncable {
		t.Error("a scan of the uploads source lost the book")
	}
}

// A book with nothing but a filename to go on still has to arrive named.
func TestAFilenameIsUsedWhenTheFileSaysNothing(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "The Long Way - Becky Chambers.fb2", []byte("<FictionBook/>"))

	book := e.book(t, id)
	if book.Title != "The Long Way" {
		t.Errorf("Title = %q, want it read from the filename", book.Title)
	}
	if book.AuthorSort != "Chambers, Becky" {
		t.Errorf("AuthorSort = %q, want the sort form of the name in the filename", book.AuthorSort)
	}
	// Without a converter on this machine it cannot be delivered, so it must not
	// be offered either.
	if book.Syncable {
		t.Error("a book that cannot be converted here was offered to devices")
	}
}
