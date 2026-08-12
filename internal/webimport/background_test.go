package webimport

import (
	"context"
	"testing"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

// Two imports of the same book must never run at once: they would write the
// same cache directory and the same assembled file.
func TestOneImportPerBookAtATime(t *testing.T) {
	im, _ := newImporter(t, &fakeSource{chapters: 2})

	if !im.runner.acquire(fakeURL, "") {
		t.Fatal("could not claim a book nobody is downloading")
	}
	if im.runner.acquire(fakeURL, "") {
		t.Error("claimed a book that is already being downloaded")
	}

	// A different translation of the same title is a different download.
	if !im.runner.acquire(fakeURL, "other-edition") {
		t.Error("a second translation was refused, though it is a separate download")
	}

	// Once it finishes, the book can be claimed again.
	im.runner.finish(fakeURL, "", Result{Title: "A Serial Story", Chapters: 2}, nil)
	if !im.runner.acquire(fakeURL, "") {
		t.Error("could not claim the book again after the first import finished")
	}
}

// Start refuses a second run rather than beginning one.
func TestStartRefusesADuplicate(t *testing.T) {
	ctx := context.Background()
	im, _ := newImporter(t, &fakeSource{chapters: 2})

	if !im.runner.acquire(fakeURL, "") {
		t.Fatal("setup: could not claim")
	}
	if im.Start(ctx, fakeURL, ImportOptions{}) {
		t.Error("Start began a second download of a book already in progress")
	}
}

// A finished import is reported, then forgotten; a failed one stays visible.
func TestRunningReportsOutcomes(t *testing.T) {
	im, _ := newImporter(t, &fakeSource{chapters: 1})

	im.runner.acquire(fakeURL, "")
	im.runner.finish(fakeURL, "", Result{Title: "Done", Chapters: 4}, nil)

	running := im.Running()
	if len(running) != 1 {
		t.Fatalf("got %d statuses, want 1", len(running))
	}
	if !running[0].Finished || running[0].Err != "" {
		t.Errorf("status = %+v, want a clean finish", running[0])
	}
	if im.Busy() {
		t.Error("Busy is true although nothing is downloading")
	}

	// Age it past the window and it clears itself away.
	im.runner.mu.Lock()
	im.runner.running[key(fakeURL, "")].StartedAt = time.Now().Add(-2 * time.Minute)
	im.runner.mu.Unlock()

	if got := im.Running(); len(got) != 0 {
		t.Errorf("a finished import is still listed after its window: %+v", got)
	}
}

// A failure is kept on screen until the book is tried again, because nobody is
// watching the moment it happens.
func TestFailedImportStaysVisible(t *testing.T) {
	im, _ := newImporter(t, &fakeSource{chapters: 1})

	im.runner.acquire(fakeURL, "")
	im.runner.finish(fakeURL, "", Result{}, errAlreadyRunning)

	im.runner.mu.Lock()
	im.runner.running[key(fakeURL, "")].StartedAt = time.Now().Add(-time.Hour)
	im.runner.mu.Unlock()

	running := im.Running()
	if len(running) != 1 || running[0].Err == "" {
		t.Errorf("a failed import was forgotten: %+v", running)
	}
}

// The periodic check has to pick up chapters published since the import.
func TestRefreshAllPicksUpNewChapters(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 2}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	src.chapters = 6
	im.RefreshAll(ctx)

	var total int
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT chapters_total FROM web_imports`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 6 {
		t.Errorf("after the periodic check the book has %d chapters, want 6", total)
	}

	// Still one book, and its revision moved so a reader is told to refetch.
	book, err := store.GetBook(ctx, st.Reader(), first.BookID)
	if err != nil {
		t.Fatal(err)
	}
	if book.MetadataRev < 2 {
		t.Errorf("metadata_rev = %d, want it to have moved", book.MetadataRev)
	}
}

// A cancelled context stops the sweep rather than working through every book.
func TestRefreshAllStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &fakeSource{chapters: 2}
	im, _ := newImporter(t, src)

	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatal(err)
	}
	fetched := src.fetched

	cancel()
	src.chapters = 20
	im.RefreshAll(ctx)

	if src.fetched != fetched {
		t.Errorf("a cancelled sweep still downloaded %d chapters", src.fetched-fetched)
	}
}

// The timer form must return promptly when the context ends, and must do
// nothing at all when the check is switched off.
func TestPeriodicRefreshRespectsItsSettings(t *testing.T) {
	im, _ := newImporter(t, &fakeSource{chapters: 1})

	done := make(chan struct{})
	go func() { im.RunPeriodicRefresh(context.Background(), 0); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPeriodicRefresh did not return when switched off")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { im.RunPeriodicRefresh(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPeriodicRefresh did not return when its context ended")
	}
}
