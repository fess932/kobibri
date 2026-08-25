package kepubconv

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// nativeConverter turns an EPUB into a KEPUB without kepubify.
//
// It exists because kepubify has had no release since 2022 and one dependency
// stands between this server and every book it serves. What it must do is not a
// matter of taste: spec_test.go pins the rules by measuring the converter this
// replaces, and the differential test runs both over the same book and compares
// span for span. Ids are where reading positions are stored, so a converter that
// numbered them differently would move every saved position in every book.
type nativeConverter struct{}

func newNativeConverter() *nativeConverter { return &nativeConverter{} }

func (n *nativeConverter) Name() string { return "kobibri" }

// kobostylehacks is what Kobo's own renderer expects to find. Without it a book
// renders with the wrong margins.
const kobostylehacks = "div#book-inner { margin-top: 0; margin-bottom: 0;}"

func (n *nativeConverter) Convert(ctx context.Context, srcPath, dstPath string) error {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("open epub: %w", err)
	}
	defer func() { _ = zr.Close() }()

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	zw := zip.NewWriter(out)

	// `mimetype` must come first and must not be compressed — the format says so,
	// and a reader that checks will reject the book outright.
	if err := writeMimetype(zw, &zr.Reader); err != nil {
		return err
	}

	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.Name == "mimetype" {
			continue
		}
		if err := n.copyOrConvert(zw, f); err != nil {
			return err
		}
	}

	if err := zw.Close(); err != nil {
		return err
	}
	return out.Sync()
}

func writeMimetype(zw *zip.Writer, zr *zip.Reader) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		return err
	}
	// Written from what the book says rather than from a constant: a book whose
	// mimetype differs is not ours to correct.
	content := "application/epub+zip"
	if f, err := zr.Open("mimetype"); err == nil {
		if data, err := io.ReadAll(io.LimitReader(f, 256)); err == nil && len(data) > 0 {
			content = strings.TrimSpace(string(data))
		}
		_ = f.Close()
	}
	_, err = io.WriteString(w, content)
	return err
}

func (n *nativeConverter) copyOrConvert(zw *zip.Writer, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("read %s: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	header := f.FileHeader
	header.Name = f.Name
	// Sizes and checksums are about to change for anything transformed, and the
	// writer recomputes them anyway.
	header.Method = zip.Deflate
	w, err := zw.CreateHeader(&header)
	if err != nil {
		return err
	}

	if !isContentDocument(f.Name) {
		_, err = io.Copy(w, rc)
		return err
	}

	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	converted, err := spanify(data)
	if err != nil {
		// A chapter that cannot be parsed is served as it was. Losing the spans
		// in one chapter costs word-level progress there; refusing the book
		// costs the whole book.
		converted = data
	}
	_, err = w.Write(converted)
	return err
}

func isContentDocument(name string) bool {
	switch strings.ToLower(name[strings.LastIndexByte(name, '.')+1:]) {
	case "xhtml", "html", "htm":
		return true
	}
	return false
}

// spanify is the whole conversion of one chapter.
func spanify(data []byte) ([]byte, error) {
	doc, err := parseDocument(data)
	if err != nil {
		return nil, err
	}

	// The HTML parser has no notion of an XML declaration and keeps it as a
	// comment. Left there it would be written out a second time, beside the real
	// one, and the document would no longer be well-formed.
	dropXMLDeclarationComment(doc)

	head := find(doc, atom.Head)
	body := find(doc, atom.Body)
	if body == nil {
		return nil, fmt.Errorf("no body")
	}

	if head != nil {
		head.AppendChild(styleHack())
	}
	addSpans(body)
	wrapBody(body)

	var buf strings.Builder
	buf.WriteString(xmlDeclaration(data))
	if err := renderXHTML(&buf, doc); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// xmlDeclaration keeps the book's own prologue, since html.Parse drops it and an
// EPUB is XML.
func xmlDeclaration(data []byte) string {
	text := string(data)
	if !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<?xml") {
		return `<?xml version="1.0" encoding="UTF-8"?>`
	}
	if end := strings.Index(text, "?>"); end > 0 {
		return strings.TrimSpace(text[:end+2])
	}
	return `<?xml version="1.0" encoding="UTF-8"?>`
}

func dropXMLDeclarationComment(doc *html.Node) {
	for c := doc.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.CommentNode && strings.HasPrefix(strings.TrimSpace(c.Data), "?xml") {
			doc.RemoveChild(c)
		}
		c = next
	}
}

