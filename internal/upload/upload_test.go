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
// mark a file exported from a library keeps. It always has a cover, because a
// book without one is the exception rather than the rule.
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
			  <manifest>
			    <item id="c1" href="one.xhtml" media-type="application/xhtml+xml"/>
			    <item id="cover" href="images/cover.png" media-type="image/png"
			          properties="cover-image"/>
			  </manifest>
			  <spine><itemref idref="c1"/></spine>
			</package>`,
		"one.xhtml":        `<html><body><p>Words.</p></body></html>`,
		"images/cover.png": string(pngPixel()),
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

// pngPixel is the smallest valid PNG, so a fixture can carry a real image.
func pngPixel() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0d, 0x0a, 0x2d, 0xb4,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
}

// An uploaded book carries its cover inside itself. Without pulling it out, the
// book arrives on a reader as a blank rectangle.
func TestAnUploadKeepsItsCover(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "Pretty.epub", epub(t, "Pretty", "Jane Author", ""))

	book := e.book(t, id)
	if book.CoverImageID == "" {
		t.Fatal("the uploaded book has no cover")
	}

	// And the file is really there, beside the book.
	var libraryPath, relPath string
	if err := e.store.Reader().QueryRowContext(e.ctx, `
		SELECT s.library_path, sb.cover_rel_path
		FROM source_books sb JOIN sources s ON s.id = sb.source_id
		WHERE sb.id = ?`, book.CoverSourceBookID.Int64).Scan(&libraryPath, &relPath); err != nil {
		t.Fatal(err)
	}
	if relPath == "" {
		t.Fatal("no cover path was recorded")
	}
	path, err := store.CoverPath(libraryPath, relPath)
	if err != nil {
		t.Fatalf("the recorded cover is not on disk: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("the cover file is empty: %v", err)
	}
}

// A book with no cover at all must still import, just without one.
func TestABookWithoutACoverStillImports(t *testing.T) {
	e := newEnv(t)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf"/></rootfiles></container>`,
		"content.opf": `<package><metadata><title>Plain</title></metadata>
			  <manifest><item id="c1" href="one.xhtml" media-type="application/xhtml+xml"/></manifest>
			  <spine><itemref idref="c1"/></spine></package>`,
		"one.xhtml": `<html><body><p>Words.</p></body></html>`,
	} {
		w, _ := zw.Create(name)
		io.WriteString(w, body)
	}
	zw.Close()

	id := e.add(t, "Plain.epub", buf.Bytes())
	if book := e.book(t, id); book.CoverImageID != "" {
		t.Errorf("a book with no cover got one: %q", book.CoverImageID)
	}
}

// Books filed before covers were read out of the file at all have none, and
// nothing else would give them one: a scan does not touch these sources, and a
// re-import would download the whole book again.
func TestCoversAreRecoveredForBooksThatHaveNone(t *testing.T) {
	e := newEnv(t)
	id := e.add(t, "Pretty.epub", epub(t, "Pretty", "Jane Author", ""))

	// Put it back the way an older version left it: the file is there, the
	// record of its cover is not.
	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE source_books SET cover_rel_path = '', cover_mtime = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET cover_image_id = '', cover_source_book_id = NULL`); err != nil {
		t.Fatal(err)
	}
	if e.book(t, id).CoverImageID != "" {
		t.Fatal("the test did not manage to take the cover away")
	}

	found, err := ingest.BackfillCovers(e.ctx, e.store)
	if err != nil {
		t.Fatalf("BackfillCovers: %v", err)
	}
	if found != 1 {
		t.Fatalf("recovered %d covers, want 1", found)
	}
	if e.book(t, id).CoverImageID == "" {
		t.Error("the book still has no cover")
	}

	// It runs once: a book that genuinely has none must not be reopened on every
	// start for the rest of its life.
	again, err := ingest.BackfillCovers(e.ctx, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("the pass ran a second time and touched %d books", again)
	}
}

// A Calibre library keeps its covers beside the books, and this must not go
// rummaging through them.
func TestTheBackfillLeavesCalibreAlone(t *testing.T) {
	e := newEnv(t)

	lib := calibretest.New(t, calibretest.BookSpec{Title: "From Calibre"})
	sourceID, err := store.CreateSource(e.ctx, e.store.Writer(), &store.Source{
		Name: "main", LibraryPath: lib.Path, Priority: 1, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.scanner.Scan(e.ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatal(err)
	}

	found, err := ingest.BackfillCovers(e.ctx, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Errorf("the pass took %d covers out of a Calibre library's books", found)
	}
}
