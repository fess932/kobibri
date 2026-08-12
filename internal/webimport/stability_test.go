package webimport

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// entries reads a zip into name -> content hash.
func entries(t *testing.T, path string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer zr.Close()

	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, rc); err != nil {
			rc.Close()
			t.Fatal(err)
		}
		rc.Close()
		out[f.Name] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

// TestAppendingChaptersLeavesEarlierOnesAlone is the question behind updating a
// serial in place rather than as a new book.
//
// A Kobo stores a reading position as a koboSpan id inside a named content
// document. So a position survives new chapters only if the earlier chapters
// keep both their filenames and their contents, byte for byte, when the book is
// assembled again. This measures exactly that.
func TestAppendingChaptersLeavesEarlierOnesAlone(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := epubOf(t, ctx, st, first.BookID)
	beforeEntries := entries(t, before)

	// Keep a copy: the next import overwrites the file in place.
	kept := filepath.Join(t.TempDir(), "before.epub")
	copyFile(t, before, kept)
	beforeEntries = entries(t, kept)

	src.chapters = 5
	second, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	afterEntries := entries(t, epubOf(t, ctx, st, second.BookID))

	var (
		changed []string
		missing []string
		content int
	)
	for name, hash := range beforeEntries {
		if !isContentDocument(name) {
			continue
		}
		content++
		got, ok := afterEntries[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case got != hash:
			changed = append(changed, name)
		}
	}

	if content == 0 {
		t.Fatalf("no content documents in the assembled book; entries: %v", keys(beforeEntries))
	}
	t.Logf("content documents before: %d, after: %d", content, countContent(afterEntries))

	if len(missing) > 0 {
		t.Errorf("chapters that existed before were renamed or dropped: %v", missing)
	}
	if len(changed) > 0 {
		t.Errorf("chapters that existed before changed content: %v", changed)
	}
	if len(missing) == 0 && len(changed) == 0 {
		t.Log("earlier chapters are untouched, so a reading position inside one survives")
	}

	// The table of contents is expected to change; say so explicitly, so that a
	// future reader of this test does not think it was overlooked.
	for name := range beforeEntries {
		if isNavigation(name) && afterEntries[name] == beforeEntries[name] {
			t.Logf("note: %s did not change, though it would have been fine if it had", name)
		}
	}
}

// TestKepubSpanIDsSurviveNewChapters goes one step further: the position is an
// id assigned by the converter, so what has to hold is that converting the
// longer book leaves the earlier chapters' span ids exactly as they were.
func TestKepubSpanIDsSurviveNewChapters(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	cache, err := kepubconv.NewCache(kepubconv.Options{
		Dir: filepath.Join(t.TempDir(), "kepub"), Store: st,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	firstKepub, _, err := cache.Path(ctx, first.BookID, epubOf(t, ctx, st, first.BookID))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	beforeSpans := spansByDocument(t, firstKepub)

	src.chapters = 5
	second, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secondKepub, _, err := cache.Path(ctx, second.BookID, epubOf(t, ctx, st, second.BookID))
	if err != nil {
		t.Fatalf("convert again: %v", err)
	}
	afterSpans := spansByDocument(t, secondKepub)

	if len(beforeSpans) == 0 {
		t.Fatal("the converted book has no kobo spans at all")
	}
	if firstKepub == secondKepub {
		t.Fatal("the longer book reused the cached conversion; the fingerprint did not change")
	}

	for doc, ids := range beforeSpans {
		got, ok := afterSpans[doc]
		if !ok {
			t.Errorf("chapter %s is gone from the longer book, so any position in it is lost", doc)
			continue
		}
		if strings.Join(got, ",") != strings.Join(ids, ",") {
			t.Errorf("chapter %s has different span ids after new chapters were added:\n before %v\n after  %v",
				doc, ids, got)
		}
	}
}

// spansByDocument returns the koboSpan ids of each content document, in order.
func spansByDocument(t *testing.T, path string) map[string][]string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	out := map[string][]string{}
	for _, f := range zr.File {
		if !isContentDocument(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		buf, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = spanIDs(string(buf))
	}
	return out
}

func spanIDs(body string) []string {
	var out []string
	const marker = `id="kobo.`
	for {
		i := strings.Index(body, marker)
		if i < 0 {
			return out
		}
		body = body[i+len(marker):]
		end := strings.IndexByte(body, '"')
		if end < 0 {
			return out
		}
		out = append(out, "kobo."+body[:end])
	}
}

func isContentDocument(name string) bool {
	if !strings.HasSuffix(name, ".xhtml") && !strings.HasSuffix(name, ".html") {
		return false
	}
	// The table of contents lists the chapters, so of course it changes when
	// chapters are added. It is not somewhere a reader has a position.
	return !isNavigation(name)
}

func isNavigation(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return base == "nav.xhtml" || base == "toc.xhtml" || base == "nav.html"
}

func countContent(m map[string]string) int {
	n := 0
	for name := range m {
		if isContentDocument(name) {
			n++
		}
	}
	return n
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func epubOf(t *testing.T, ctx context.Context, st *store.Store, bookID string) string {
	t.Helper()
	book, err := store.GetBook(ctx, st.Reader(), bookID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.BookFilePath(ctx, st.Reader(), book, "EPUB")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}
