package ebookconv_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// fakeConvertBin writes a script that stands in for Calibre's ebook-convert.
// Calibre is not installed here, and the thing under test is the plumbing
// around the converter rather than the conversion itself.
func fakeConvertBin(t *testing.T, behaviour string) string {
	t.Helper()

	var body string
	switch behaviour {
	case "ok":
		// Copy the input to the output, so the result is a real, readable file.
		body = "#!/bin/sh\ncp \"$1\" \"$2\"\n"
	case "fail":
		body = "#!/bin/sh\necho 'Traceback: no plugin can handle this' >&2\nexit 1\n"
	default:
		t.Fatalf("unknown behaviour %q", behaviour)
	}

	path := filepath.Join(t.TempDir(), "ebook-convert")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type env struct {
	store   *store.Store
	cache   *ebookconv.Cache
	scanner *ingest.Scanner
	lib     *calibretest.Library
	ctx     context.Context
}

func newEnv(t *testing.T, bin string, books ...calibretest.BookSpec) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cache, err := ebookconv.New(ebookconv.Options{
		Dir: filepath.Join(dir, "epub"), Store: st, Bin: bin,
	})
	if err != nil {
		t.Fatal(err)
	}
	ingest.SetConverter(cache.BestFor)
	t.Cleanup(func() { ingest.SetConverter(nil) })

	lib := calibretest.New(t, books...)
	scanner := ingest.NewScanner(st, filepath.Join(dir, "tmp"))

	sourceID, err := store.CreateSource(ctx, st.Writer(), &store.Source{
		Name: "main", LibraryPath: lib.Path, Priority: 100, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	return &env{store: st, cache: cache, scanner: scanner, lib: lib, ctx: ctx}
}

func (e *env) book(t *testing.T, title string) *store.Book {
	t.Helper()
	var id string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM books WHERE title = ? AND merged_into IS NULL`, title).Scan(&id); err != nil {
		t.Fatalf("no book %q: %v", title, err)
	}
	book, err := store.GetBook(e.ctx, e.store.Reader(), id)
	if err != nil {
		t.Fatal(err)
	}
	return book
}

// A book Calibre holds only as AZW3 should reach a reader, given a converter.
func TestBookInAnotherFormatBecomesSyncable(t *testing.T) {
	e := newEnv(t, fakeConvertBin(t, "ok"),
		calibretest.BookSpec{Title: "Kindle Only",
			Formats: []calibretest.FormatSpec{{Format: "AZW3"}}},
	)

	book := e.book(t, "Kindle Only")
	if !book.Syncable {
		t.Fatal("a book with a convertible format is not syncable, though a converter is available")
	}
	if book.DownloadFormat != store.FormatKEPUB {
		t.Errorf("DownloadFormat = %q, want KEPUB", book.DownloadFormat)
	}
	if book.ConvertFrom != "AZW3" {
		t.Errorf("ConvertFrom = %q, want AZW3", book.ConvertFrom)
	}

	path, err := e.cache.EPUBFor(e.ctx, book)
	if err != nil {
		t.Fatalf("EPUBFor: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("converted file missing or empty: %v", err)
	}
}

// Without a converter the same book must not be offered at all: advertising a
// book and then failing its download is worse than never offering it.
func TestWithoutAConverterSuchBooksAreNotOffered(t *testing.T) {
	e := newEnv(t, "/nonexistent/ebook-convert-missing",
		calibretest.BookSpec{Title: "Kindle Only",
			Formats: []calibretest.FormatSpec{{Format: "AZW3"}}},
	)

	if e.cache.HasCalibre() {
		t.Fatal("a missing binary was reported as usable")
	}
	if got := e.cache.BestFor([]string{"AZW3"}); got != "" {
		t.Errorf("BestFor(AZW3) = %q with no Calibre; nothing here can do it", got)
	}
	book := e.book(t, "Kindle Only")
	if book.Syncable {
		t.Error("a book was offered although nothing here can convert it")
	}
	if book.DownloadFormat != "" {
		t.Errorf("DownloadFormat = %q, want empty", book.DownloadFormat)
	}
}

// A real EPUB is served as it is; nothing is converted for it.
func TestEPUBIsNotConverted(t *testing.T) {
	e := newEnv(t, fakeConvertBin(t, "ok"),
		calibretest.BookSpec{Title: "Already Epub"},
	)

	book := e.book(t, "Already Epub")
	if book.ConvertFrom != "" {
		t.Errorf("ConvertFrom = %q for a book that is already EPUB", book.ConvertFrom)
	}

	path, err := e.cache.EPUBFor(e.ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	if !isUnderLibrary(path, e.lib.Path) {
		t.Errorf("an existing EPUB was copied into the cache instead of served from the library: %s", path)
	}

	var cached int
	_ = e.store.Reader().QueryRowContext(e.ctx, `SELECT count(*) FROM epub_cache`).Scan(&cached)
	if cached != 0 {
		t.Errorf("%d cache entries for a book that needed no conversion", cached)
	}
}

// PDF is out of scope: Kobo does not sync it, and converting a scan produces
// something nobody wants to read.
func TestPDFIsNeverConverted(t *testing.T) {
	e := newEnv(t, fakeConvertBin(t, "ok"),
		calibretest.BookSpec{Title: "Scan Only",
			Formats: []calibretest.FormatSpec{{Format: "PDF"}}},
	)

	book := e.book(t, "Scan Only")
	if book.Syncable || book.ConvertFrom != "" {
		t.Errorf("a PDF-only book was queued for conversion: syncable=%v from=%q",
			book.Syncable, book.ConvertFrom)
	}
}

// A conversion that fails is remembered, so it is not retried on every request.
func TestFailedConversionIsRemembered(t *testing.T) {
	e := newEnv(t, fakeConvertBin(t, "fail"),
		calibretest.BookSpec{Title: "Awkward",
			Formats: []calibretest.FormatSpec{{Format: "AZW3"}}},
	)

	book := e.book(t, "Awkward")
	if _, err := e.cache.EPUBFor(e.ctx, book); err == nil {
		t.Fatal("a failing conversion reported success")
	}

	var failures int
	_ = e.store.Reader().QueryRowContext(e.ctx, `SELECT count(*) FROM epub_failures`).Scan(&failures)
	if failures != 1 {
		t.Errorf("%d recorded failures, want 1", failures)
	}

	// And nothing half-written was left behind to look like a cached result.
	var cached int
	_ = e.store.Reader().QueryRowContext(e.ctx, `SELECT count(*) FROM epub_cache`).Scan(&cached)
	if cached != 0 {
		t.Errorf("%d cache entries after a failed conversion", cached)
	}
}

// Converting twice reuses the result.
func TestConversionIsCached(t *testing.T) {
	e := newEnv(t, fakeConvertBin(t, "ok"),
		calibretest.BookSpec{Title: "Kindle Only",
			Formats: []calibretest.FormatSpec{{Format: "AZW3"}}},
	)

	book := e.book(t, "Kindle Only")
	first, err := e.cache.EPUBFor(e.ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := e.cache.EPUBFor(e.ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("second call returned a different path: %q vs %q", second, first)
	}
	after, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Error("the file was rewritten; the cache did not hit")
	}
}

// The best available format is picked, not just any of them.
func TestBestConvertible(t *testing.T) {
	tests := []struct {
		have []string
		want string
	}{
		{[]string{"MOBI", "AZW3"}, "AZW3"},
		{[]string{"TXT", "FB2"}, "FB2"},
		{[]string{"mobi"}, "MOBI"},
		{[]string{"PDF", "CBZ"}, ""},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := ebookconv.BestConvertible(tt.have); got != tt.want {
			t.Errorf("BestConvertible(%v) = %q, want %q", tt.have, got, tt.want)
		}
	}
}

func isUnderLibrary(path, library string) bool {
	rel, err := filepath.Rel(library, path)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." &&
		!hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// Calibre on macOS installs its converter inside the application bundle and puts
// nothing on PATH, so a machine with Calibre installed and working looked exactly
// like a machine without it — and every book in another format was silently never
// offered. The lookup has to know where Calibre actually lives.
func TestTheConverterIsFoundOutsideThePath(t *testing.T) {
	if len(ebookconv.ConverterLocations()) == 0 {
		t.Fatal("no known locations are searched at all")
	}

	var mac bool
	for _, path := range ebookconv.ConverterLocations() {
		if strings.Contains(path, "calibre.app/Contents/MacOS/ebook-convert") {
			mac = true
		}
	}
	if !mac {
		t.Error("the macOS application bundle is not among the places searched")
	}
}

// FB2 is the format these libraries are actually full of, and it must not need
// Calibre installed. A machine with nothing on it has to be able to put an FB2
// on a Kobo.
func TestFB2NeedsNothingInstalled(t *testing.T) {
	e := newEnv(t, "/nonexistent/ebook-convert-missing",
		calibretest.BookSpec{Title: "Russian Novel",
			Formats: []calibretest.FormatSpec{{Format: "FB2", Kind: "fb2"}}},
	)

	if e.cache.HasCalibre() {
		t.Fatal("this test is meaningless with Calibre available")
	}
	if got := e.cache.BestFor([]string{"FB2"}); got != "FB2" {
		t.Fatalf("BestFor(FB2) = %q with no Calibre, want FB2", got)
	}

	book := e.book(t, "Russian Novel")
	if !book.Syncable {
		t.Fatal("an FB2 book is not offered although nothing else is needed to convert it")
	}
	if book.ConvertFrom != "FB2" {
		t.Errorf("ConvertFrom = %q, want FB2", book.ConvertFrom)
	}

	path, err := e.cache.EPUBFor(e.ctx, book)
	if err != nil {
		t.Fatalf("EPUBFor: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("the converted file is missing or empty: %v", err)
	}
}

// And what the interface promises has to be what this machine can do. Listing a
// format nothing here converts is how someone uploads twelve files and gets
// twelve blank rows.
func TestOnlyWhatCanBeDoneIsPromised(t *testing.T) {
	withCalibre := newEnv(t, fakeConvertBin(t, "ok"))
	without := newEnv(t, "/nonexistent/ebook-convert-missing")

	if !contains(without.cache.Formats(), "FB2") {
		t.Error("FB2 is not promised even though it needs nothing")
	}
	if contains(without.cache.Formats(), "AZW3") {
		t.Error("AZW3 is promised with no Calibre to do it")
	}
	if !contains(withCalibre.cache.Formats(), "AZW3") {
		t.Error("AZW3 is not promised even with Calibre available")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
