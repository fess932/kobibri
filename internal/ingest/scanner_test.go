package ingest_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

type harness struct {
	t       *testing.T
	store   *store.Store
	scanner *ingest.Scanner
	ctx     context.Context
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	return &harness{t: t, store: st, ctx: ctx,
		scanner: ingest.NewScanner(st, filepath.Join(dir, "tmp"))}
}

func (h *harness) addSource(name, path string, priority int) int64 {
	h.t.Helper()
	src := &store.Source{Name: name, LibraryPath: path, Priority: priority,
		Enabled: true, ShareAll: true}
	id, err := store.CreateSource(h.ctx, h.store.Writer(), src)
	if err != nil {
		h.t.Fatalf("create source: %v", err)
	}
	return id
}

func (h *harness) scan(sourceID int64) ingest.Result {
	h.t.Helper()
	res, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{Force: true})
	if err != nil {
		h.t.Fatalf("scan source %d: %v", sourceID, err)
	}
	return res
}

// books returns every canonical (non-alias) book, ordered by title.
func (h *harness) books() []*store.Book {
	h.t.Helper()
	rows, err := h.store.Reader().QueryContext(h.ctx,
		`SELECT id FROM books WHERE merged_into IS NULL ORDER BY title, id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()

	var out []*store.Book
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		b, err := store.GetBook(h.ctx, h.store.Reader(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func (h *harness) bookByTitle(title string) *store.Book {
	h.t.Helper()
	for _, b := range h.books() {
		if b.Title == title {
			return b
		}
	}
	h.t.Fatalf("no canonical book titled %q; have %v", title, h.titles())
	return nil
}

func (h *harness) titles() []string {
	var out []string
	for _, b := range h.books() {
		out = append(out, b.Title)
	}
	return out
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.store.Reader().QueryRowContext(h.ctx, query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func TestScanIngestsBooks(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t,
		calibretest.BookSpec{Title: "The Long Book", Authors: []string{"Jane Author"},
			Series: "The Series", SeriesIndex: 2.5, Publisher: "Some Press",
			Languages: []string{"eng"}, Cover: true},
		calibretest.BookSpec{Title: "Pdf Only", Authors: []string{"Paper Person"},
			Formats: []calibretest.FormatSpec{{Format: "PDF"}}},
	)

	res := h.scan(h.addSource("main", lib.Path, 100))
	if res.Added != 2 || res.Seen != 2 {
		t.Fatalf("scan result = %+v, want 2 seen and 2 added", res)
	}

	long := h.bookByTitle("The Long Book")
	if !long.Available || !long.Syncable {
		t.Errorf("available=%v syncable=%v, want both true", long.Available, long.Syncable)
	}
	if long.DownloadFormat != store.FormatKEPUB {
		t.Errorf("DownloadFormat = %q, want KEPUB", long.DownloadFormat)
	}
	if long.SeriesName != "The Series" || !long.SeriesIndex.Valid || long.SeriesIndex.Float64 != 2.5 {
		t.Errorf("series = %q #%v", long.SeriesName, long.SeriesIndex)
	}
	if long.SeriesUUID != ingest.SeriesUUID("The Series") {
		t.Errorf("SeriesUUID = %q, want the uuid3 of the series name", long.SeriesUUID)
	}
	if long.CoverImageID == "" {
		t.Error("CoverImageID is empty despite a cover on disk")
	}

	// PDF-only books cannot be synced to a Kobo at all.
	pdf := h.bookByTitle("Pdf Only")
	if pdf.Syncable {
		t.Error("a PDF-only book is marked syncable")
	}
	if pdf.DownloadFormat != "" {
		t.Errorf("DownloadFormat = %q for a PDF-only book, want empty", pdf.DownloadFormat)
	}
}

// A pre-paginated EPUB must be offered as EPUB3FL and never kepubified.
func TestFixedLayoutIsOfferedAsEPUB3FL(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{
		Title:   "Fixed Layout Art",
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: "pre-paginated"}},
	})
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	if got := h.bookByTitle("Fixed Layout Art").DownloadFormat; got != store.FormatEPUB3FL {
		t.Errorf("DownloadFormat = %q, want EPUB3FL — a fixed-layout book must not be converted", got)
	}

	// The layout must have been recorded during the scan, not guessed later.
	var layout string
	err := h.store.Reader().QueryRowContext(h.ctx,
		`SELECT layout FROM source_book_files WHERE format = 'EPUB'`).Scan(&layout)
	if err != nil {
		t.Fatal(err)
	}
	if layout != store.LayoutPrePaginated {
		t.Errorf("recorded layout = %q, want %q", layout, store.LayoutPrePaginated)
	}
	_ = sourceID
}

// A normal book must stay reflowable, or it would be served unconverted and
// lose span-level reading progress.
func TestReflowableBookIsOfferedAsKepub(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Normal Book"})
	h.scan(h.addSource("main", lib.Path, 100))

	if got := h.bookByTitle("Normal Book").DownloadFormat; got != store.FormatKEPUB {
		t.Errorf("DownloadFormat = %q, want KEPUB", got)
	}
}

// Two libraries holding the same book must collapse into one canonical row.
func TestTwoSourcesMergeOnCalibreUUID(t *testing.T) {
	h := newHarness(t)
	shared := calibretest.BookSpec{
		Title: "Shared Book", Authors: []string{"Jane Author"},
		UUID: "aaaaaaaa-0000-4000-8000-000000000001", Cover: true,
	}

	libA := calibretest.New(t, shared, calibretest.BookSpec{Title: "Only In A"})
	libB := calibretest.New(t, shared, calibretest.BookSpec{Title: "Only In B"})

	h.scan(h.addSource("a", libA.Path, 100))
	h.scan(h.addSource("b", libB.Path, 200))

	if got := len(h.books()); got != 3 {
		t.Fatalf("got %d canonical books, want 3 (the shared one merged): %v", got, h.titles())
	}
	if n := h.count(`SELECT count(*) FROM source_books WHERE book_id = ?`,
		h.bookByTitle("Shared Book").ID); n != 2 {
		t.Errorf("shared book has %d contributing source rows, want 2", n)
	}
}

// A source that only knows title+author must still merge with one that carries
// an ISBN, once a bridging row appears.
func TestMergeAcrossDifferentIdentityKinds(t *testing.T) {
	h := newHarness(t)

	// Different Calibre uuids, but the same ISBN.
	libA := calibretest.New(t, calibretest.BookSpec{
		Title: "Bridged", Authors: []string{"Jane Author"},
		UUID:        "aaaaaaaa-0000-4000-8000-00000000000a",
		Identifiers: map[string]string{"isbn": "9780306406157"},
	})
	libB := calibretest.New(t, calibretest.BookSpec{
		Title: "Bridged", Authors: []string{"Jane Author"},
		UUID:        "bbbbbbbb-0000-4000-8000-00000000000b",
		Identifiers: map[string]string{"isbn": "0306406152"}, // ISBN-10 of the same book
	})

	h.scan(h.addSource("a", libA.Path, 100))
	h.scan(h.addSource("b", libB.Path, 200))

	if got := len(h.books()); got != 1 {
		t.Fatalf("got %d canonical books, want 1: %v", got, h.titles())
	}
}

// The headline requirement: a source disappearing must not disturb the
// canonical id, because that id is all a device knows.
func TestBookIDSurvivesSourceRemovalAndReadd(t *testing.T) {
	h := newHarness(t)
	spec := calibretest.BookSpec{Title: "Durable", Authors: []string{"Jane Author"},
		UUID: "cccccccc-0000-4000-8000-00000000000c"}
	lib := calibretest.New(t, spec)

	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)
	original := h.bookByTitle("Durable").ID

	// The library goes away entirely.
	if err := h.scanner.SetSourceEnabled(h.ctx, sourceID, false); err != nil {
		t.Fatal(err)
	}

	gone := h.bookByTitle("Durable")
	if gone.ID != original {
		t.Fatalf("book id changed while the source was away: %s -> %s", original, gone.ID)
	}
	if gone.Available || gone.Syncable {
		t.Errorf("available=%v syncable=%v, want both false while the source is disabled",
			gone.Available, gone.Syncable)
	}

	// And it comes back. Re-enabling alone must restore it: nothing changed in
	// Calibre, so a scan would find no changed books to re-resolve.
	if err := h.scanner.SetSourceEnabled(h.ctx, sourceID, true); err != nil {
		t.Fatal(err)
	}

	back := h.bookByTitle("Durable")
	if back.ID != original {
		t.Fatalf("book id changed after the source returned: %s -> %s", original, back.ID)
	}
	if !back.Syncable {
		t.Error("book is not syncable again after the source returned")
	}
}

// Ingest must never delete rows: history and canonical ids depend on them.
func TestVanishedBookIsFlaggedNotDeleted(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t,
		calibretest.BookSpec{Title: "Stays"},
		calibretest.BookSpec{Title: "Goes"},
	)
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	goesID := h.bookByTitle("Goes").ID
	lib.Remove(2)

	res := h.scan(sourceID)
	if res.Vanished != 1 {
		t.Fatalf("vanished = %d, want 1", res.Vanished)
	}

	if n := h.count(`SELECT count(*) FROM source_books`); n != 2 {
		t.Errorf("source_books has %d rows, want 2 — ingest must not delete rows", n)
	}
	if n := h.count(`SELECT count(*) FROM source_books WHERE missing = 1`); n != 1 {
		t.Errorf("%d rows flagged missing, want 1", n)
	}

	gone, err := store.GetBook(h.ctx, h.store.Reader(), goesID)
	if err != nil {
		t.Fatalf("canonical book was deleted along with the source row: %v", err)
	}
	if gone.Available || gone.Syncable {
		t.Errorf("available=%v syncable=%v, want both false", gone.Available, gone.Syncable)
	}
}

// A half-mounted share looks exactly like a library that lost most of its
// books. The guard must refuse it and leave the database untouched.
func TestSuspiciousVanishIsRefused(t *testing.T) {
	h := newHarness(t)

	specs := make([]calibretest.BookSpec, 0, 40)
	for i := range 40 {
		specs = append(specs, calibretest.BookSpec{Title: "Book " + string(rune('A'+i))})
	}
	lib := calibretest.New(t, specs...)

	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)
	before := h.count(`SELECT count(*) FROM source_books WHERE missing = 0`)

	for id := int64(1); id <= 30; id++ {
		lib.Remove(id)
	}

	_, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{Force: true})
	if err == nil {
		t.Fatal("scan accepted a mass disappearance without confirmation")
	}
	if after := h.count(`SELECT count(*) FROM source_books WHERE missing = 0`); after != before {
		t.Errorf("live rows changed from %d to %d despite the refusal", before, after)
	}

	src, err := store.GetSource(h.ctx, h.store.Reader(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if src.LastStatus != store.SourceStatusSuspicious {
		t.Errorf("source status = %q, want %q", src.LastStatus, store.SourceStatusSuspicious)
	}

	// With explicit confirmation it goes through.
	if _, err := h.scanner.Scan(h.ctx, sourceID,
		ingest.ScanOptions{Force: true, ConfirmVanish: true}); err != nil {
		t.Fatalf("confirmed scan failed: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM source_books WHERE missing = 1`); n != 30 {
		t.Errorf("%d rows flagged missing after confirmation, want 30", n)
	}
}