func styleHack() *html.Node {
	style := &html.Node{
		Type: html.ElementNode, Data: "style", DataAtom: atom.Style,
		Attr: []html.Attribute{
			{Key: "type", Val: "text/css"},
			{Key: "class", Val: "kobostylehacks"},
		},
	}
	style.AppendChild(&html.Node{Type: html.TextNode, Data: kobostylehacks})
	return style
}

// wrapBody puts the body's children inside the two divs Kobo's renderer expects.
func wrapBody(body *html.Node) {
	columns := element("div", "id", "book-columns")
	inner := element("div", "id", "book-inner")
	columns.AppendChild(inner)

	var children []*html.Node
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		children = append(children, c)
	}
	for _, c := range children {
		body.RemoveChild(c)
		inner.AppendChild(c)
	}
	body.AppendChild(columns)
}

func element(name string, attrs ...string) *html.Node {
	n := &html.Node{Type: html.ElementNode, Data: name, DataAtom: atom.Lookup([]byte(name))}
	for i := 0; i+1 < len(attrs); i += 2 {
		n.Attr = append(n.Attr, html.Attribute{Key: attrs[i], Val: attrs[i+1]})
	}
	return n
}

// addSpans wraps every run of text in the body in a koboSpan.
//
// The rules are not ours to choose. Ids are where reading positions are stored,
// so this follows kepubify exactly — its counters, its skips, its idea of where
// a sentence ends — because a book reconverted with different ids loses
// everyone's place in it. Where the behaviour looks odd, it is odd on purpose;
// spec_test.go measures both and fails on any difference.
func addSpans(body *html.Node) {
	// A book that already carries kobo spans is left alone: converting twice
	// would nest them and break the very positions they exist for.
	if hasKoboSpan(body) {
		return
	}

	var para, seg int
	var incParaNext bool

	// Depth-first, top to bottom. The tree is rewritten as it is walked, so the
	// stack holds what is still to do rather than an iterator over it.
	stack := []*html.Node{body}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		switch cur.Type {
		case html.TextNode:
			parent := cur.Parent
			for _, sentence := range splitSentences(cur.Data) {
				// Whitespace between elements is not read, so it is put back as
				// it was — except directly inside a paragraph, where it is part
				// of the text.
				if isAllSpace(sentence) && parent.DataAtom != atom.P {
					parent.InsertBefore(&html.Node{Type: html.TextNode, Data: sentence}, cur)
					continue
				}
				if incParaNext {
					para++
					seg = 0
					incParaNext = false
				}
				seg++
				span := koboSpan(para, seg)
				span.AppendChild(&html.Node{Type: html.TextNode, Data: sentence})
				parent.InsertBefore(span, cur)
			}
			parent.RemoveChild(cur)

		case html.ElementNode:
			switch cur.DataAtom {
			case atom.Img:
				// An image is a paragraph of its own, and takes its number now
				// rather than waiting for text that will never come.
				para++
				seg = 1
				incParaNext = false

				span := koboSpan(para, seg)
				parent := cur.Parent
				parent.InsertBefore(span, cur)
				parent.RemoveChild(cur)
				span.AppendChild(cur)
				continue

			case atom.Script, atom.Style, atom.Pre, atom.Audio, atom.Video,
				atom.Svg, atom.Math:
				// Left as they are: preformatted text is shown space for space,
				// and the rest is not text at all.
				continue

			case atom.P, atom.Ol, atom.Ul, atom.Table,
				atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				// Deferred on purpose: an element that turns out to hold no text
				// must not consume a number, or every id after it shifts.
				incParaNext = true
			}

			if cur.Data == "math" || cur.Data == "svg" {
				continue
			}
			for c := cur.LastChild; c != nil; c = c.PrevSibling {
				stack = append(stack, c)
			}
		}
	}
}

