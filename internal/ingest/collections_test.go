package ingest_test

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

type collEnv struct {
	store   *store.Store
	scanner *ingest.Scanner
	userID  int64
	ctx     context.Context
}

func newCollEnv(t *testing.T, mode string, books ...calibretest.BookSpec) *collEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	userID, err := store.CreateUser(ctx, st.Writer(), "reader", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ingest.SetCollectionsMode(ctx, st.Writer(), mode); err != nil {
		t.Fatal(err)
	}

	lib := calibretest.New(t, books...)
	sourceID, err := store.CreateSource(ctx, st.Writer(), &store.Source{
		Name: "main", LibraryPath: lib.Path, Priority: 100, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	scanner := ingest.NewScanner(st, filepath.Join(dir, "tmp"))
	if _, err := scanner.Scan(ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	return &collEnv{store: st, scanner: scanner, userID: userID, ctx: ctx}
}

// shelves reports each live collection and how many books are on it.
func (e *collEnv) shelves(t *testing.T) map[string]int {
	t.Helper()
	tags, err := store.ListTags(e.ctx, e.store.Reader(), e.userID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, tag := range tags {
		ids, err := store.TagBookIDs(e.ctx, e.store.Reader(), tag.ID)
		if err != nil {
			t.Fatal(err)
		}
		out[tag.Name] = len(ids)
	}
	return out
}

func names(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The default has to be off: a library with two hundred tags would otherwise put
// two hundred shelves on someone's reader without being asked.
func TestCollectionsAreOffUntilAskedFor(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsOff,
		calibretest.BookSpec{Title: "One", Tags: []string{"scifi"}, Series: "Dune"},
	)
	if got := e.shelves(t); len(got) != 0 {
		t.Errorf("built %v with collections off", names(got))
	}
}

func TestTagsBecomeShelves(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsTags,
		calibretest.BookSpec{Title: "One", Tags: []string{"scifi", "favourites"}},
		calibretest.BookSpec{Title: "Two", Tags: []string{"scifi"}},
		calibretest.BookSpec{Title: "Three"},
	)

	got := e.shelves(t)
	if got["scifi"] != 2 {
		t.Errorf("scifi holds %d books, want 2", got["scifi"])
	}
	if got["favourites"] != 1 {
		t.Errorf("favourites holds %d books, want 1", got["favourites"])
	}
	if len(got) != 2 {
		t.Errorf("shelves are %v, want just the two tags", names(got))
	}
}

func TestSeriesBecomeShelves(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsSeries,
		calibretest.BookSpec{Title: "One", Series: "Dune", Tags: []string{"scifi"}},
		calibretest.BookSpec{Title: "Two", Series: "Dune"},
	)

	got := e.shelves(t)
	if got["Dune"] != 2 {
		t.Errorf("Dune holds %d books, want 2", got["Dune"])
	}
	if _, ok := got["scifi"]; ok {
		t.Error("a tag became a shelf although only series were asked for")
	}
}

// Rebuilding must not churn: a device is told about a collection when its
// revision moves, so a no-op pass that bumped revisions would re-announce every
// shelf on every scan.
func TestRebuildingChangesNothingOnItsOwn(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsBoth,
		calibretest.BookSpec{Title: "One", Tags: []string{"scifi"}, Series: "Dune"},
	)

	before, err := store.ListTags(e.ctx, e.store.Reader(), e.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.scanner.RebuildCollections(e.ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListTags(e.ctx, e.store.Reader(), e.userID)
	if err != nil {
		t.Fatal(err)
	}

	if len(before) != len(after) {
		t.Fatalf("%d shelves became %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Rev != after[i].Rev {
			t.Errorf("%s moved from revision %d to %d without changing",
				before[i].Name, before[i].Rev, after[i].Rev)
		}
	}
}

// A shelf someone threw away on their reader must stay thrown away. Putting it
// back on the next scan is an argument nobody can win.
func TestAShelfDeletedOnTheReaderStaysDeleted(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsTags,
		calibretest.BookSpec{Title: "One", Tags: []string{"scifi"}},
	)

	tags, err := store.ListTags(e.ctx, e.store.Reader(), e.userID)
	if err != nil || len(tags) != 1 {
		t.Fatalf("want one shelf, got %v (%v)", tags, err)
	}
	if err := store.DeleteTag(e.ctx, e.store.Writer(), tags[0].ID); err != nil {
		t.Fatal(err)
	}

	if err := e.scanner.RebuildCollections(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.shelves(t); len(got) != 0 {
		t.Errorf("a deleted shelf came back as %v", names(got))
	}
}

// A shelf someone made themselves is theirs, and rebuilding must not touch it.
func TestShelvesMadeOnTheReaderAreLeftAlone(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsTags,
		calibretest.BookSpec{Title: "One", Tags: []string{"scifi"}},
	)

	id, err := store.CreateTag(e.ctx, e.store.Writer(), e.userID, "Bedtime", store.TagOriginDevice)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.scanner.RebuildCollections(e.ctx); err != nil {
		t.Fatal(err)
	}

	tag, err := store.GetTag(e.ctx, e.store.Reader(), id)
	if err != nil {
		t.Fatalf("a device's own shelf was removed: %v", err)
	}
	if tag.DeletedAt != "" {
		t.Error("a device's own shelf was deleted by the rebuild")
	}
}

// A tag that leaves the library takes its shelf with it — and, unlike one a
// reader deleted, comes back if the library gets it back.
func TestAShelfFollowsTheLibrary(t *testing.T) {
	e := newCollEnv(t, ingest.CollectionsTags,
		calibretest.BookSpec{Title: "One", Tags: []string{"scifi"}},
	)
	if got := e.shelves(t); got["scifi"] != 1 {
		t.Fatalf("shelves are %v, want scifi with one book", got)
	}

	// The book is hidden, so nothing is left to hang the shelf on.
	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET hidden = 1, syncable = 0`); err != nil {
		t.Fatal(err)
	}
	if err := e.scanner.RebuildCollections(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.shelves(t); len(got) != 0 {
		t.Errorf("shelves are %v after the last book left, want none", names(got))
	}

	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET hidden = 0, syncable = 1`); err != nil {
		t.Fatal(err)
	}
	if err := e.scanner.RebuildCollections(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.shelves(t); got["scifi"] != 1 {
		t.Errorf("shelves are %v after the book came back, want scifi with one book", got)
	}
}
