package kepubconv

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// An EPUB content document is XHTML, which is XML, and it has to be parsed as
// XML.
//
// Parsing it as HTML looks like it works and quietly ruins books: `<title/>` is
// self-closing in XHTML but opens an element in HTML, whose content is RCDATA —
// so an HTML parser swallows the rest of the document as the title's text. Every
// book from a converter that self-closes its empty elements comes out with one
// chapter of nothing. Fifty-three of fifty-five real books hit this.
//
// The tree is still html.Node so the transformation and the renderer do not have
// to care which parser produced it.

// parseDocument reads a content document, as XML first and as HTML only if that
// fails.
//
// The fallback is not a nicety: plenty of EPUB 2 books in the wild are not
// well-formed XML at all, and a book that renders badly is better than a book
// that will not convert.
func parseDocument(data []byte) (*html.Node, error) {
	doc, err := parseXHTML(data)
	if err == nil {
		return doc, nil
	}
	return html.Parse(strings.NewReader(string(data)))
}

func parseXHTML(data []byte) (*html.Node, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	// XHTML defines the HTML entities; a document using one is not in error.
	dec.Entity = xml.HTMLEntity

	doc := &html.Node{Type: html.DocumentNode}
	stack := []*html.Node{doc}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		parent := stack[len(stack)-1]
		switch tok := tok.(type) {
		case xml.StartElement:
			n := elementFrom(tok)
			parent.AppendChild(n)
			stack = append(stack, n)

		case xml.EndElement:
			if len(stack) < 2 {
				return nil, fmt.Errorf("unbalanced </%s>", tok.Name.Local)
			}
			stack = stack[:len(stack)-1]

		case xml.CharData:
			parent.AppendChild(&html.Node{Type: html.TextNode, Data: string(tok)})

		case xml.Comment:
			parent.AppendChild(&html.Node{Type: html.CommentNode, Data: string(tok)})

		case xml.Directive:
			// <!DOCTYPE html> and friends.
			if d := strings.TrimSpace(string(tok)); strings.HasPrefix(strings.ToUpper(d), "DOCTYPE") {
				parent.AppendChild(&html.Node{
					Type: html.DoctypeNode,
					Data: strings.TrimSpace(d[len("DOCTYPE"):]),
				})
			}
		}
	}

	if len(stack) != 1 {
		return nil, fmt.Errorf("document ends inside an element")
	}
	if find(doc, atom.Body) == nil {
		return nil, fmt.Errorf("no body")
	}
	return doc, nil
}

// elementFrom keeps the name and attributes exactly as written, prefixes and
// all: a book's own namespaces are not ours to normalise, and rewriting them is
// how epub:type and xml:lang go missing.
func elementFrom(start xml.StartElement) *html.Node {
	name := start.Name.Local
	n := &html.Node{
		Type:     html.ElementNode,
		Data:     name,
		DataAtom: atom.Lookup([]byte(strings.ToLower(name))),
	}

	for _, a := range start.Attr {
		key, ns := a.Name.Local, a.Name.Space
		switch ns {
		case "":
			// nothing to restore
		case "xmlns":
			key = "xmlns:" + key
		default:
			// Go hands back the resolved namespace URI rather than the prefix it
			// was written with. The common ones are restored by name; anything
			// else keeps the local name, which is still valid XML.
			if prefix, ok := knownPrefixes[ns]; ok {
				key = prefix + ":" + key
			}
		}
		n.Attr = append(n.Attr, html.Attribute{Key: key, Val: a.Value})
	}
	return n
}

// knownPrefixes maps the namespaces an EPUB actually uses back to the prefix
// they are conventionally written with.
var knownPrefixes = map[string]string{
	"http://www.w3.org/XML/1998/namespace": "xml",
	"http://www.idpf.org/2007/ops":         "epub",
	"http://www.w3.org/1999/xlink":         "xlink",
	"http://www.w3.org/2000/svg":           "svg",
}
