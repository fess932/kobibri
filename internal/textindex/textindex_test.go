package textindex_test

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
	"github.com/fess932/kobibri/internal/textindex"
)

// build writes a minimal EPUB whose content documents carry koboSpan ids, the
// same shape the converter produces and the device reports positions against.
func build(t *testing.T, chapters map[string]string) string {
	t.Helper()

	manifest, spine := "", ""
	for name := range chapters {
		id := "x" + name
		manifest += `<item id="` + id + `" href="` + name + `" media-type="application/xhtml+xml"/>`
		spine += `<itemref idref="` + id + `"/>`
	}

	files := map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.0"?>
			<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
			  <rootfiles><rootfile full-path="OEBPS/content.opf"
			    media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OEBPS/content.opf": `<?xml version="1.0"?>
			<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
			  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Measured</dc:title>
			    <dc:identifier id="id">urn:uuid:1</dc:identifier></metadata>
			  <manifest>` + manifest + `</manifest>
			  <spine>` + spine + `</spine></package>`,
	}
	for name, body := range chapters {
		files["OEBPS/"+name] = body
	}

	path := filepath.Join(t.TempDir(), "book.kepub.epub")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func chapter(spans ...string) string {
	body := ""
	for i, text := range spans {
		body += `<p><span class="koboSpan" id="kobo.` + itoa(i+1) + `.1">` + text + `</span></p>`
	}
	return `<?xml version="1.0" encoding="utf-8"?>
		<html xmlns="http://www.w3.org/1999/xhtml"><head><title>A chapter</title>
		<style>p { margin: 0 }</style></head><body>` + body + `</body></html>`
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var out []byte
	for v > 0 {
		out = append([]byte{byte('0' + v%10)}, out...)
		v /= 10
	}
	return string(out)
}

// Every koboSpan block must know how many words stand before it: that number is
// what turns two reported positions into a distance read.
func TestBlocksCarryTheWordsBeforeThem(t *testing.T) {
	path := build(t, map[string]string{
		"ch1.xhtml": chapter("one two three", "four five", "six"),
	})

	ix, err := textindex.Build(path, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Spanned {
		t.Fatal("the book has koboSpans and was not recorded as spanned")
	}
	if ix.Words != 6 {
		t.Errorf("words = %d, want 6", ix.Words)
	}

	want := []int{0, 3, 5}
	if len(ix.Blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(ix.Blocks), len(want))
	}
	for i, b := range ix.Blocks {
		if b.Block != i+1 || b.Before != want[i] {
			t.Errorf("block %d: id %d, %d words before; want id %d, %d before",
				i, b.Block, b.Before, i+1, want[i])
		}
	}
}

// Offsets are book-wide, not per file: a position in the second chapter has to
// sort after every position in the first.
func TestOffsetsRunAcrossTheWholeBook(t *testing.T) {
	path := build(t, map[string]string{
		"a.xhtml": chapter("one two three four"),
		"b.xhtml": chapter("five six"),
	})

	ix, err := textindex.Build(path, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if ix.Words != 6 {
		t.Fatalf("words = %d, want 6", ix.Words)
	}
	if len(ix.Docs) != 2 {
		t.Fatalf("documents = %d, want 2", len(ix.Docs))
	}

	prev := -1
	for _, d := range ix.Docs {
		if d.Before <= prev {
			t.Fatalf("document %q starts at %d, not after the previous one", d.Source, d.Before)
		}
		prev = d.Before
	}
	if ix.Docs[1].Before != 4 {
		t.Errorf("second document starts at word %d, want 4", ix.Docs[1].Before)
	}
}

// The title and the stylesheet are not part of what anyone reads. Counting the
// title as body text is a fault the converter's own specification caught once
// already.
func TestTheHeadIsNotCounted(t *testing.T) {
	path := build(t, map[string]string{"ch1.xhtml": chapter("one two")})

	ix, err := textindex.Build(path, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if ix.Words != 2 {
		t.Errorf("words = %d, want 2 — the head leaked into the count", ix.Words)
	}
}

func TestWordsAreCounted(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"one", 1},
		{" one  two\nthree\t", 3},
		{"don't stop", 2},
		{"«a quoted sentence.» Then another.", 5},
		// Written without spaces, so a paragraph would otherwise be one word
		// and a reading speed three words a minute.
		{"日本語の文", 5},
		{"mixed 日本 text", 4},
	}
	for _, tt := range tests {
		if got := textindex.CountWords(tt.in); got != tt.want {
			t.Errorf("CountWords(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// The numbers this package reads are the ones the converter writes, and the
// device stores its position against them. A fixture with hand-written spans
// proves nothing about that pairing; a real conversion does.
func TestAConvertedBookIsMeasurable(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cache, err := kepubconv.NewCache(kepubconv.Options{Dir: filepath.Join(dir, "kepub"), Store: st})
	if err != nil {
		t.Fatal(err)
	}

	lib := calibretest.New(t, calibretest.BookSpec{
		Title:   "Convertible",
		Authors: []string{"Jane Author"},
		Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: "reflowable"}},
	})
	var src string
	filepath.Walk(lib.Path, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".epub") {
			src = path
		}
		return nil
	})
	if src == "" {
		t.Fatal("fixture produced no .epub file")
	}

	converted, _, err := cache.Path(t.Context(), "book-1", src)
	if err != nil {
		t.Fatal(err)
	}

	ix, err := textindex.Build(converted, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if !ix.Spanned {
		t.Fatal("a converted book produced no koboSpan blocks to measure against")
	}
	if ix.Words == 0 {
		t.Fatal("a converted book measured as empty")
	}

	prev := -1
	for _, b := range ix.Blocks {
		if b.Before < prev {
			t.Fatalf("block %d of %q goes backwards: %d after %d", b.Block, b.Source, b.Before, prev)
		}
		prev = b.Before
	}
}