func hasKoboSpan(n *html.Node) bool {
	if n.Type == html.ElementNode {
		for _, a := range n.Attr {
			if a.Key == "class" && strings.Contains(a.Val, "koboSpan") {
				return true
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if hasKoboSpan(c) {
			return true
		}
	}
	return false
}

func koboSpan(para, seg int) *html.Node {
	return element("span",
		"class", "koboSpan",
		"id", fmt.Sprintf("kobo.%d.%d", para, seg))
}

// isAllSpace uses Unicode's idea of a space, which includes the non-breaking
// one. The sentence splitter deliberately does not — the two disagree in
// kepubify too, and matching one while missing the other puts a span around
// every stray &#160; in a book, shifting every id after it.
func isAllSpace(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// splitSentences cuts text into the pieces each koboSpan covers.
//
// A sentence runs up to terminal punctuation, optionally one closing quote, and
// the whitespace after it; the whitespace belongs to the sentence it ends. This
// is a transcription of kepubify's state machine, deliberately down to its
// quirks — "Mr. Smith" is two sentences to it, and making that better here would
// renumber every book already converted.
func splitSentences(text string) []string {
	const (
		stateDefault = iota
		stateAfterPunct
		stateAfterExtra
		stateAfterSpace
		stateDone = -1
	)

	var out []string
	rest, i, state := text, 0, stateDefault

	for state != stateDone {
		r, width := utf8.DecodeRuneInString(rest[i:])

		var class int
		switch {
		case width == 0:
			class = classEOS
		case r == utf8.RuneError:
			class = classAny
		case r == '.' || r == '!' || r == '?':
			class = classPunct
		case r == '\'' || r == '"' || r == '\u201d' || r == '\u2019' || r == '\u201c' || r == '\u2026':
			class = classExtra
		case r == '\t' || r == '\n' || r == '\f' || r == '\r' || r == ' ':
			class = classSpace
		default:
			class = classAny
		}

		emit := emitNone
		switch state {
		case stateDefault:
			switch class {
			case classPunct:
				state = stateAfterPunct
			case classEOS:
				emit, state = emitRest, stateDone
			}
		case stateAfterPunct:
			switch class {
			case classPunct:
				state = stateAfterPunct
			case classExtra:
				state = stateAfterExtra
			case classSpace:
				state = stateAfterSpace
			case classEOS:
				emit, state = emitRest, stateDone
			default:
				state = stateDefault
			}
		case stateAfterExtra:
			switch class {
			case classPunct:
				state = stateAfterPunct
			case classSpace:
				state = stateAfterSpace
			case classEOS:
				emit, state = emitRest, stateDone
			default:
				state = stateDefault
			}
		case stateAfterSpace:
			switch class {
			case classSpace:
				state = stateAfterSpace
			case classPunct:
				emit, state = emitNext, stateAfterPunct
			case classEOS:
				emit, state = emitRest, stateDone
			default:
				emit, state = emitNext, stateDefault
			}
		}

		switch emit {
		case emitNone:
			i += width
		case emitNext:
			out = append(out, rest[:i])
			rest, i = rest[i:], width
		case emitRest:
			if len(rest) != 0 || len(out) == 0 {
				out = append(out, rest)
			}
		}
	}
	return out
}

const (
	classPunct = iota
	classExtra
	classSpace
	classAny
	classEOS
)

const (
	emitNone = iota
	emitNext
	emitRest
)

func find(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if got := find(c, a); got != nil {
			return got
		}
	}
	return nil
}
