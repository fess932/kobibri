package kepubconv

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

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
	defer zr.Close()

	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()

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
		f.Close()
	}
	_, err = io.WriteString(w, content)
	return err
}

func (n *nativeConverter) copyOrConvert(zw *zip.Writer, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("read %s: %w", f.Name, err)
	}
	defer rc.Close()

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
	doc, err := html.Parse(strings.NewReader(string(data)))
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
	wrapBody(body)

	inner := body.FirstChild.FirstChild // book-columns > book-inner
	block := 0
	for child := inner.FirstChild; child != nil; child = child.NextSibling {
		segments := 0
		spanifyNode(child, block+1, &segments)
		// A child that produced nothing does not consume a number, which is what
		// keeps an empty paragraph from shifting every id after it.
		if segments > 0 {
			block++
		}
	}

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

// spanifyNode walks one block, wrapping each run of text — and each image — in a
// koboSpan.
func spanifyNode(n *html.Node, block int, segments *int) {
	switch n.Type {
	case html.TextNode:
		wrapText(n, block, segments)
		return
	case html.ElementNode:
		switch n.DataAtom {
		case atom.Pre, atom.Script, atom.Style, atom.Svg, atom.Math:
			// Preformatted text is left alone: every space in it is part of what
			// is shown, and slicing it would change how it renders.
			return
		case atom.Img:
			wrapNode(n, block, segments)
			return
		}
	default:
		return
	}

	// Children are collected first: the walk rewrites the tree as it goes.
	var children []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		children = append(children, c)
	}
	for _, c := range children {
		spanifyNode(c, block, segments)
	}
}

// wrapText replaces a text node with one span per sentence.
func wrapText(n *html.Node, block int, segments *int) {
	if strings.TrimSpace(n.Data) == "" && n.Data == "" {
		return
	}
	// Whitespace between block elements is not text anyone reads, and wrapping it
	// would invent a segment the other converter does not have.
	if isStructuralWhitespace(n) {
		return
	}

	parts := splitSentences(n.Data)
	if len(parts) == 0 {
		return
	}

	parent := n.Parent
	for _, part := range parts {
		*segments++
		span := koboSpan(block, *segments)
		span.AppendChild(&html.Node{Type: html.TextNode, Data: part})
		parent.InsertBefore(span, n)
	}
	parent.RemoveChild(n)
}

// wrapNode puts a span around an element, for things that are read as content
// but hold no text of their own.
func wrapNode(n *html.Node, block int, segments *int) {
	*segments++
	span := koboSpan(block, *segments)

	parent := n.Parent
	parent.InsertBefore(span, n)
	parent.RemoveChild(n)
	span.AppendChild(n)
}

func koboSpan(block, segment int) *html.Node {
	return element("span",
		"class", "koboSpan",
		"id", fmt.Sprintf("kobo.%d.%d", block, segment))
}

// isStructuralWhitespace reports whether a text node is only the indentation
// between two elements rather than anything a reader sees.
func isStructuralWhitespace(n *html.Node) bool {
	if strings.TrimSpace(n.Data) != "" {
		return false
	}
	// Inside a paragraph, a run of spaces separates words and is part of the
	// text; between blocks it is formatting.
	if n.Parent != nil && isBlock(n.Parent.DataAtom) {
		return n.PrevSibling != nil || n.NextSibling != nil
	}
	return false
}

func isBlock(a atom.Atom) bool {
	switch a {
	case atom.Body, atom.Div, atom.Section, atom.Article, atom.Nav, atom.Aside,
		atom.Ul, atom.Ol, atom.Dl, atom.Table, atom.Thead, atom.Tbody, atom.Tfoot,
		atom.Tr, atom.Blockquote, atom.Figure, atom.Header, atom.Footer, atom.Main:
		return true
	}
	return false
}

// splitSentences cuts text after terminal punctuation that is immediately
// followed by a space, keeping the space with the sentence it ends.
//
// Deliberately naive, and it has to stay that way. A better splitter would end
// «a quoted sentence.» at the closing quote rather than swallowing the next
// sentence with it — but kepubify does not, and every book converted so far has
// ids that follow its rule. Being cleverer here would move every reading
// position in every book already on a device. The differential test caught this
// exact case, which is what it is for.
func splitSentences(text string) []string {
	if text == "" {
		return nil
	}

	var out []string
	start := 0
	runes := []rune(text)

	for i := 0; i < len(runes); i++ {
		if !isTerminal(runes[i]) {
			continue
		}
		end := i + 1
		if end >= len(runes) || !isSpace(runes[end]) {
			continue
		}
		// The space goes with the sentence it ends.
		for end < len(runes) && isSpace(runes[end]) {
			end++
		}
		out = append(out, string(runes[start:end]))
		start = end
		i = end - 1
	}

	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

func isTerminal(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '…' }

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ' '
}

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
