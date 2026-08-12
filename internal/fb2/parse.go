package fb2

import (
	"encoding/xml"
	"strings"
)

// The subset of FB2 that matters for reading a book. Everything else in the
// format — custom info, document history, the second title-info some files carry
// — is metadata about the file rather than about the book.

type fictionBook struct {
	XMLName     xml.Name `xml:"FictionBook"`
	Description struct {
		TitleInfo   titleInfo `xml:"title-info"`
		PublishInfo struct {
			Publisher string `xml:"publisher"`
			Year      string `xml:"year"`
			ISBN      string `xml:"isbn"`
		} `xml:"publish-info"`
	} `xml:"description"`
	Bodies   []body   `xml:"body"`
	Binaries []binary `xml:"binary"`
}

type titleInfo struct {
	BookTitle  string   `xml:"book-title"`
	Lang       string   `xml:"lang"`
	Authors    []author `xml:"author"`
	Annotation node     `xml:"annotation"`
	Coverpage  struct {
		Image struct {
			Href string `xml:"href,attr"`
		} `xml:"image"`
	} `xml:"coverpage"`
	Sequence struct {
		Name   string `xml:"name,attr"`
		Number string `xml:"number,attr"`
	} `xml:"sequence"`
}

type author struct {
	First  string `xml:"first-name"`
	Middle string `xml:"middle-name"`
	Last   string `xml:"last-name"`
	Nick   string `xml:"nickname"`
}

// name assembles a display name. A nickname stands in when there is nothing
// else, which is how translations and web publications usually credit people.
func (a author) name() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{a.First, a.Middle, a.Last} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return strings.TrimSpace(a.Nick)
	}
	return strings.Join(parts, " ")
}

type body struct {
	Name     string    `xml:"name,attr"`
	Title    node      `xml:"title"`
	Sections []section `xml:"section"`
	// Content is everything not in a section: an epigraph, a stray paragraph.
	Content []node `xml:",any"`
}

type section struct {
	ID       string    `xml:"id,attr"`
	Title    node      `xml:"title"`
	Sections []section `xml:"section"`
	Content  []node    `xml:",any"`
}

type binary struct {
	ID          string `xml:"id,attr"`
	ContentType string `xml:"content-type,attr"`
	Data        string `xml:",chardata"`
}

// node is any element of the text, kept with enough structure to render it.
//
// FB2 nests freely — emphasis inside a link inside a poem line — so this holds
// the tag, its attributes and its children rather than trying to flatten
// anything as it parses.
type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Chardata string     `xml:",chardata"`
	Children []node     `xml:",any"`
}

// text is everything a person would read under this node, whitespace collapsed.
func (n node) text() string {
	var b strings.Builder
	n.appendText(&b)
	return strings.Join(strings.Fields(b.String()), " ")
}

func (n node) appendText(b *strings.Builder) {
	b.WriteString(n.Chardata)
	for _, c := range n.Children {
		c.appendText(b)
		b.WriteString(" ")
	}
}

func (n node) attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
