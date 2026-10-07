package webimport

import (
	"archive/zip"
	"context"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/store"
)

func TestAFirstChapterSurvivesLaterChecks(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 10}
	im, _ := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{FromChapter: 8})
	if err != nil {
		t.Fatal(err)
	}
	if first.Chapters != 3 || src.fetched != 3 {
		t.Fatalf("imported %d chapters with %d fetched, want chapters 8-10 only",
			first.Chapters, src.fetched)
	}

	src.chapters = 12
	im.RefreshAll(ctx)
	if src.fetched != 5 {
		t.Errorf("the check fetched %d chapters in all, want 5: the two new ones and nothing before chapter 8",
			src.fetched)
	}

	imported, err := im.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 1 || imported[0].ChaptersDone != 5 || imported[0].FromChapter != 8 {
		t.Errorf("after the check the import is %+v, want 5 chapters starting at 8", imported)
	}

	again, err := im.Refresh(ctx, first.BookID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Rebuilt || src.fetched != 5 {
		t.Errorf("a check with nothing new rebuilt=%v and fetched %d, want no rebuild and 5",
			again.Rebuilt, src.fetched)
	}
}

func TestALastChapterStopsTheChecks(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 10}
	im, _ := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{FromChapter: 2, ToChapter: 4})
	if err != nil {
		t.Fatal(err)
	}
	if first.Chapters != 3 {
		t.Fatalf("imported %d chapters, want chapters 2-4", first.Chapters)
	}

	src.chapters = 20
	im.RefreshAll(ctx)
	if src.fetched != 3 {
		t.Errorf("a book with a last chapter set was checked: %d chapters fetched, want 3", src.fetched)
	}
	if err := im.StartRefresh(ctx, first.BookID); err == nil {
		t.Error("a book with a last chapter set accepted a check for new chapters")
	}
}

func TestChangedMetadataAloneDoesNotRebuild(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, ctx, st, first.BookID)

	src.description = " The site rewrote the blurb."
	again, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Rebuilt {
		t.Error("the book was assembled again though no chapter had arrived")
	}
	if after := stateOf(t, ctx, st, first.BookID); after != before {
		t.Errorf("the file moved without a new chapter: %+v -> %+v", before, after)
	}
}

func TestARebuildIsOnlyDoneWhenAsked(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src.description = " Rewritten."

	asked, err := im.Import(ctx, fakeURL, ImportOptions{Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if !asked.Rebuilt {
		t.Fatal("a rebuild that was asked for did not happen")
	}
	if src.fetched != 3 {
		t.Errorf("the rebuild fetched chapters again: %d in all, want 3", src.fetched)
	}
	var description string
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT description_html FROM source_books WHERE book_id = ?`,
		first.BookID).Scan(&description); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, "Rewritten.") {
		t.Errorf("the rebuilt book still carries the old description: %q", description)
	}

	before := stateOf(t, ctx, st, first.BookID)
	idle, err := im.Import(ctx, fakeURL, ImportOptions{Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if idle.Rebuilt || stateOf(t, ctx, st, first.BookID) != before {
		t.Error("a rebuild was done though nothing on the site had changed")
	}
}

func TestNarrowingTheRangeRebuilds(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 6}
	im, _ := newImporter(t, src)

	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	narrowed, err := im.Import(ctx, fakeURL, ImportOptions{FromChapter: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !narrowed.Rebuilt || narrowed.Chapters != 3 {
		t.Errorf("after narrowing rebuilt=%v with %d chapters, want a rebuild with 3",
			narrowed.Rebuilt, narrowed.Chapters)
	}
	if src.fetched != 6 {
		t.Errorf("narrowing fetched chapters again: %d in all, want 6", src.fetched)
	}
}

func TestDownloadingAgainPicksUpEditedChapters(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, ctx, st, first.BookID)

	same, err := im.Import(ctx, fakeURL, ImportOptions{Redownload: true})
	if err != nil {
		t.Fatal(err)
	}
	if src.fetched != 6 {
		t.Errorf("%d chapters fetched in all, want every one of the 3 fetched twice", src.fetched)
	}
	if same.Rebuilt || stateOf(t, ctx, st, first.BookID) != before {
		t.Error("the book was rebuilt though the chapters came back the same")
	}

	src.edited = " Corrected."
	edited, err := im.Import(ctx, fakeURL, ImportOptions{Redownload: true})
	if err != nil {
		t.Fatal(err)
	}
	if !edited.Rebuilt || edited.Chapters != 3 {
		t.Errorf("after an edit upstream rebuilt=%v with %d chapters, want a rebuild with 3",
			edited.Rebuilt, edited.Chapters)
	}
	if edited.BookID != first.BookID {
		t.Errorf("downloading again landed on a different book: %s -> %s", first.BookID, edited.BookID)
	}
}

func TestAFailedDownloadAgainLeavesTheBookAlone(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := stateOf(t, ctx, st, first.BookID)

	src.broken = true
	if _, err := im.Import(ctx, fakeURL, ImportOptions{Redownload: true}); err == nil {
		t.Fatal("downloading again from a site that is down reported success")
	}
	if stateOf(t, ctx, st, first.BookID) != before {
		t.Error("a failed download again changed the book")
	}

	src.broken = false
	check, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if check.Rebuilt || check.Chapters != 3 || src.fetched != 3 {
		t.Errorf("after the failure a check rebuilt=%v, chapters=%d, fetched=%d; want the old cache back untouched",
			check.Rebuilt, check.Chapters, src.fetched)
	}
}

func TestPicturesAreScaledDownForAReader(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 1, illustrated: true}
	im, st := newImporter(t, src)

	res, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	book, err := store.GetBook(ctx, st.Reader(), res.BookID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.BookFilePath(ctx, st.Reader(), book, "EPUB")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()

	original := int64(len(largePNG()))
	var pictures int
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "img-") {
			continue
		}
		pictures++
		if !strings.HasSuffix(f.Name, ".jpg") {
			t.Errorf("the illustration went in as %s, want a re-encoded .jpg", f.Name)
		}
		if int64(f.UncompressedSize64) >= original/2 {
			t.Errorf("the illustration is %d bytes in the book, the original was %d", f.UncompressedSize64, original)
		}
	}
	if pictures != 1 {
		t.Fatalf("the book carries %d illustrations, want 1", pictures)
	}
}
