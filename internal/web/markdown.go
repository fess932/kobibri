package web

import (
	"html/template"
	"strings"
)

// renderProse turns the small amount of Markdown the API document uses into
// HTML: blank-line paragraphs, `code`, **bold**, and `- ` lists.
//
// Not a Markdown library, and not trying to be. These four are what the
// document actually contains, everything else is escaped, and a dependency that
// understands tables and footnotes would earn nothing here.
func renderProse(text string) template.HTML {
	var out strings.Builder
	var list []string

	flushList := func() {
		if len(list) == 0 {
			return
		}
		out.WriteString("<ul>")
		for _, item := range list {
			out.WriteString("<li>" + inlineProse(item) + "</li>")
		}
		out.WriteString("</ul>")
		list = nil
	}

	for _, block := range strings.Split(text, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		lines := strings.Split(block, "\n")
		bullets := true
		for _, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "- ") {
				bullets = false
				break
			}
		}
		if bullets {
			for _, line := range lines {
				list = append(list, strings.TrimPrefix(strings.TrimSpace(line), "- "))
			}
			flushList()
			continue
		}

		flushList()
		out.WriteString("<p>" + inlineProse(strings.Join(lines, " ")) + "</p>")
	}
	flushList()
	return template.HTML(out.String())
}

// inlineProse escapes first and marks up second, so no text in the document can
// reach the page as HTML.
func inlineProse(s string) string {
	escaped := template.HTMLEscapeString(s)
	escaped = wrapPairs(escaped, "**", "<strong>", "</strong>")
	escaped = wrapPairs(escaped, "`", "<code>", "</code>")
	return escaped
}

// wrapPairs replaces matched pairs of a delimiter. An unmatched trailing one is
// left as it was written rather than swallowing the rest of the paragraph.
func wrapPairs(s, delim, open, close string) string {
	parts := strings.Split(s, delim)
	if len(parts) < 3 {
		return s
	}

	var out strings.Builder
	for i, part := range parts {
		switch {
		case i == 0:
			out.WriteString(part)
		case i%2 == 1 && i+1 < len(parts):
			out.WriteString(open + part + close)
		case i%2 == 1:
			out.WriteString(delim + part)
		default:
			out.WriteString(part)
		}
	}
	return out.String()
}
