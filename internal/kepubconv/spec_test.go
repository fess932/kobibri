package kepubconv

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// What a KEPUB converter has to get right, pinned against the one we use.
//
// This is the precondition for replacing kepubify with our own: a replacement is
// only safe if it produces the same span ids for the same text, because those
// ids are where a reader's position is stored. A book converted by a different
// converter with different ids loses everyone's place in it.
//
// So the rules are established here by measurement, and the same helpers can run
// any other converter against the same fixtures.

// span is one koboSpan: the id a reading position points at, and the text under
// it.
type span struct {
	id   string
	text string
}

var spanID = regexp.MustCompile(`^kobo\.(\d+)\.(\d+)$`)

// spansOf pulls the koboSpans out of a converted chapter, in document order.
func spansOf(t *testing.T, xhtml string) []span {
	t.Helper()

	var out []span
	dec := xml.NewDecoder(strings.NewReader(xhtml))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	var current *span
	var depth int

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading converted chapter: %v\n%s", err, xhtml)
		}

		switch tok := tok.(type) {
		case xml.StartElement:
			if current != nil {
				depth++
				continue
			}
			if tok.Name.Local != "span" {
				continue
			}
			var id, class string
			for _, a := range tok.Attr {
				switch a.Name.Local {
				case "id":
					id = a.Value
				case "class":
					class = a.Value
				}
			}
			if strings.Contains(class, "koboSpan") {
				current = &span{id: id}
				depth = 0
			}
		case xml.EndElement:
			if current == nil {
				continue
			}
			if depth > 0 {
				depth--
				continue
			}
			out = append(out, *current)
			current = nil
		case xml.CharData:
			if current != nil {
				current.text += string(tok)
			}
		}
	}
	return out
}

// visibleText is every character a reader would see, with whitespace collapsed:
// what must survive a conversion unchanged.
func visibleText(t *testing.T, xhtml string) string {
	t.Helper()

	var b strings.Builder
	dec := xml.NewDecoder(strings.NewReader(xhtml))
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	// The head is not part of the book's text: a converter is free to leave a
	// title there, and counting it would make every comparison fail for the
	// wrong reason.
	unread := map[string]bool{"head": true, "script": true, "style": true}

	skip := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading chapter: %v", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if unread[tok.Name.Local] {
				skip++
			}
		case xml.EndElement:
			if unread[tok.Name.Local] && skip > 0 {
				skip--
			}
		case xml.CharData:
			if skip == 0 {
				b.Write(tok)
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// squash drops every space, including the non-breaking one — which counts as
// whitespace to Go and not to a reader, and comparing the two sides with
// different ideas about that is how a harness accuses a converter of losing text
// it kept.
//
// Two blocks that touch in the source ("</p><p>") are separated in the output,
// and a converter is entitled to do that. What it is not entitled to do is
// change a character a reader sees.
func squash(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), "")
}

// chapters lists the fixtures, each one aimed at something a converter can get
// wrong.
var chapters = []struct {
	name string
	body string
}{
	{"plain", `<p>First sentence. Second sentence!</p><p>Another paragraph?</p>`},
	{"inline", `<p>Text with <em>emphasis</em> and <a href="x.xhtml">a link</a> inside it.</p>`},
	{"nested", `<div><blockquote><p>Quoted <strong>strongly</strong>.</p></blockquote></div>`},
	{"heading", `<h1>A Heading</h1><p>Body under it.</p>`},
	{"image", `<p>Before.</p><p><img src="images/plate.png" alt="a plate"/></p><p>After.</p>`},
	{"entities", `<p>Caf&#233; &amp; cr&#232;me &#8212; &#171;quoted&#187;.</p>`},
	{"cyrillic", `<p>Первое предложение. Второе предложение! И третье?</p>`},
	{"table", `<table><tr><td>Cell one.</td><td>Cell two.</td></tr></table>`},
	{"pre", `<pre>  indented   text
kept as is</pre>`},
	{"empty", `<p></p><p>   </p><p>Real text.</p>`},
	{"existing-span", `<p><span id="mine">Already spanned.</span> And more.</p>`},
	{"list", `<ul><li>One item.</li><li>Two items.</li></ul>`},

	// The awkward half: what real books are full of and hand-written fixtures
	// usually are not.
	{"nbsp", `<p>Non&#160;breaking&#160;spaces. And a&nbsp;named one.</p>`},
	{"footnote", `<p>A claim<a href="notes.xhtml#n1" epub:type="noteref"><sup>1</sup></a> and then more.</p>`},
	{"attributes", `<p class="a &amp; b" title="quote &quot;here&quot;">Text.</p>`},
	{"svg", `<p>Before.</p><div><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"><rect width="1" height="1"/></svg></div><p>After.</p>`},
	{"deep-inline", `<p><em><strong><span>Deeply</span> nested</strong> markup</em> ends here.</p>`},
	{"br", `<p>Line one.<br/>Line two.</p>`},
	{"abbrev", `<p>Written by A. B. Author in 1999. Next sentence.</p>`},
	{"quotes", `<p>&#171;A quoted sentence.&#187; Then another.</p>`},
	{"lang", `<p xml:lang="fr" lang="fr">Bonjour. Ça va?</p>`},
	{"comment", `<p>Before.<!-- a note --> After.</p>`},
}

