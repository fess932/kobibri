package fb2

import (
	"fmt"
	"strings"
)

// Turning FB2 markup into XHTML.
//
// FB2's vocabulary is small and maps onto HTML almost one to one. What needs
// care is the handful of things it has and HTML does not — poems, epigraphs,
// footnote links pointing at another body — and the rule that anything
// unrecognised keeps its text rather than being dropped: a reader would rather
// have a paragraph in the wrong style than not have it.

func renderSection(s section, depth int) string {
	var b strings.Builder
	b.WriteString(heading(s.Title.text(), depth))
	b.WriteString(renderNodes(s.Content, depth))
	for _, child := range s.Sections {
		b.WriteString(renderSection(child, depth+1))
	}
	return b.String()
}

func renderNodes(nodes []node, depth int) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(renderNode(n, depth))
	}
	return b.String()
}

func renderNode(n node, depth int) string {
	switch n.XMLName.Local {
	case "title", "section":
		// Handled by renderSection; a title reached here belongs to a body.
		return ""

	case "p":
		return wrap("p", "", renderInline(n))
	case "subtitle":
		return heading(n.text(), min(depth+1, 6))
	case "empty-line":
		return "<p class=\"empty-line\"> </p>"

	case "epigraph":
		return wrap("blockquote", "epigraph", renderNodes(n.Children, depth))
	case "cite":
		return wrap("blockquote", "cite", renderNodes(n.Children, depth))
	case "text-author":
		return wrap("p", "text-author", renderInline(n))

	case "poem":
		return wrap("div", "poem", renderNodes(n.Children, depth))
	case "stanza":
		return wrap("div", "stanza", renderNodes(n.Children, depth))
	case "v":
		return wrap("p", "verse", renderInline(n))

	case "image":
		return renderImage(n)

	case "table":
		return wrap("table", "", renderNodes(n.Children, depth))
	case "tr":
		return wrap("tr", "", renderNodes(n.Children, depth))
	case "td":
		return wrap("td", "", renderInline(n))
	case "th":
		return wrap("th", "", renderInline(n))

	case "annotation":
		return wrap("div", "annotation", renderNodes(n.Children, depth))

	default:
		// Unknown, but its text is still the book's.
		if len(n.Children) > 0 {
			return renderNodes(n.Children, depth)
		}
		if text := strings.TrimSpace(n.Chardata); text != "" {
			return wrap("p", "", escape(n.Chardata))
		}
		return ""
	}
}

// renderInline renders the contents of a text-bearing element, keeping the
// emphasis and links inside it.
func renderInline(n node) string {
	var b strings.Builder
	b.WriteString(escape(n.Chardata))

	for _, c := range n.Children {
		switch c.XMLName.Local {
		case "strong":
			b.WriteString(wrap("strong", "", renderInline(c)))
		case "emphasis":
			b.WriteString(wrap("em", "", renderInline(c)))
		case "strikethrough":
			b.WriteString(wrap("s", "", renderInline(c)))
		case "sub":
			b.WriteString(wrap("sub", "", renderInline(c)))
		case "sup":
			b.WriteString(wrap("sup", "", renderInline(c)))
		case "code":
			b.WriteString(wrap("code", "", renderInline(c)))
		case "style":
			b.WriteString(wrap("span", c.attr("name"), renderInline(c)))
		case "a":
			b.WriteString(renderLink(c))
		case "image":
			b.WriteString(renderImage(c))
		case "empty-line":
			b.WriteString("<br/>")
		default:
			b.WriteString(renderInline(c))
		}
		// Character data after a child element belongs to the parent.
		b.WriteString(escape(c.tail()))
	}
	return b.String()
}

// tail is nothing in this parse — Go's decoder gives all of an element's own
// character data in one field — but naming it says why nothing is missing here.
func (n node) tail() string { return "" }

// renderLink turns an FB2 link into an anchor.
//
// Footnotes point into another body of the same file with "#id"; everything ends
// up in one book, so a fragment reference works as it stands once the target
// keeps its id.
func renderLink(n node) string {
	href := n.attr("href")
	if href == "" {
		return renderInline(n)
	}
	text := renderInline(n)
	if strings.TrimSpace(stripTags(text)) == "" {
		text = escape(strings.TrimPrefix(href, "#"))
	}
	if strings.HasPrefix(href, "#") {
		return fmt.Sprintf(`<a href="notes.xhtml%s">%s</a>`, escapeAttr(href), text)
	}
	return fmt.Sprintf(`<a href="%s">%s</a>`, escapeAttr(href), text)
}

func renderImage(n node) string {
	href := strings.TrimPrefix(n.attr("href"), "#")
	if href == "" {
		return ""
	}
	alt := n.attr("alt")
	return fmt.Sprintf(`<div class="image"><img src="images/%s" alt="%s"/></div>`,
		escapeAttr(imageRef(href)), escapeAttr(alt))
}

// imageRef is the name a rendered reference points at, which has to agree with
// what the binaries were stored as.
func imageRef(id string) string { return imageName(id, "") }

func wrap(tag, class, inner string) string {
	if strings.TrimSpace(stripTags(inner)) == "" && tag != "td" && tag != "th" {
		return ""
	}
	if class == "" {
		return "<" + tag + ">" + inner + "</" + tag + ">"
	}
	return `<` + tag + ` class="` + escapeAttr(class) + `">` + inner + `</` + tag + `>`
}

func escapeAttr(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	).Replace(s)
}
