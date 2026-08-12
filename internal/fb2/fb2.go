// Package fb2 turns an FB2 book into an EPUB.
//
// FB2 is the format Russian libraries are full of, and until now it needed
// Calibre installed to be readable on a Kobo at all — which put a desktop
// application between this server and half of some people's shelves. It does not
// need one: an FB2 is a single XML file with its pictures inlined as base64, and
// everything an EPUB needs is already in there.
//
// The other formats are a different matter. AZW3, MOBI and LIT are compressed
// binary containers, and doing them properly is a project of its own; those
// still go through Calibre when it happens to be installed. What this removes is
// the dependency for the format that actually turns up.
package fb2

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/text/encoding/htmlindex"
)

// Convert reads an FB2 and writes an EPUB.
func Convert(ctx context.Context, srcPath, dstPath string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}

	book, err := Parse(data)
	if err != nil {
		return fmt.Errorf("read fb2: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if err := book.WriteEPUB(out); err != nil {
		return err
	}
	return out.Sync()
}

// Book is an FB2 as far as an EPUB cares.
type Book struct {
	Title       string
	Authors     []string
	Language    string
	Annotation  string
	Publisher   string
	Date        string
	ISBN        string
	SeriesName  string
	SeriesIndex string
	CoverID     string

	Chapters []Chapter
	Images   []Image
}

// Chapter is one file in the finished book.
type Chapter struct {
	Title string
	// HTML is the chapter's body, already escaped and ready to write.
	HTML string
}

// Image is a picture, decoded from the base64 the file carries it as.
type Image struct {
	Name string // the name it gets inside the EPUB
	Type string
	Data []byte
}

// Parse reads an FB2 file.
func Parse(data []byte) (*Book, error) {
	var doc fictionBook
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	// FB2 files come in every encoding anyone ever used; the declaration is
	// trusted only when Go knows the name, and the bytes pass through otherwise.
	dec.CharsetReader = charsetReader

	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}

	book := &Book{
		Title:      strings.TrimSpace(doc.Description.TitleInfo.BookTitle),
		Language:   strings.TrimSpace(doc.Description.TitleInfo.Lang),
		Annotation: doc.Description.TitleInfo.Annotation.text(),
		Publisher:  strings.TrimSpace(doc.Description.PublishInfo.Publisher),
		Date:       strings.TrimSpace(doc.Description.PublishInfo.Year),
		ISBN:       strings.TrimSpace(doc.Description.PublishInfo.ISBN),
		SeriesName: strings.TrimSpace(doc.Description.TitleInfo.Sequence.Name),
	}
	if book.Language == "" {
		book.Language = "ru"
	}
	if n := strings.TrimSpace(doc.Description.TitleInfo.Sequence.Number); n != "" {
		book.SeriesIndex = n
	}
	for _, a := range doc.Description.TitleInfo.Authors {
		if name := a.name(); name != "" {
			book.Authors = append(book.Authors, name)
		}
	}

	// The cover is named by reference, and the reference carries a leading '#'.
	book.CoverID = strings.TrimPrefix(
		strings.TrimSpace(doc.Description.TitleInfo.Coverpage.Image.Href), "#")

	for _, bin := range doc.Binaries {
		decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(bin.Data), ""))
		if err != nil || len(decoded) == 0 {
			continue // a picture that will not decode is not worth failing a book over
		}
		book.Images = append(book.Images, Image{
			Name: imageName(bin.ID, bin.ContentType),
			Type: bin.ContentType,
			Data: decoded,
		})
	}

	book.Chapters = chaptersOf(doc)
	if len(book.Chapters) == 0 {
		return nil, fmt.Errorf("no readable text in this fb2")
	}
	if book.Title == "" {
		book.Title = "Untitled"
	}
	return book, nil
}

// chaptersOf splits the book into files.
//
// One per top-level section, which is what an FB2 author means by a chapter.
// Sections nested inside stay in the same file as headings, because splitting
// them out would turn a book with sub-sections into hundreds of fragments.
func chaptersOf(doc fictionBook) []Chapter {
	var out []Chapter

	for _, body := range doc.Bodies {
		notes := strings.EqualFold(body.Name, "notes") || strings.EqualFold(body.Name, "comments")

		// A body's own opening matter, before any section.
		if intro := renderNodes(body.Content, 1); strings.TrimSpace(stripTags(intro)) != "" {
			title := body.Title.text()
			if title == "" && notes {
				title = "Notes"
			}
			out = append(out, Chapter{Title: title, HTML: heading(title, 1) + intro})
		}

		for _, section := range body.Sections {
			html := renderSection(section, 1)
			if strings.TrimSpace(stripTags(html)) == "" {
				continue
			}
			out = append(out, Chapter{Title: section.Title.text(), HTML: html})
		}
	}
	return out
}

// imageName keeps the id the book refers to, so links resolve, while making sure
// it has an extension a reader will believe.
func imageName(id, contentType string) string {
	name := path.Base(strings.TrimSpace(id))
	if name == "" || name == "." || name == "/" {
		name = "image"
	}
	if path.Ext(name) == "" {
		name += extensionFor(contentType)
	}
	return name
}

func extensionFor(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	case "image/webp":
		return ".webp"
	}
	return ".jpg"
}

// charsetReader decodes the encodings FB2 files are actually written in.
//
// Go's XML decoder only knows UTF-8, and a great many FB2 files — most of the
// Russian ones — declare windows-1251 or koi8-r. Passing those bytes through
// unchanged fails on the first Cyrillic character, which is to say on the title.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(strings.TrimSpace(label))
	if err != nil {
		// An encoding nobody has heard of: pass it through and let the parser
		// salvage what it can, rather than refusing the book.
		return input, nil
	}
	return enc.NewDecoder().Reader(input), nil
}

func stripTags(s string) string {
	var b strings.Builder
	var depth int
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func escape(s string) string { return html.EscapeString(s) }

func heading(text string, level int) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	if level > 6 {
		level = 6
	}
	return fmt.Sprintf("<h%d>%s</h%d>", level, escape(text), level)
}

// writeZipFile writes one entry, storing the mimetype uncompressed as the format
// requires.
func writeZipFile(zw *zip.Writer, name string, data []byte) error {
	method := zip.Deflate
	if name == "mimetype" {
		method = zip.Store
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