// convertFixtures builds an EPUB from the fixtures above, converts it, and
// returns each chapter's converted XHTML.
func convertFixtures(t *testing.T, conv Converter) map[string]string {
	t.Helper()

	dir := t.TempDir()
	src := filepath.Join(dir, "in.epub")
	writeFixtureEPUB(t, src)

	dst := filepath.Join(dir, "out.kepub.epub")
	if err := conv.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("%s: %v", conv.Name(), err)
	}

	zr, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	out := map[string]string{}
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		// kepubify adds a title page of its own; it is not one of the book's
		// chapters and follows none of the rules being pinned here.
		if !strings.HasSuffix(name, ".xhtml") || name == "nav.xhtml" ||
			strings.HasPrefix(name, "kepubify-") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(name, ".xhtml")] = string(data)
	}
	return out
}

func writeFixtureEPUB(t *testing.T, path string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	var manifest, spine strings.Builder
	for _, c := range chapters {
		fmt.Fprintf(&manifest,
			`<item id="%s" href="%s.xhtml" media-type="application/xhtml+xml"/>`, c.name, c.name)
		fmt.Fprintf(&spine, `<itemref idref="%s"/>`, c.name)
	}

	files := map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"content.opf": `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="id">urn:uuid:11111111-2222-3333-4444-555555555555</dc:identifier>
    <dc:title>Conversion Fixtures</dc:title>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>` + manifest.String() + `</manifest>
  <spine>` + spine.String() + `</spine>
</package>`,
	}
	for _, c := range chapters {
		files[c.name+".xhtml"] = `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>` + c.name + `</title></head>
<body>` + c.body + `</body></html>`
	}

	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// Nothing a reader can see may be lost or duplicated. This is the rule a
// converter cannot bend: it is not a matter of taste which characters survive.
func TestConversionKeepsEveryVisibleCharacter(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	for _, c := range chapters {
		got, ok := converted[c.name]
		if !ok {
			t.Errorf("%s: chapter missing from the converted book", c.name)
			continue
		}

		before := visibleText(t, "<body>"+c.body+"</body>")
		after := visibleText(t, got)
		if squash(before) != squash(after) {
			t.Errorf("%s: the text changed\n before: %q\n  after: %q", c.name, before, after)
		}
	}
}

// Ids are where a reading position is stored, so they have to be unique within a
// chapter and shaped the way the device expects.
func TestSpanIDsAreWellFormedAndUnique(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	for name, xhtml := range converted {
		seen := map[string]bool{}
		spans := spansOf(t, xhtml)

		for _, s := range spans {
			if !spanID.MatchString(s.id) {
				t.Errorf("%s: id %q is not of the form kobo.N.M", name, s.id)
			}
			if seen[s.id] {
				t.Errorf("%s: id %q appears twice", name, s.id)
			}
			seen[s.id] = true
		}
	}
}

// Every fixture that has words must end up with spans over them, or a reading
// position in that chapter cannot be expressed at all.
func TestTextEndsUpInsideSpans(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	for _, c := range chapters {
		// Preformatted text is left alone, on purpose — see the test below.
		if c.name == "pre" {
			continue
		}

		xhtml := converted[c.name]
		want := visibleText(t, "<body>"+c.body+"</body>")
		if want == "" {
			continue
		}

		var inSpans strings.Builder
		for _, s := range spansOf(t, xhtml) {
			inSpans.WriteString(s.text)
			inSpans.WriteString(" ")
		}
		got := inSpans.String()

		if squash(got) != squash(want) {
			t.Errorf("%s: the spans do not cover the text\n  spanned: %q\n     text: %q",
				c.name, got, want)
		}
	}
}

// Preformatted text is left unspanned, and that is right rather than an
// oversight: every space in a <pre> block is part of what is shown, and slicing
// it into spans would change how it renders. The cost is that a reading position
// inside such a block falls back to the block itself, which is a fair trade for
// not corrupting code and poetry.
func TestPreformattedTextIsLeftAlone(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	if spans := spansOf(t, converted["pre"]); len(spans) != 0 {
		t.Errorf("preformatted text was sliced into %d spans; its whitespace is load-bearing",
			len(spans))
	}
	if !strings.Contains(converted["pre"], "  indented   text") {
		t.Error("the preformatted text was reflowed")
	}
}

// The wrappers are what Kobo's own renderer expects to find. Without them the
// book renders with the wrong margins and paging.
func TestTheKoboWrappersArePresent(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	for name, xhtml := range converted {
		for _, want := range []string{
			`<div id="book-columns">`,
			`<div id="book-inner">`,
			`class="kobostylehacks"`,
		} {
			if !strings.Contains(xhtml, want) {
				t.Errorf("%s: missing %s", name, want)
			}
		}
	}
}