// An unreachable library must change nothing at all.
func TestUnreachableSourceChangesNothing(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)
	before := h.count(`SELECT count(*) FROM source_books WHERE missing = 0`)

	// The share goes away.
	h.mustExec(`UPDATE sources SET library_path = ? WHERE id = ?`,
		filepath.Join(t.TempDir(), "unmounted"), sourceID)

	if _, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{Force: true}); err == nil {
		t.Fatal("scan of an unreachable library succeeded")
	}
	if after := h.count(`SELECT count(*) FROM source_books WHERE missing = 0`); after != before {
		t.Errorf("live rows changed from %d to %d for an unreachable library", before, after)
	}
	if n := h.count(`SELECT count(*) FROM books WHERE syncable = 1`); n != 2 {
		t.Errorf("%d syncable books after an unreachable scan, want 2", n)
	}
}

// metadata_rev is what pushes an update to every device, so it must move only
// when the device can actually observe a difference.
func TestMetadataRevOnlyMovesOnObservableChange(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Revved", Authors: []string{"Jane Author"}})
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	first := h.bookByTitle("Revved")

	// A scan that re-reads the same data must not bump anything.
	h.scan(sourceID)
	if got := h.bookByTitle("Revved").MetadataRev; got != first.MetadataRev {
		t.Errorf("metadata_rev moved on a no-op scan: %d -> %d", first.MetadataRev, got)
	}

	// last_modified alone changing is not observable to a device either.
	lib.Touch(1, time.Now().Add(time.Hour))
	h.scan(sourceID)
	if got := h.bookByTitle("Revved").MetadataRev; got != first.MetadataRev {
		t.Errorf("metadata_rev moved on a touch-only change: %d -> %d", first.MetadataRev, got)
	}

	// A real title change is.
	lib.Exec(`UPDATE books SET title = 'Revved Again' WHERE id = 1`)
	lib.Touch(1, time.Now().Add(2*time.Hour))
	h.scan(sourceID)

	after := h.bookByTitle("Revved Again")
	if after.MetadataRev != first.MetadataRev+1 {
		t.Errorf("metadata_rev = %d after a title change, want %d",
			after.MetadataRev, first.MetadataRev+1)
	}
	if after.ID != first.ID {
		t.Errorf("book id changed on a title edit: %s -> %s", first.ID, after.ID)
	}
}

