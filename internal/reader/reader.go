// Package reader opens an EPUB or KEPUB well enough to leaf through it in a
// browser.
//
// It is deliberately not a good reading experience. It exists to answer one
// question without a Kobo in hand: is this file actually all right — did the
// conversion produce readable chapters, in the right order, with their images
// and stylesheets intact?
package reader

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
)

var (
	ErrNotAnEPUB = errors.New("not an epub: no container.xml")
	ErrNoSpine   = errors.New("epub has no readable chapters")
)

// Book is an opened file. Close it when done.
type Book struct {
	zr    *zip.ReadCloser
	Title string
	Spine []Chapter
}

// Chapter is one spine entry, in reading order.
type Chapter struct {
	Path  string // where it lives inside the zip
	Title string
}

// Open reads the structure of an EPUB. Chapter contents are read on demand, so
// this stays cheap for a big book.
func Open(filename string) (*Book, error) {
	zr, err := zip.OpenReader(filename)
	if err != nil {
		return nil, err
	}

	book := &Book{zr: zr}
	if err := book.load(); err != nil {
		zr.Close()
		return nil, err
	}
	return book, nil
}

func (b *Book) Close() error { return b.zr.Close() }

func (b *Book) load() error {
	opfPath, err := b.rootfile()
	if err != nil {
		return err
	}

	var pkg opfPackage
	if err := b.unmarshal(opfPath, &pkg); err != nil {
		return fmt.Errorf("reading %s: %w", opfPath, err)
	}
	b.Title = strings.TrimSpace(pkg.Metadata.Title)

	// Hrefs in the OPF are relative to the OPF's own directory, which is not
	// necessarily the root of the zip.
	base := path.Dir(opfPath)
	href := map[string]string{}      // manifest id -> zip path
	mediaType := map[string]string{} // zip path -> declared type
	navPath := ""
	for _, item := range pkg.Manifest.Items {
		full := resolve(base, item.Href)
		href[item.ID] = full
		mediaType[full] = item.MediaType
		if strings.Contains(item.Properties, "nav") {
			navPath = full
		}
	}

	titles := b.titles(navPath, href[pkg.Spine.TOC])

	for _, ref := range pkg.Spine.Refs {
		full, ok := href[ref.IDRef]
		if !ok {
			continue
		}
		// Only documents are pages; a stylesheet listed in the spine is a
		// broken book, not a chapter.
		if t := mediaType[full]; t != "" && !isDocument(t) {
			continue
		}
		title := titles[full]
		if title == "" {
			title = fmt.Sprintf("%d", len(b.Spine)+1)
		}
		b.Spine = append(b.Spine, Chapter{Path: full, Title: title})
	}

	if len(b.Spine) == 0 {
		return ErrNoSpine
	}
	return nil
}

// rootfile finds the OPF the way a reader is required to: through
// META-INF/container.xml, never by guessing at a filename.
func (b *Book) rootfile() (string, error) {
	var container struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := b.unmarshal("META-INF/container.xml", &container); err != nil {
		return "", ErrNotAnEPUB
	}
	for _, rf := range container.Rootfiles {
		if rf.FullPath != "" {
			return path.Clean(rf.FullPath), nil
		}
	}
	return "", ErrNotAnEPUB
}

