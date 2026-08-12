package kepubconv

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// An EPUB content document is XHTML, so it has to be well-formed XML.
// html.Render writes HTML5 — `<img src="…">` with nothing closing it — which a
// strict reader refuses to parse, and a book that will not open is worse than
// one without word-level progress. Hence a renderer of our own.

// voidElements have no closing tag in HTML and must be self-closed in XHTML.
var voidElements = map[atom.Atom]bool{
	atom.Area: true, atom.Base: true, atom.Br: true, atom.Col: true,
	atom.Embed: true, atom.Hr: true, atom.Img: true, atom.Input: true,
	atom.Link: true, atom.Meta: true, atom.Param: true, atom.Source: true,
	atom.Track: true, atom.Wbr: true,
}

func renderXHTML(w io.Writer, n *html.Node) error {
	switch n.Type {
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := renderXHTML(w, c); err != nil {
				return err
			}
		}
		return nil

	case html.DoctypeNode:
		_, err := io.WriteString(w, "<!DOCTYPE "+n.Data+">")
		return err

	case html.CommentNode:
		// A comment cannot contain "--", and one that does would make the
		// document unparseable.
		_, err := io.WriteString(w, "<!--"+strings.ReplaceAll(n.Data, "--", "- -")+"-->")
		return err

	case html.TextNode:
		_, err := io.WriteString(w, escapeText(n.Data))
		return err

	case html.ElementNode:
		return renderElement(w, n)
	}
	return nil
}

func renderElement(w io.Writer, n *html.Node) error {
	name := n.Data
	if _, err := io.WriteString(w, "<"+name); err != nil {
		return err
	}

	// The html element must carry the XHTML namespace, or the document is not a
	// content document at all. The parser drops it from some inputs.
	attrs := n.Attr
	if n.DataAtom == atom.Html && !hasAttr(attrs, "xmlns") {
		attrs = append([]html.Attribute{{Key: "xmlns", Val: "http://www.w3.org/1999/xhtml"}}, attrs...)
	}

	for _, a := range attrs {
		key := a.Key
		if a.Namespace != "" {
			key = a.Namespace + ":" + a.Key
		}
		if _, err := fmt.Fprintf(w, ` %s="%s"`, key, escapeAttr(a.Val)); err != nil {
			return err
		}
	}

	if voidElements[n.DataAtom] && n.FirstChild == nil {
		_, err := io.WriteString(w, "/>")
		return err
	}
	if _, err := io.WriteString(w, ">"); err != nil {
		return err
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := renderXHTML(w, c); err != nil {
			return err
		}
	}

	_, err := io.WriteString(w, "</"+name+">")
	return err
}

func hasAttr(attrs []html.Attribute, key string) bool {
	for _, a := range attrs {
		if a.Key == key {
			return true
		}
	}
	return false
}

// escapeText escapes what XML requires and nothing else. Named entities are not
// used: XHTML only defines a handful of them, and a book carrying `&nbsp;` in
// its output is a book that will not parse.
func escapeText(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}

func escapeAttr(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	).Replace(s)
}
