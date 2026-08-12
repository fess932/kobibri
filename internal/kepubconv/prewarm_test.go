package kepubconv_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// prewarmEnv wires a real Calibre source into a real store, so the prewarmer is
// exercised against the same query the server uses.
func prewarmEnv(t *testing.T, books ...calibretest.BookSpec) (*kepubconv.Prewarmer, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cache, err := kepubconv.NewCache(kepubconv.Options{Dir: filepath.Join(dir, "kepub"), Store: st})
	if err != nil {
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

	return kepubconv.NewPrewarmer(cache, st, nil), st, ctx
}

func countCached(t *testing.T, st *store.Store, ctx context.Context) int {
	t.Helper()
	var n int
	if err := st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kepub_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every imported book must end up converted, so the web UI can offer the file
// and no device ever waits on a conversion mid-sync.
func TestPrewarmConvertsImportedBooks(t *testing.T) {
	p, st, ctx := prewarmEnv(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)

	if n := countCached(t, st, ctx); n != 0 {
		t.Fatalf("%d books cached before prewarming, want 0", n)
	}

	converted, err := p.Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if converted != 2 {
		t.Errorf("converted %d books, want 2", converted)
	}
	if n := countCached(t, st, ctx); n != 2 {
		t.Errorf("%d cache entries, want 2", n)
	}
}

// A second pass must be a no-op rather than reconverting the library.
func TestPrewarmIsIdempotent(t *testing.T) {
	p, st, ctx := prewarmEnv(t, calibretest.BookSpec{Title: "One"})

	if _, err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	converted, err := p.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if converted != 0 {
		t.Errorf("second pass converted %d books, want 0", converted)
	}
	if n := countCached(t, st, ctx); n != 1 {
		t.Errorf("%d cache entries after two passes, want 1", n)
	}
}

// Books that cannot be synced to a Kobo at all must not be queued, and a
// fixed-layout book must not be converted: it is served as it is.
func TestPrewarmSkipsBooksThatAreNotConverted(t *testing.T) {
	p, st, ctx := prewarmEnv(t,
		calibretest.BookSpec{Title: "Reflowable"},
		calibretest.BookSpec{Title: "Fixed Art",
			Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: "pre-paginated"}}},
		calibretest.BookSpec{Title: "Pdf Only",
			Formats: []calibretest.FormatSpec{{Format: "PDF"}}},
	)

	converted, err := p.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if converted != 1 {
		t.Errorf("converted %d books, want only the reflowable one", converted)
	}
	if n := countCached(t, st, ctx); n != 1 {
		t.Errorf("%d cache entries, want 1", n)
	}
}

// A book that cannot be converted must be recorded as failed and then left
// alone, not retried on every pass.
func TestPrewarmDoesNotRetryFailures(t *testing.T) {
	p, st, ctx := prewarmEnv(t, calibretest.BookSpec{
		Title:   "Corrupt",
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: "broken"}},
	})

	if _, err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}

	var failures int
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM kepub_failures`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Errorf("%d recorded failures, want 1", failures)
	}

	// The second pass must not touch it again.
	if converted, err := p.Pass(ctx); err != nil || converted != 0 {
		t.Errorf("second pass converted %d books (err %v), want 0", converted, err)
	}
}
