package kepubconv_test

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

func newCache(t *testing.T) (*kepubconv.Cache, *store.Store) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(context.Background(), filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cache, err := kepubconv.NewCache(kepubconv.Options{
		Dir: filepath.Join(dir, "kepub"), Store: st,
	})
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	return cache, st
}

// fixtureEPUB writes one of calibretest's generated EPUBs to disk and returns
// its path.
func fixtureEPUB(t *testing.T, kind string) string {
	t.Helper()
	lib := calibretest.New(t, calibretest.BookSpec{
		Title:   "Convertible",
		Authors: []string{"Jane Author"},
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: kind}},
	})

	var found string
	filepath.Walk(lib.Path, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".epub") {
			found = path
		}
		return nil
	})
	if found == "" {
		t.Fatal("fixture produced no .epub file")
	}
	return found
}

// zipEntries reads a converted file back as a zip.
func zipEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("converted file is not a valid zip: %v", err)
	}
	defer zr.Close()

	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening %s: %v", f.Name, err)
		}
		buf, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(buf)
	}
	return out
}

// The whole point of converting: Kobo's reader anchors reading position on
// koboSpan ids, and they only exist in a converted file.
func TestConversionProducesKoboSpans(t *testing.T) {
	cache, _ := newCache(t)
	src := fixtureEPUB(t, "reflowable")

	path, size, err := cache.Path(context.Background(), "book-1", src)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if size == 0 {
		t.Fatal("converted file is empty")
	}

	entries := zipEntries(t, path)

	var content string
	for name, body := range entries {
		if strings.HasSuffix(name, ".xhtml") || strings.HasSuffix(name, ".html") {
			content += body
		}
	}
	if content == "" {
		t.Fatalf("converted file has no content documents; entries: %v", keys(entries))
	}
	if !strings.Contains(content, `class="koboSpan"`) {
		t.Errorf("converted content has no koboSpan elements, so reading progress "+
			"cannot be tracked mid-chapter:\n%s", truncate(content, 600))
	}
	if !strings.Contains(content, `id="kobo.`) {
		t.Error("koboSpan elements carry no kobo.N.M ids")
	}
	if _, ok := entries["mimetype"]; !ok {
		t.Error("converted file has no mimetype entry; it is not a valid EPUB")
	}
}

// The extension is load-bearing: Kobo only uses its KEPUB renderer for files
// named *.kepub.epub.
func TestCachePathKeepsTheKepubSuffix(t *testing.T) {
	cache, _ := newCache(t)
	src := fixtureEPUB(t, "reflowable")

	path, _, err := cache.Path(context.Background(), "book-1", src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, kepubconv.KepubSuffix) {
		t.Errorf("cached path %q does not end in %s", path, kepubconv.KepubSuffix)
	}
}

// A second request must reuse the converted file rather than convert again.
func TestConversionIsCached(t *testing.T) {
	cache, st := newCache(t)
	src := fixtureEPUB(t, "reflowable")
	ctx := context.Background()

	first, _, err := cache.Path(ctx, "book-1", src)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	second, _, err := cache.Path(ctx, "book-1", src)
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

	var n int
	if err := st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kepub_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("kepub_cache has %d rows, want 1", n)
	}
}

// A source file replaced in place must invalidate the cache, even if its
// timestamp was preserved — which rsync and some sync tools do.
func TestCacheInvalidatesWhenTheSourceChanges(t *testing.T) {
	cache, _ := newCache(t)
	ctx := context.Background()

	src := fixtureEPUB(t, "reflowable")
	first, _, err := cache.Path(ctx, "book-1", src)
	if err != nil {
		t.Fatal(err)
	}

	// Swap in different content, restoring the original modification time.
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	replacement := fixtureEPUB(t, "epub2")
	buf, err := os.ReadFile(replacement)
	if err != nil {
		t.Fatal(err)
	}
	buf = append(buf, make([]byte, 64)...) // ensure the size differs
	if err := os.WriteFile(src, buf[:len(buf)-64], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	second, _, err := cache.Path(ctx, "book-1", src)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Error("the cache was reused after the source file was replaced")
	}
}

// A device commonly starts several downloads at once; they must converge on one
// conversion rather than racing.
func TestConcurrentRequestsConvertOnce(t *testing.T) {
	cache, st := newCache(t)
	src := fixtureEPUB(t, "reflowable")
	ctx := context.Background()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		paths []string
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, _, err := cache.Path(ctx, "book-1", src)
			if err != nil {
				t.Errorf("Path: %v", err)
				return
			}
			mu.Lock()
			paths = append(paths, p)
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, p := range paths {
		if p != paths[0] {
			t.Fatalf("concurrent calls disagreed: %q vs %q", p, paths[0])
		}
	}
	var n int
	if err := st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kepub_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("kepub_cache has %d rows after 8 concurrent requests, want 1", n)
	}
}

// A broken EPUB must fail cleanly and be remembered, so the caller can serve the
// original file without retrying the conversion on every request.
func TestBrokenEPUBFailsAndIsRemembered(t *testing.T) {
	cache, _ := newCache(t)
	src := fixtureEPUB(t, "broken")
	ctx := context.Background()

	if _, _, err := cache.Path(ctx, "book-1", src); err == nil {
		t.Fatal("converting a file that is not a zip succeeded")
	}
	if !cache.Failed(ctx, "book-1", src) {
		t.Error("the failure was not recorded")
	}

	// No half-written file may be left behind, or it would look cached.
	matches, _ := filepath.Glob(filepath.Join(t.TempDir(), "**", "*"+kepubconv.KepubSuffix))
	if len(matches) > 0 {
		t.Errorf("a partial file was left behind: %v", matches)
	}
}

// Eviction must trim to the budget, keeping the most recently used.
func TestEvictTrimsToBudget(t *testing.T) {
	cache, st := newCache(t)
	ctx := context.Background()

	src := fixtureEPUB(t, "reflowable")
	for _, id := range []string{"book-1", "book-2", "book-3"} {
		if _, _, err := cache.Path(ctx, id, src); err != nil {
			t.Fatal(err)
		}
	}

	var total int64
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT COALESCE(sum(size), 0) FROM kepub_cache`).Scan(&total); err != nil {
		t.Fatal(err)
	}

	// A budget that fits roughly one entry.
	if err := cache.Evict(ctx, total/3); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	var remaining int
	if err := st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kepub_cache`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining >= 3 {
		t.Errorf("%d entries survived eviction to a one-entry budget", remaining)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
