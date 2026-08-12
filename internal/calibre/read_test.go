package calibre_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/calibre/calibretest"
)

func open(t *testing.T, lib *calibretest.Library) *calibre.DB {
	t.Helper()
	db, err := calibre.Open(lib.Path, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestReadsFullRecord(t *testing.T) {
	ctx := context.Background()
	lastMod := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	lib := calibretest.New(t, calibretest.BookSpec{
		Title:        "The Long Book",
		Authors:      []string{"Jane Author", "Kim Second"},
		Series:       "The Series",
		SeriesIndex:  2.5,
		Description:  "<p>Blurb.</p>",
		Publisher:    "Some Press",
		Languages:    []string{"eng", "rus"},
		Tags:         []string{"scifi", "favourites"},
		Identifiers:  map[string]string{"isbn": "9780306406157", "goodreads": "12345"},
		LastModified: lastMod,
		Cover:        true,
		Formats:      []calibretest.FormatSpec{{Format: "EPUB"}},
	})

	db := open(t, lib)

	stubs, err := db.Stubs(ctx)
	if err != nil {
		t.Fatalf("Stubs: %v", err)
	}
	if len(stubs) != 1 {
		t.Fatalf("got %d stubs, want 1", len(stubs))
	}
	if !stubs[0].LastModified.Equal(lastMod) {
		t.Errorf("stub LastModified = %v, want %v", stubs[0].LastModified, lastMod)
	}
	if stubs[0].UUID == "" {
		t.Error("stub UUID is empty")
	}

	books, err := db.Books(ctx, []int64{stubs[0].ID})
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("got %d books, want 1", len(books))
	}
	b := books[0]

	if b.Title != "The Long Book" {
		t.Errorf("Title = %q", b.Title)
	}
	if got := b.AuthorNames(); len(got) != 2 || got[0] != "Jane Author" || got[1] != "Kim Second" {
		t.Errorf("Authors = %v, want link order [Jane Author, Kim Second]", got)
	}
	if b.PrimaryAuthorSort() != "Author, Jane" {
		t.Errorf("PrimaryAuthorSort = %q, want %q", b.PrimaryAuthorSort(), "Author, Jane")
	}
	if b.SeriesName != "The Series" || b.SeriesIndex != 2.5 || !b.HasSeries {
		t.Errorf("series = %q #%v (has=%v)", b.SeriesName, b.SeriesIndex, b.HasSeries)
	}
	if b.Description != "<p>Blurb.</p>" {
		t.Errorf("Description = %q", b.Description)
	}
	if b.Publisher != "Some Press" {
		t.Errorf("Publisher = %q", b.Publisher)
	}
	if len(b.Languages) != 2 || b.Languages[0] != "eng" || b.Languages[1] != "rus" {
		t.Errorf("Languages = %v, want item_order [eng rus]", b.Languages)
	}
	if len(b.Tags) != 2 {
		t.Errorf("Tags = %v", b.Tags)
	}
	if b.Identifiers["isbn"] != "9780306406157" || b.Identifiers["goodreads"] != "12345" {
		t.Errorf("Identifiers = %v", b.Identifiers)
	}
	if !b.LastModified.Equal(lastMod) {
		t.Errorf("LastModified = %v, want %v", b.LastModified, lastMod)
	}

	f, ok := b.Format("EPUB")
	if !ok {
		t.Fatalf("no EPUB format in %v", b.Formats)
	}
	if !f.Present {
		t.Error("EPUB is not marked present")
	}
	if f.Size == 0 {
		t.Error("EPUB size is 0; it should be the statted file size")
	}
	if abs := filepath.Join(lib.Path, filepath.FromSlash(f.RelPath)); !fileExists(abs) {
		t.Errorf("format RelPath %q does not resolve to a file", f.RelPath)
	}

	if b.CoverRelPath == "" {
		t.Error("CoverRelPath is empty despite has_cover=1 and a file on disk")
	}
	if b.CoverMtime == 0 {
		t.Error("CoverMtime is 0; it is the cache-buster input for CoverImageId")
	}
}

// A `data` row whose file was moved away by hand is common in real libraries.
// It must be reported as not present, not treated as an error.
func TestMissingFileIsFlaggedNotFatal(t *testing.T) {
	ctx := context.Background()
	lib := calibretest.New(t, calibretest.BookSpec{
		Title:   "Ghost",
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Missing: true}},
	})

	books, err := open(t, lib).Books(ctx, []int64{1})
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	f, ok := books[0].Format("EPUB")
	if !ok {
		t.Fatal("EPUB row missing from the model")
	}
	if f.Present {
		t.Error("format is marked present but the file was never written")
	}
}

// has_cover=1 with no cover.jpg on disk must degrade to "no cover".
func TestCoverInDatabaseOnly(t *testing.T) {
	ctx := context.Background()
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Coverless", CoverInDBOnly: true})

	books, err := open(t, lib).Books(context.Background(), []int64{1})
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	_ = ctx
	if books[0].CoverRelPath != "" {
		t.Errorf("CoverRelPath = %q, want empty", books[0].CoverRelPath)
	}
}