// The higher-priority source wins metadata, but an empty field falls back.
func TestWinnerSelectionAndFieldFallback(t *testing.T) {
	h := newHarness(t)
	uuid := "dddddddd-0000-4000-8000-00000000000d"

	rich := calibretest.New(t, calibretest.BookSpec{
		Title: "Rich Copy", Authors: []string{"Jane Author"}, UUID: uuid,
		Description: "<p>The good blurb.</p>", Publisher: "Good Press",
	})
	poor := calibretest.New(t, calibretest.BookSpec{
		Title: "Poor Copy", Authors: []string{"Jane Author"}, UUID: uuid,
	})

	// The poor copy has the better priority, so it wins the record...
	h.scan(h.addSource("poor", poor.Path, 10))
	h.scan(h.addSource("rich", rich.Path, 20))

	book := h.books()[0]
	if book.Title != "Poor Copy" {
		t.Errorf("title = %q, want the higher-priority source's %q", book.Title, "Poor Copy")
	}
	// ...but its empty description falls back to the other source.
	if book.DescriptionHTML != "<p>The good blurb.</p>" {
		t.Errorf("description = %q, want the fallback from the lower-priority source",
			book.DescriptionHTML)
	}
	if book.Publisher != "Good Press" {
		t.Errorf("publisher = %q, want the fallback", book.Publisher)
	}
}

