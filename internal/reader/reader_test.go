package reader_test

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/reader"
)

// build writes an EPUB from a map of zip path to contents.
func build(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "book.epub")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// epub3 is the shape novelkit and most converters produce: the OPF in a
// subfolder, a navigation document, chapters beside it.
func epub3(t *testing.T) string {
	return build(t, map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.0"?>
			<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
			  <rootfiles><rootfile full-path="OEBPS/content.opf"
			    media-type="application/oebps-package+xml"/></rootfiles>
			</container>`,
		"OEBPS/content.opf": `<?xml version="1.0"?>
			<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
			  <metadata><dc:title xmlns:dc="http://purl.org/dc/elements/1.1/">A Book</dc:title></metadata>
			  <manifest>
			    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
			    <item id="c1" href="text/one.xhtml" media-type="application/xhtml+xml"/>
			    <item id="c2" href="text/two.xhtml" media-type="application/xhtml+xml"/>
			    <item id="css" href="styles/main.css" media-type="text/css"/>
			  </manifest>
			  <spine><itemref idref="c1"/><itemref idref="c2"/></spine>
			</package>`,
		"OEBPS/nav.xhtml": `<html xmlns:epub="http://www.idpf.org/2007/ops"><body>
			  <nav epub:type="toc"><ol>
			    <li><a href="text/one.xhtml">The Beginning</a></li>
			    <li><a href="text/two.xhtml#top">What Followed</a></li>
			  </ol></nav></body></html>`,
		"OEBPS/text/one.xhtml":  `<html><body><p>one</p></body></html>`,
		"OEBPS/text/two.xhtml":  `<html><body><p>two</p></body></html>`,
		"OEBPS/styles/main.css": `p { margin: 1em; }`,
	})
}

func open(t *testing.T, path string) *reader.Book {
	t.Helper()
	b, err := reader.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// Reading order is the spine's, and it has to survive the OPF living in a
// subfolder: every href in there is relative to the OPF, not to the zip root.
func TestSpineIsReadInOrderAndResolvedAgainstTheOPF(t *testing.T) {
	b := open(t, epub3(t))

	if b.Title != "A Book" {
		t.Errorf("Title = %q, want %q", b.Title, "A Book")
	}
	if len(b.Spine) != 2 {
		t.Fatalf("%d chapters, want 2", len(b.Spine))
	}
	if b.Spine[0].Path != "OEBPS/text/one.xhtml" {
		t.Errorf("first chapter is %q, want it resolved against the OPF's folder", b.Spine[0].Path)
	}
	if b.Spine[1].Path != "OEBPS/text/two.xhtml" {
		t.Errorf("second chapter is %q", b.Spine[1].Path)
	}
}

// Titles come from the table of contents, including when it points at an anchor
// inside the chapter rather than at the chapter itself.
func TestTitlesComeFromTheNavigationDocument(t *testing.T) {
	b := open(t, epub3(t))

	if b.Spine[0].Title != "The Beginning" {
		t.Errorf("first title = %q", b.Spine[0].Title)
	}
	if b.Spine[1].Title != "What Followed" {
		t.Errorf("second title = %q — an anchored link was not matched to its chapter",
			b.Spine[1].Title)
	}
}

// An EPUB 2 book has no navigation document, only an NCX. Calibre writes plenty
// of them.
func TestTitlesFallBackToTheNCX(t *testing.T) {
	path := build(t, map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf"/></rootfiles></container>`,
		"content.opf": `<package>
			  <metadata><title>Old Book</title></metadata>
			  <manifest>
			    <item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
			    <item id="c1" href="chapter1.html" media-type="application/xhtml+xml"/>
			  </manifest>
			  <spine toc="ncx"><itemref idref="c1"/></spine>
			</package>`,
		"toc.ncx": `<ncx><navMap><navPoint>
			  <navLabel><text>Chapter the First</text></navLabel>
			  <content src="chapter1.html"/>
			</navPoint></navMap></ncx>`,
		"chapter1.html": `<html><body>text</body></html>`,
	})

	b := open(t, path)
	if len(b.Spine) != 1 {
		t.Fatalf("%d chapters, want 1", len(b.Spine))
	}
	if b.Spine[0].Title != "Chapter the First" {
		t.Errorf("title = %q, want it from the NCX", b.Spine[0].Title)
	}
}

// A book with no table of contents still has to be readable: the pages are
// numbered rather than named.
func TestChaptersWithoutTitlesAreNumbered(t *testing.T) {
	path := build(t, map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf"/></rootfiles></container>`,
		"content.opf": `<package><manifest>
			    <item id="c1" href="a.xhtml" media-type="application/xhtml+xml"/>
			    <item id="c2" href="b.xhtml" media-type="application/xhtml+xml"/>
			  </manifest><spine><itemref idref="c1"/><itemref idref="c2"/></spine></package>`,
		"a.xhtml": `<html/>`,
		"b.xhtml": `<html/>`,
	})

	b := open(t, path)
	if b.Spine[0].Title != "1" || b.Spine[1].Title != "2" {
		t.Errorf("titles are %q and %q, want them numbered", b.Spine[0].Title, b.Spine[1].Title)
	}
}

// The path comes from a URL, so a walk out of the zip must be refused rather
// than reaching the filesystem.
func TestAPathCannotEscapeTheBook(t *testing.T) {
	b := open(t, epub3(t))

	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"OEBPS/../../secret",
		"OEBPS/text/nothing.xhtml",
	} {
		if _, _, _, err := b.Open(bad); err == nil {
			t.Errorf("%q was served", bad)
		}
	}
}

// A stylesheet has to arrive as a stylesheet, or the chapter renders unstyled
// and the whole point of looking at it is lost.
func TestContentTypeComesFromTheExtension(t *testing.T) {
	b := open(t, epub3(t))

	tests := map[string]string{
		"OEBPS/styles/main.css":  "text/css; charset=utf-8",
		"OEBPS/text/one.xhtml":   "text/html; charset=utf-8",
		"META-INF/container.xml": "text/xml; charset=utf-8",
	}
	for name, want := range tests {
		rc, size, got, err := b.Open(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		_ = rc.Close()
		if name == "OEBPS/styles/main.css" && got != want {
			t.Errorf("%s: content type = %q, want %q", name, got, want)
		}
		if size == 0 {
			t.Errorf("%s: size is 0", name)
		}
	}
}

// A file that is not an EPUB has to say so rather than producing an empty book.
func TestSomethingElseIsRejected(t *testing.T) {
	path := build(t, map[string]string{"hello.txt": "not a book"})
	if _, err := reader.Open(path); err == nil {
		t.Error("a zip that is not an EPUB was opened as one")
	}

	notZip := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(notZip, []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Open(notZip); err == nil {
		t.Error("a file that is not a zip was opened")
	}
}

// A stylesheet listed in the spine is a broken book, not a page.
func TestOnlyDocumentsBecomePages(t *testing.T) {
	path := build(t, map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf"/></rootfiles></container>`,
		"content.opf": `<package><manifest>
			    <item id="c1" href="a.xhtml" media-type="application/xhtml+xml"/>
			    <item id="css" href="a.css" media-type="text/css"/>
			  </manifest><spine><itemref idref="c1"/><itemref idref="css"/></spine></package>`,
		"a.xhtml": `<html/>`,
		"a.css":   `p{}`,
	})

	b := open(t, path)
	if len(b.Spine) != 1 {
		t.Errorf("%d pages, want 1 — a stylesheet was treated as a chapter", len(b.Spine))
	}
}