// Ids are kobo.<block>.<segment>, both counted from one, and they run in reading
// order. A replacement converter that numbered them differently would move every
// saved position in the book.
func TestSpanNumberingRunsInReadingOrder(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	spans := spansOf(t, converted["plain"])
	want := []span{
		{"kobo.1.1", "First sentence. "},
		{"kobo.1.2", "Second sentence!"},
		{"kobo.2.1", "Another paragraph?"},
	}
	if len(spans) != len(want) {
		t.Fatalf("%d spans, want %d: %+v", len(spans), len(want), spans)
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, spans[i], want[i])
		}
	}
}

// A span the book already had is kept, with the koboSpan placed inside it.
// Replacing it would break any link or stylesheet that referred to it.
func TestAnExistingSpanIsKept(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	got := converted["existing-span"]
	if !strings.Contains(got, `<span id="mine">`) {
		t.Errorf("the book's own span was lost:\n%s", got)
	}
	if !strings.Contains(got, `<span id="mine"><span class="koboSpan"`) {
		t.Errorf("the koboSpan did not go inside the book's own span:\n%s", got)
	}
}

// The output has to be well-formed XML. EPUB content documents are XHTML, and a
// renderer that emits HTML5 void elements — <img> with no closing slash — makes
// a book a strict reader refuses to open at all.
func TestConvertedChaptersAreWellFormedXML(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	for name, xhtml := range converted {
		dec := xml.NewDecoder(strings.NewReader(xhtml))
		dec.Strict = true
		dec.Entity = xml.HTMLEntity

		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("%s: not well-formed XML: %v\n%s", name, err, xhtml)
				break
			}
		}
	}
}

// Blocks are numbered by the body's own children, and a child that yields no
// spans at all does not consume a number. Measured, not assumed: this is the
// rule a replacement has to reproduce exactly, or every saved position moves.
func TestBlockNumbering(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	tests := map[string][]string{
		// Two paragraphs, two blocks.
		"plain": {"kobo.1.1", "kobo.1.2", "kobo.2.1"},
		// One outer div, however deeply the text sits inside it.
		"nested": {"kobo.1.1", "kobo.1.2", "kobo.1.3"},
		// A heading is a block like any other.
		"heading": {"kobo.1.1", "kobo.2.1"},
		// A whole list is one block; each item is a segment.
		"list": {"kobo.1.1", "kobo.1.2"},
		// So is a whole table.
		"table": {"kobo.1.1", "kobo.1.2"},
		// A paragraph holding only an image still counts, and still gets a span.
		"image": {"kobo.1.1", "kobo.2.1", "kobo.3.1"},
		// The genuinely empty paragraph is skipped; the whitespace one is not.
		"empty": {"kobo.1.1", "kobo.2.1"},
	}

	for name, want := range tests {
		var got []string
		for _, s := range spansOf(t, converted[name]) {
			got = append(got, s.id)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: ids %v, want %v", name, got, want)
		}
	}
}

// Sentences are split after terminal punctuation followed by a space, and the
// space stays with the sentence it ends.
func TestSentenceSplitting(t *testing.T) {
	converted := convertFixtures(t, newLibConverter())

	var got []string
	for _, s := range spansOf(t, converted["cyrillic"]) {
		got = append(got, s.text)
	}
	want := []string{"Первое предложение. ", "Второе предложение! ", "И третье?"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("segments %q, want %q", got, want)
	}
}

// The gate. A converter of our own is only allowed to replace kepubify if it
// produces the same spans over the same text, because those ids are where every
// reader's position is stored — a book reconverted with different ids loses
// everyone's place in it.
//
// Every rule above is checked against ours as well, so the two are held to one
// standard rather than to a description of one.
func TestOurConverterMatchesKepubifySpanForSpan(t *testing.T) {
	theirs := convertFixtures(t, newLibConverter())
	ours := convertFixtures(t, newNativeConverter())

	for _, c := range chapters {
		want := spansOf(t, theirs[c.name])
		got := spansOf(t, ours[c.name])

		if len(got) != len(want) {
			t.Errorf("%s: %d spans, want %d\n  ours:   %+v\n  theirs: %+v",
				c.name, len(got), len(want), got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: span %d is %+v, want %+v", c.name, i, got[i], want[i])
			}
		}
	}
}

// And ours has to obey the rules on its own terms, not only by agreeing.
func TestOurConverterObeysTheRules(t *testing.T) {
	converted := convertFixtures(t, newNativeConverter())

	for _, c := range chapters {
		xhtml, ok := converted[c.name]
		if !ok {
			t.Errorf("%s: chapter missing from the converted book", c.name)
			continue
		}

		if squash(visibleText(t, "<body>"+c.body+"</body>")) != squash(visibleText(t, xhtml)) {
			t.Errorf("%s: the text changed\n%s", c.name, xhtml)
		}

		dec := xml.NewDecoder(strings.NewReader(xhtml))
		dec.Strict = true
		dec.Entity = xml.HTMLEntity
		for {
			_, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("%s: not well-formed XML: %v\n%s", c.name, err, xhtml)
				break
			}
		}

		for _, want := range []string{`<div id="book-columns">`, `<div id="book-inner">`,
			`class="kobostylehacks"`} {
			if !strings.Contains(xhtml, want) {
				t.Errorf("%s: missing %s", c.name, want)
			}
		}
	}
}