// A source row with no readable EPUB must not beat one that has a file we can
// actually serve, whatever the priority says.
func TestSourceWithAReadableFileWinsRegardlessOfPriority(t *testing.T) {
	h := newHarness(t)
	uuid := "eeeeeeee-0000-4000-8000-00000000000e"

	broken := calibretest.New(t, calibretest.BookSpec{
		Title: "Broken Copy", Authors: []string{"Jane Author"}, UUID: uuid,
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Missing: true}},
	})
	good := calibretest.New(t, calibretest.BookSpec{
		Title: "Good Copy", Authors: []string{"Jane Author"}, UUID: uuid,
	})

	h.scan(h.addSource("broken", broken.Path, 1)) // best priority, no file
	h.scan(h.addSource("good", good.Path, 500))   // worst priority, real file

	book := h.books()[0]
	if book.Title != "Good Copy" {
		t.Errorf("title = %q, want the source that actually has the file", book.Title)
	}
	if !book.Syncable {
		t.Error("book is not syncable despite one source having a readable EPUB")
	}
}

// An unchanged metadata.db must not be copied and re-read at all.
func TestUnchangedLibraryIsSkipped(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Static"})
	sourceID := h.addSource("main", lib.Path, 100)

	h.scan(sourceID)

	res, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{})
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if !res.Skipped {
		t.Error("an unchanged library was scanned again instead of being skipped")
	}
}

// A merged-away id must keep resolving: a device may still be holding it.
func TestOldIDResolvesAfterMerge(t *testing.T) {
	h := newHarness(t)

	libA := calibretest.New(t, calibretest.BookSpec{
		Title: "Bridged", Authors: []string{"Jane Author"},
		UUID: "11111111-0000-4000-8000-000000000001",
	})
	libB := calibretest.New(t, calibretest.BookSpec{
		Title: "Bridged", Authors: []string{"Jane Author"},
		UUID: "22222222-0000-4000-8000-000000000002",
	})

	h.scan(h.addSource("a", libA.Path, 100))
	firstID := h.books()[0].ID

	h.scan(h.addSource("b", libB.Path, 200))
	if got := len(h.books()); got != 1 {
		t.Fatalf("got %d canonical books after the merge, want 1", got)
	}
	survivor := h.books()[0].ID

	resolved, err := store.ResolveBookID(h.ctx, h.store.Reader(), firstID)
	if err != nil {
		t.Fatalf("the pre-merge id no longer resolves: %v", err)
	}
	if resolved != survivor {
		t.Errorf("old id resolved to %s, want the survivor %s", resolved, survivor)
	}
	if firstID != survivor {
		t.Logf("survivor is %s; the older id %s is now an alias", survivor, firstID)
	}
}

func (h *harness) mustExec(query string, args ...any) {
	h.t.Helper()
	if err := h.store.Tx(h.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(h.ctx, query, args...)
		return err
	}); err != nil {
		h.t.Fatalf("exec %q: %v", query, err)
	}
}
