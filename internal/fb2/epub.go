package fb2

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"
)

// Writing the EPUB.
//
// EPUB 3, because that is what a Kobo reads best and what the KEPUB conversion
// after this expects. The parts are few: a mimetype that must come first and
// uncompressed, a container pointing at the package document, the package
// document itself, a navigation document, and the chapters.

const stylesheet = `body { margin: 0 5%; line-height: 1.4; text-align: justify; }
h1, h2, h3, h4 { text-align: left; line-height: 1.2; }
p { margin: 0; text-indent: 1.2em; }
p.empty-line { text-indent: 0; }
.image { text-align: center; margin: 1em 0; text-indent: 0; }
.image img { max-width: 100%; }
blockquote.epigraph { margin: 1em 2em; font-style: italic; }
blockquote.cite { margin: 1em 2em; }
p.text-author { text-align: right; font-style: italic; text-indent: 0; }
div.poem { margin: 1em 2em; }
div.stanza { margin-bottom: 1em; }
p.verse { text-indent: 0; }
table { border-collapse: collapse; margin: 1em 0; }
td, th { border: 1px solid #999; padding: 0.3em 0.5em; }
`

// WriteEPUB writes the whole book.
func (b *Book) WriteEPUB(w io.Writer) error {
	zw := zip.NewWriter(w)

	if err := writeZipFile(zw, "mimetype", []byte("application/epub+zip")); err != nil {
		return err
	}
	if err := writeZipFile(zw, "META-INF/container.xml", []byte(containerXML)); err != nil {
		return err
	}
	if err := writeZipFile(zw, "OEBPS/style.css", []byte(stylesheet)); err != nil {
		return err
	}

	for i, c := range b.Chapters {
		name := chapterFile(i)
		if err := writeZipFile(zw, "OEBPS/"+name, []byte(b.chapterXHTML(c))); err != nil {
			return err
		}
	}
	for _, img := range b.Images {
		if err := writeZipFile(zw, "OEBPS/images/"+img.Name, img.Data); err != nil {
			return err
		}
	}

	if err := writeZipFile(zw, "OEBPS/nav.xhtml", []byte(b.navXHTML())); err != nil {
		return err
	}
	if err := writeZipFile(zw, "OEBPS/content.opf", []byte(b.packageXML())); err != nil {
		return err
	}
	return zw.Close()
}

// chapterFile names a chapter. Notes keep a name of their own because footnote
// links point at it by name.
func chapterFile(i int) string {
	if i == 0 {
		return "notes.xhtml"
	}
	return fmt.Sprintf("ch%03d.xhtml", i)
}

const containerXML = `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

func (b *Book) chapterXHTML(c Chapter) string {
	title := c.Title
	if title == "" {
		title = b.Title
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="` + escapeAttr(b.Language) + `">
<head><title>` + escape(title) + `</title>
<link rel="stylesheet" type="text/css" href="style.css"/></head>
<body>` + c.HTML + `</body></html>`
}

func (b *Book) navXHTML() string {
	var items strings.Builder
	for i, c := range b.Chapters {
		title := c.Title
		if title == "" {
			title = fmt.Sprintf("%d", i+1)
		}
		fmt.Fprintf(&items, `<li><a href="%s">%s</a></li>`, chapterFile(i), escape(title))
	}

	return `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"
      xml:lang="` + escapeAttr(b.Language) + `">
<head><title>` + escape(b.Title) + `</title></head>
<body><nav epub:type="toc" id="toc"><h1>` + escape(b.Title) + `</h1>
<ol>` + items.String() + `</ol></nav></body></html>`
}

func (b *Book) packageXML() string {
	var meta, manifest, spine strings.Builder

	fmt.Fprintf(&meta, `<dc:title>%s</dc:title>`, escape(b.Title))
	fmt.Fprintf(&meta, `<dc:language>%s</dc:language>`, escape(b.Language))
	fmt.Fprintf(&meta, `<dc:identifier id="pub-id">urn:uuid:%s</dc:identifier>`, escape(b.identifier()))
	for _, a := range b.Authors {
		fmt.Fprintf(&meta, `<dc:creator>%s</dc:creator>`, escape(a))
	}
	if b.Publisher != "" {
		fmt.Fprintf(&meta, `<dc:publisher>%s</dc:publisher>`, escape(b.Publisher))
	}
	if b.Annotation != "" {
		fmt.Fprintf(&meta, `<dc:description>%s</dc:description>`, escape(b.Annotation))
	}
	if b.SeriesName != "" {
		// Calibre's own convention, which is what reads a series back out again.
		fmt.Fprintf(&meta, `<meta name="calibre:series" content="%s"/>`, escapeAttr(b.SeriesName))
		if b.SeriesIndex != "" {
			fmt.Fprintf(&meta, `<meta name="calibre:series_index" content="%s"/>`,
				escapeAttr(b.SeriesIndex))
		}
	}
	// EPUB 3 requires a modification date.
	meta.WriteString(`<meta property="dcterms:modified">2000-01-01T00:00:00Z</meta>`)

	manifest.WriteString(`<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>`)
	manifest.WriteString(`<item id="css" href="style.css" media-type="text/css"/>`)

	for i := range b.Chapters {
		id := fmt.Sprintf("ch%d", i)
		fmt.Fprintf(&manifest, `<item id="%s" href="%s" media-type="application/xhtml+xml"/>`,
			id, chapterFile(i))
		fmt.Fprintf(&spine, `<itemref idref="%s"/>`, id)
	}

	coverName := imageRef(b.CoverID)
	for i, img := range b.Images {
		properties := ""
		if b.CoverID != "" && img.Name == coverName {
			properties = ` properties="cover-image"`
			// The cover is also named the old way, which is what most readers
			// and every converter actually look at.
			fmt.Fprintf(&meta, `<meta name="cover" content="img%d"/>`, i)
		}
		fmt.Fprintf(&manifest, `<item id="img%d" href="images/%s" media-type="%s"%s/>`,
			i, escapeAttr(img.Name), escapeAttr(mediaType(img)), properties)
	}

	return `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="pub-id">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">` + meta.String() + `</metadata>
<manifest>` + manifest.String() + `</manifest>
<spine>` + spine.String() + `</spine>
</package>`
}

// identifier is derived from the book rather than random, so converting the same
// file twice produces the same book instead of a stranger.
func (b *Book) identifier() string {
	seed := b.Title + "|" + strings.Join(b.Authors, ",") + "|" + b.ISBN
	sum := fnv1a(seed)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum&0xffffffff, (sum>>8)&0xffff, (sum>>16)&0xffff, (sum>>24)&0xffff, sum&0xffffffffffff)
}

func fnv1a(s string) uint64 {
	const offset, prime = 14695981039346656037, 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func mediaType(img Image) string {
	if t := strings.TrimSpace(img.Type); t != "" {
		return t
	}
	switch {
	case strings.HasSuffix(img.Name, ".png"):
		return "image/png"
	case strings.HasSuffix(img.Name, ".gif"):
		return "image/gif"
	case strings.HasSuffix(img.Name, ".svg"):
		return "image/svg+xml"
	}
	return "image/jpeg"
}
