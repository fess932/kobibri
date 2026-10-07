package ingest_test

import (
	"os"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

func (h *harness) filePath(book *store.Book, format string) string {
	h.t.Helper()
	path, err := store.BookFilePath(h.ctx, h.store.Reader(), book, format)
	if err != nil {
		h.t.Fatalf("no %s behind %q: %v", format, book.Title, err)
	}
	return path
}

func TestAScanCopiesTheLibraryHere(t *testing.T) {
	h := newHarness(t)
	own := t.TempDir()
	h.scanner.KeepCopiesIn(own)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Copied"})

	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	path := h.filePath(h.bookByTitle("Copied"), "EPUB")
	if !strings.HasPrefix(path, own) {
		t.Fatalf("the book is read from %s, want this server's own copy under %s", path, own)
	}

	if err := os.RemoveAll(lib.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the copy went with the library: %v", err)
	}
	if cover, err := store.BookCoverPath(h.ctx, h.store.Reader(), h.bookByTitle("Copied").ID); err == nil {
		if _, err := os.Stat(cover); err != nil {
			t.Errorf("the cover was not copied: %v", err)
		}
	}
}

func TestCopyingALibraryMovesNothingADeviceSees(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Steady"})
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	before := h.bookByTitle("Steady")
	original, err := os.Stat(h.filePath(before, "EPUB"))
	if err != nil {
		t.Fatal(err)
	}

	own := t.TempDir()
	h.scanner.KeepCopiesIn(own)
	res, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("a library that had not been copied yet was skipped as unchanged")
	}

	after := h.bookByTitle("Steady")
	if after.MetadataRev != before.MetadataRev || after.ServingHash != before.ServingHash ||
		after.CoverImageID != before.CoverImageID {
		t.Errorf("copying the library moved the book for devices: rev %d -> %d, hash %q -> %q, cover %q -> %q",
			before.MetadataRev, after.MetadataRev, before.ServingHash, after.ServingHash,
			before.CoverImageID, after.CoverImageID)
	}

	path := h.filePath(after, "EPUB")
	if !strings.HasPrefix(path, own) {
		t.Fatalf("after the copy the book is still read from %s", path)
	}
	copied, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Size() != original.Size() || copied.ModTime().Unix() != original.ModTime().Unix() {
		t.Errorf("the copy is %d bytes at %v, the original %d bytes at %v",
			copied.Size(), copied.ModTime(), original.Size(), original.ModTime())
	}

	again, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Skipped {
		t.Error("an unchanged, already copied library was read again")
	}
}

func TestABookAddedLaterIsCopiedToo(t *testing.T) {
	h := newHarness(t)
	own := t.TempDir()
	h.scanner.KeepCopiesIn(own)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "First"})
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	lib.Add(calibretest.BookSpec{Title: "Second"})
	h.scan(sourceID)

	path := h.filePath(h.bookByTitle("Second"), "EPUB")
	if !strings.HasPrefix(path, own) {
		t.Fatalf("the new book is read from %s, want the copy under %s", path, own)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the new book was not copied: %v", err)
	}
}

func TestRemovingALibraryKeepsItsBooks(t *testing.T) {
	h := newHarness(t)
	h.scanner.KeepCopiesIn(t.TempDir())
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Stays Here"})
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)
	original := h.bookByTitle("Stays Here")

	if err := os.RemoveAll(lib.Path); err != nil {
		t.Fatal(err)
	}
	kept, err := h.scanner.KeepBooksOf(h.ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !kept {
		t.Fatal("removing a copied library did not keep its books")
	}

	book := h.bookByTitle("Stays Here")
	if book.ID != original.ID {
		t.Fatalf("book id changed: %s -> %s", original.ID, book.ID)
	}
	if !book.Available || !book.Syncable {
		t.Errorf("available=%v syncable=%v after the library was removed, want both", book.Available, book.Syncable)
	}
	if book.MetadataRev != original.MetadataRev {
		t.Errorf("metadata_rev moved %d -> %d, so devices are told about a book that did not change",
			original.MetadataRev, book.MetadataRev)
	}
	if _, err := os.Stat(h.filePath(book, "EPUB")); err != nil {
		t.Errorf("the kept book has no file: %v", err)
	}

	src, err := store.GetSource(h.ctx, h.store.Reader(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != store.SourceKindKept {
		t.Errorf("source kind = %q, want %q", src.Kind, store.SourceKindKept)
	}
	if res, err := h.scanner.Scan(h.ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil || !res.Skipped {
		t.Errorf("a kept source was scanned: skipped=%v err=%v", res.Skipped, err)
	}
	if again := h.bookByTitle("Stays Here"); !again.Syncable {
		t.Error("the kept book stopped being syncable after a scan")
	}
}

func TestALibraryNeverCopiedIsSimplyRemoved(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Not Copied"})
	sourceID := h.addSource("main", lib.Path, 100)
	h.scan(sourceID)

	h.scanner.KeepCopiesIn(t.TempDir())
	if err := os.RemoveAll(lib.Path); err != nil {
		t.Fatal(err)
	}
	kept, err := h.scanner.KeepBooksOf(h.ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if kept {
		t.Error("a library with nothing copied claims to have kept its books")
	}
}
