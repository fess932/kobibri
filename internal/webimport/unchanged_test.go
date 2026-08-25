package webimport

import (
	"context"
	"os"
	"testing"

	"github.com/fess932/kobibri/internal/store"
)

// fileState is everything a device can notice about a book on disk.
type fileState struct {
	size    int64
	mtime   int64
	cover   int64
	rev     int64
	imageID string
}

func stateOf(t *testing.T, ctx context.Context, st *store.Store, bookID string) fileState {
	t.Helper()

	book, err := store.GetBook(ctx, st.Reader(), bookID)
	if err != nil {
		t.Fatalf("get book: %v", err)
	}
	path, err := store.BookFilePath(ctx, st.Reader(), book, "EPUB")
	if err != nil {
		t.Fatalf("no file behind the book: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the assembled file: %v", err)
	}

	var cover int64
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT cover_mtime FROM source_books WHERE book_id = ?`, bookID).Scan(&cover); err != nil {
		t.Fatalf("cover mtime: %v", err)
	}
	return fileState{
		size: fi.Size(), mtime: fi.ModTime().UnixNano(), cover: cover,
		rev: book.MetadataRev, imageID: book.CoverImageID,
	}
}

// A check that finds nothing must leave every byte and every timestamp alone.
//
// Reassembling the file would give it a new mtime, which is what the kepub
// cache is keyed by, and rewrite the cover, whose mtime is inside CoverImageId
// and therefore inside serving_hash. Both together mean a full re-download of a
// book whose text has not moved — every ten hours, on every device.
func TestCheckWithNothingNewChangesNothing(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, ctx, st, first.BookID)
	fetched := src.fetched

	again, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Rebuilt {
		t.Error("the book was assembled again though nothing had changed")
	}
	if again.BookID != first.BookID {
		t.Fatalf("the check landed on a different book: %s -> %s", first.BookID, again.BookID)
	}
	if again.Chapters != 3 {
		t.Errorf("Chapters = %d after a check that found nothing, want 3", again.Chapters)
	}
	if src.fetched != fetched {
		t.Errorf("the site was asked for %d more chapters, want none", src.fetched-fetched)
	}

	after := stateOf(t, ctx, st, first.BookID)
	switch {
	case after.mtime != before.mtime:
		t.Error("the assembled file was rewritten; the cached kepub is now stale for nothing")
	case after.cover != before.cover:
		t.Error("the cover was rewritten; every device will refetch it")
	case after.imageID != before.imageID:
		t.Errorf("CoverImageId moved: %q -> %q", before.imageID, after.imageID)
	case after.rev != before.rev:
		t.Errorf("metadata_rev moved %d -> %d, so every device is told to fetch the book again",
			before.rev, after.rev)
	case after.size != before.size:
		t.Errorf("file size moved %d -> %d", before.size, after.size)
	}

	// The check itself is still recorded, or the page cannot say when the site
	// was last asked.
	var checked string
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT w.checked_at FROM web_imports w
		 JOIN source_books sb ON sb.id = w.source_book_id WHERE sb.book_id = ?`,
		first.BookID).Scan(&checked); err != nil {
		t.Fatal(err)
	}
	if checked == "" {
		t.Error("a check that found nothing was not recorded at all")
	}
}

// A missing file is rebuilt even when the signature matches: the signature says
// what the file would be built from, not that it is still there.
func TestADeletedFileIsRebuilt(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 2}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	book, err := store.GetBook(ctx, st.Reader(), first.BookID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.BookFilePath(ctx, st.Reader(), book, "EPUB")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	again, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Rebuilt {
		t.Fatal("a book whose file had been deleted was not assembled again")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the file is still missing after a rebuild: %v", err)
	}
}

// The history answers "why did this book update?", which is the question a
// device re-downloading a serial raises.
func TestHistoryRecordsWhatChanged(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, _ := newImporter(t, src)

	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	// A check that finds nothing must not fill the history with noise.
	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	src.chapters = 5
	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatal(err)
	}

	events, err := im.Events(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("history has %d entries, want the first download and the two new chapters: %+v",
			len(events), events)
	}

	newest, first := events[0], events[1]
	if newest.Kind != EventChapters {
		t.Errorf("newest entry is %q, want %q", newest.Kind, EventChapters)
	}
	if newest.Added() != 2 {
		t.Errorf("newest entry reports %d new chapters, want 2", newest.Added())
	}
	if newest.Detail == "" {
		t.Error("the new chapters are not named, so the list says nothing")
	}
	if first.Kind != EventImported {
		t.Errorf("oldest entry is %q, want %q", first.Kind, EventImported)
	}
	if newest.Title != "A Serial Story" || newest.BookID == "" {
		t.Errorf("entry does not point at the book: %+v", newest)
	}
}