// The snapshot must include metadata.db-wal, or a library Calibre wrote to
// without checkpointing reads back stale.
func TestSnapshotIncludesWAL(t *testing.T) {
	ctx := context.Background()
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Walbook"})
	lib.LeaveDirtyWAL()

	stubs, err := open(t, lib).Stubs(ctx)
	if err != nil {
		t.Fatalf("Stubs: %v", err)
	}
	want := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if !stubs[0].LastModified.Equal(want) {
		t.Errorf("LastModified = %v, want %v — the WAL was not replayed", stubs[0].LastModified, want)
	}
}

// Opening must never touch the user's library. This asserts the snapshot copy
// is what gets written to, and that closing removes it.
func TestOpenDoesNotModifyLibrary(t *testing.T) {
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Untouched"})
	before, err := calibre.Stat(lib.Path)
	if err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	db, err := calibre.Open(lib.Path, workDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Stubs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after, err := calibre.Stat(lib.Path)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("metadata.db changed: %+v -> %+v", before, after)
	}

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Close left %d entries behind in the work dir", len(entries))
	}
}

func TestUnreachableLibrary(t *testing.T) {
	_, err := calibre.Open(filepath.Join(t.TempDir(), "not-a-library"), t.TempDir())
	if err == nil {
		t.Fatal("Open succeeded on a missing library")
	}
	if !isUnreachable(err) {
		t.Errorf("error = %v, want ErrUnreachable so the scheduler leaves the database alone", err)
	}
}

func TestCorruptDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metadata.db"), []byte("definitely not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := calibre.Open(dir, t.TempDir()); err == nil {
		t.Fatal("Open succeeded on a corrupt database")
	}
}

func TestChangedBooksAreDetectedByStub(t *testing.T) {
	ctx := context.Background()
	lib := calibretest.New(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)

	first, err := open(t, lib).Stubs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	newTime := time.Date(2027, 7, 7, 7, 7, 7, 0, time.UTC)
	lib.Touch(2, newTime)
	lib.Remove(1)
	lib.Add(calibretest.BookSpec{Title: "Three"})

	second, err := open(t, lib).Stubs(ctx)
	if err != nil {
		t.Fatal(err)
	}

	byID := map[int64]calibre.Stub{}
	for _, s := range second {
		byID[s.ID] = s
	}
	if _, stillThere := byID[1]; stillThere {
		t.Error("removed book still present")
	}
	if got := byID[2].LastModified; !got.Equal(newTime) {
		t.Errorf("touched book LastModified = %v, want %v", got, newTime)
	}
	if _, ok := byID[3]; !ok {
		t.Error("added book not present")
	}
	if len(first) != 2 || len(second) != 2 {
		t.Errorf("stub counts = %d then %d, want 2 then 2", len(first), len(second))
	}
}

func TestParsesTimestampVariants(t *testing.T) {
	ctx := context.Background()
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Timey"})

	want := time.Date(2019, 11, 12, 13, 14, 15, 0, time.UTC)
	for _, form := range []string{
		"2019-11-12 13:14:15.000000+00:00",
		"2019-11-12 13:14:15+00:00",
		"2019-11-12T13:14:15Z",
		"2019-11-12 13:14:15",
	} {
		lib.Exec(`UPDATE books SET last_modified = ? WHERE id = 1`, form)
		stubs, err := open(t, lib).Stubs(ctx)
		if err != nil {
			t.Fatalf("%s: %v", form, err)
		}
		if !stubs[0].LastModified.Equal(want) {
			t.Errorf("%q parsed as %v, want %v", form, stubs[0].LastModified, want)
		}
	}

	// Garbage must degrade to the zero time, not fail the scan.
	lib.Exec(`UPDATE books SET last_modified = 'not a date' WHERE id = 1`)
	stubs, err := open(t, lib).Stubs(ctx)
	if err != nil {
		t.Fatalf("unparseable timestamp failed the scan: %v", err)
	}
	if !stubs[0].LastModified.IsZero() {
		t.Errorf("unparseable timestamp = %v, want zero time", stubs[0].LastModified)
	}
}

// A hostile or corrupt metadata.db must not make the reader resolve paths
// outside the library root.
func TestPathEscapeIsRefused(t *testing.T) {
	ctx := context.Background()
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Escapee"})
	lib.Exec(`UPDATE books SET path = '../../../../etc' WHERE id = 1`)

	books, err := open(t, lib).Books(ctx, []int64{1})
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	for _, f := range books[0].Formats {
		if f.RelPath != "" || f.Present {
			t.Errorf("format resolved outside the library root: %+v", f)
		}
	}
}

func TestBatchingAcrossManyBooks(t *testing.T) {
	ctx := context.Background()

	specs := make([]calibretest.BookSpec, 0, 750)
	for i := range 750 {
		specs = append(specs, calibretest.BookSpec{Title: titleFor(i)})
	}
	lib := calibretest.New(t, specs...)
	db := open(t, lib)

	stubs, err := db.Stubs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(stubs))
	for i, s := range stubs {
		ids[i] = s.ID
	}

	books, err := db.Books(ctx, ids)
	if err != nil {
		t.Fatalf("Books: %v", err)
	}
	if len(books) != 750 {
		t.Fatalf("got %d books across batches, want 750", len(books))
	}
	for i, b := range books {
		if b.ID != ids[i] {
			t.Fatalf("book %d has id %d, want %d — batching lost the requested order", i, b.ID, ids[i])
		}
	}
}

func titleFor(i int) string {
	return "Book " + string(rune('A'+i%26)) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func isUnreachable(err error) bool {
	for err != nil {
		if err == calibre.ErrUnreachable {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
