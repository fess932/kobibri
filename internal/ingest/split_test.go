package ingest_test

import (
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// twoLibrariesOneTitle builds the situation the whole report exists for: two
// different books that share a title and an author and nothing else, so the
// weakest identity key joins them.
func twoLibrariesOneTitle(t *testing.T) (*harness, int64, int64) {
	t.Helper()
	h := newHarness(t)

	first := calibretest.New(t, calibretest.BookSpec{
		Title: "Selected Poems", Authors: []string{"Jane Author"},
		UUID: "aaaaaaaa-0000-0000-0000-000000000001",
	})
	second := calibretest.New(t, calibretest.BookSpec{
		Title: "Selected Poems", Authors: []string{"Jane Author"},
		UUID: "bbbbbbbb-0000-0000-0000-000000000002",
	})

	a := h.addSource("first", first.Path, 100)
	b := h.addSource("second", second.Path, 200)
	h.scan(a)
	h.scan(b)
	return h, a, b
}

func (h *harness) contributors(t *testing.T, bookID string) []store.Contributor {
	t.Helper()
	book, err := store.GetBook(h.ctx, h.store.Reader(), bookID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.Contributors(h.ctx, h.store.Reader(), book)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The report has to find a merge that rests on title and author alone.
func TestSuspectMergesFindsTheWeakOnes(t *testing.T) {
	h, _, _ := twoLibrariesOneTitle(t)

	suspects, err := ingest.SuspectMerges(h.ctx, h.store.Reader())
	if err != nil {
		t.Fatal(err)
	}
	if len(suspects) != 1 {
		t.Fatalf("%d suspects, want 1", len(suspects))
	}
	if len(suspects[0].Contributors) != 2 {
		t.Errorf("%d contributors, want 2", len(suspects[0].Contributors))
	}
}

// A merge backed by a shared uuid is evidence, not a guess, and must not be
// reported: burying the real cases in noise is how a report gets ignored.
func TestAMergeOnAUUIDIsNotSuspect(t *testing.T) {
	h := newHarness(t)
	shared := calibretest.BookSpec{
		Title: "Shared Book", Authors: []string{"Jane Author"},
		UUID: "cccccccc-0000-0000-0000-000000000003",
	}
	h.scan(h.addSource("first", calibretest.New(t, shared).Path, 100))
	h.scan(h.addSource("second", calibretest.New(t, shared).Path, 200))

	suspects, err := ingest.SuspectMerges(h.ctx, h.store.Reader())
	if err != nil {
		t.Fatal(err)
	}
	if len(suspects) != 0 {
		t.Errorf("%d suspects, want none — the copies share a uuid", len(suspects))
	}
}

// Splitting must move exactly one copy, and leave the original book's identity
// alone: that id is what every reader holds.
func TestSplittingLeavesTheOriginalIdentityAlone(t *testing.T) {
	h, _, _ := twoLibrariesOneTitle(t)

	original := h.bookByTitle("Selected Poems")
	contributors := h.contributors(t, original.ID)
	moving := contributors[1].SourceBookID

	newID, err := ingest.Split(h.ctx, h.store, moving)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if newID == original.ID {
		t.Fatal("the split produced the same book")
	}

	if got := h.contributors(t, original.ID); len(got) != 1 {
		t.Errorf("the original has %d copies, want 1", len(got))
	}
	if got := h.contributors(t, newID); len(got) != 1 {
		t.Errorf("the new book has %d copies, want 1", len(got))
	}

	// Both must still be servable; a split that leaves a book unsyncable has
	// taken a book away from a reader rather than separating two.
	for _, id := range []string{original.ID, newID} {
		book, err := store.GetBook(h.ctx, h.store.Reader(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !book.Syncable {
			t.Errorf("book %s is not syncable after the split", id)
		}
	}
}

// The point of pinning: the keys that joined them still match, so without it the
// very next scan would merge them straight back.
func TestASplitSurvivesTheNextScan(t *testing.T) {
	h, a, b := twoLibrariesOneTitle(t)

	original := h.bookByTitle("Selected Poems")
	moving := h.contributors(t, original.ID)[1].SourceBookID
	newID, err := ingest.Split(h.ctx, h.store, moving)
	if err != nil {
		t.Fatal(err)
	}

	h.scan(a)
	h.scan(b)

	if got := h.contributors(t, original.ID); len(got) != 1 {
		t.Errorf("a scan merged the copies back: the original has %d again", len(got))
	}
	if got := h.contributors(t, newID); len(got) != 1 {
		t.Errorf("the split-off book has %d copies after a scan", len(got))
	}
	if h.count(`SELECT count(*) FROM books WHERE merged_into IS NULL`) != 2 {
		t.Error("a scan did not leave two separate books")
	}
}

// Splitting the only copy would leave an empty book behind.
func TestTheLastCopyCannotBeSplitOff(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Alone"})
	h.scan(h.addSource("only", lib.Path, 100))

	book := h.bookByTitle("Alone")
	only := h.contributors(t, book.ID)[0].SourceBookID

	if _, err := ingest.Split(h.ctx, h.store, only); err == nil {
		t.Error("the only copy of a book was split off it")
	}
}

// A split made by mistake has to be undoable.
func TestASplitCanBePutBack(t *testing.T) {
	h, _, _ := twoLibrariesOneTitle(t)

	original := h.bookByTitle("Selected Poems")
	moving := h.contributors(t, original.ID)[1].SourceBookID
	if _, err := ingest.Split(h.ctx, h.store, moving); err != nil {
		t.Fatal(err)
	}

	if err := ingest.Rejoin(h.ctx, h.store, moving); err != nil {
		t.Fatalf("Rejoin: %v", err)
	}
	if got := h.contributors(t, original.ID); len(got) != 2 {
		t.Errorf("the copy did not come back: the original has %d", len(got))
	}
}