// titles maps a chapter's zip path to its name in the table of contents. Both
// EPUB 3 and EPUB 2 are tried, because Calibre writes both and books from
// elsewhere write either.
func (b *Book) titles(navPath, ncxPath string) map[string]string {
	out := map[string]string{}

	if navPath != "" {
		var doc struct {
			Navs []struct {
				Type  string `xml:"type,attr"`
				Links []struct {
					Href string `xml:"href,attr"`
					Text string `xml:",chardata"`
				} `xml:"ol>li>a"`
			} `xml:"body>nav"`
		}
		if err := b.unmarshal(navPath, &doc); err == nil {
			base := path.Dir(navPath)
			for _, nav := range doc.Navs {
				// An empty type is the common case in the wild; only skip a nav
				// that says it is something other than the table of contents.
				if nav.Type != "" && nav.Type != "toc" {
					continue
				}
				for _, link := range nav.Links {
					record(out, base, link.Href, link.Text)
				}
			}
		}
	}

	if len(out) == 0 && ncxPath != "" {
		var ncx struct {
			Points []struct {
				Text    string `xml:"navLabel>text"`
				Content struct {
					Src string `xml:"src,attr"`
				} `xml:"content"`
			} `xml:"navMap>navPoint"`
		}
		if err := b.unmarshal(ncxPath, &ncx); err == nil {
			base := path.Dir(ncxPath)
			for _, p := range ncx.Points {
				record(out, base, p.Content.Src, p.Text)
			}
		}
	}
	return out
}

func record(out map[string]string, base, href, text string) {
	text = strings.Join(strings.Fields(text), " ")
	if href == "" || text == "" {
		return
	}
	// A table of contents points at anchors inside a chapter; the chapter is
	// what has a title.
	target, _, _ := strings.Cut(href, "#")
	if target == "" {
		return
	}
	full := resolve(base, target)
	if _, taken := out[full]; !taken {
		out[full] = text
	}
}

// Open returns one file from inside the book. The path must name an entry that
// is really there: it arrives from a URL, and a zip is not a filesystem that
// can be trusted to reject nonsense.
func (b *Book) Open(zipPath string) (io.ReadCloser, int64, string, error) {
	clean := path.Clean(strings.TrimPrefix(zipPath, "/"))
	if clean == "." || strings.HasPrefix(clean, "../") {
		return nil, 0, "", fmt.Errorf("no such file in this book: %s", zipPath)
	}

	for _, f := range b.zr.File {
		if path.Clean(f.Name) != clean {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, "", err
		}
		return rc, int64(f.UncompressedSize64), contentType(clean), nil
	}
	return nil, 0, "", fmt.Errorf("no such file in this book: %s", zipPath)
}

// Index is where a chapter sits in reading order, or -1.
func (b *Book) Index(zipPath string) int {
	clean := path.Clean(zipPath)
	for i, c := range b.Spine {
		if c.Path == clean {
			return i
		}
	}
	return -1
}

func resolve(base, href string) string {
	if base == "." || base == "/" || base == "" {
		return path.Clean(href)
	}
	return path.Clean(path.Join(base, href))
}

func isDocument(mediaType string) bool {
	switch mediaType {
	case "application/xhtml+xml", "text/html", "application/x-dtbook+xml":
		return true
	}
	return false
}

// contentType names the type from the extension. The OPF's own declaration is
// not used: a book that mislabels its stylesheet would then render as text.
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".xhtml", ".html", ".htm":
		// Served as HTML rather than XHTML on purpose: browsers refuse to render
		// XHTML that is even slightly malformed, and a book that will not open
		// tells us nothing about whether the conversion worked.
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".otf":
		return "font/otf"
	case ".ttf":
		return "font/ttf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// opfPackage is the part of the OPF this needs: what files exist, and in what
// order they are read.
type opfPackage struct {
	XMLName  xml.Name `xml:"package"`
	Metadata struct {
		Title string `xml:"title"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		TOC  string `xml:"toc,attr"`
		Refs []struct {
			IDRef  string `xml:"idref,attr"`
			Linear string `xml:"linear,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

func (b *Book) unmarshal(name string, v any) error {
	rc, _, _, err := b.Open(name)
	if err != nil {
		return err
	}
	defer rc.Close()

	// A book with a declared encoding other than UTF-8 is rare and not worth a
	// charset table; the decoder is told to pass unknown encodings through.
	dec := xml.NewDecoder(rc)
	dec.Strict = false
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	return dec.Decode(v)
}
