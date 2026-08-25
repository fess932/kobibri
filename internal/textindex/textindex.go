// Package textindex measures a book so a reported reading position becomes a
// number of words.
//
// The device sends whole-number percentages, which do not move at all inside a
// long book: half an hour of reading can arrive as ProgressPercent 3 four times
// over. What does move is Location — a spine file and a koboSpan id — and those
// spans are generated here, so where each one sits is knowable.
package textindex

import (
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/fess932/kobibri/internal/reader"
	"github.com/fess932/kobibri/internal/store"
)

// Build measures the file a device actually receives.
func Build(path, fingerprint string) (*store.TextIndex, error) {
	book, err := reader.Open(path)
	if err != nil {
		return nil, err
	}
	defer book.Close()

	ix := &store.TextIndex{Fingerprint: fingerprint}

	for i, ch := range book.Spine {
		rc, _, _, err := book.Open(ch.Path)
		if err != nil {
			continue
		}
		doc := measure(rc, ch.Path, ix.Words)
		rc.Close()

		ix.Docs = append(ix.Docs, store.TextDoc{
			Seq: i, Source: ch.Path, Title: ch.Title,
			Words: doc.words, Before: ix.Words,
		})
		ix.Blocks = append(ix.Blocks, doc.blocks...)
		ix.Words += doc.words
		if len(doc.blocks) > 0 {
			ix.Spanned = true
		}
	}
	return ix, nil
}

type measured struct {
	words  int
	blocks []store.TextBlock
}

// measure walks one content document, counting words and noting where each
// koboSpan block begins.
//
// It is read as a token stream rather than a tree: a book is XHTML but not
// reliably well-formed, and nothing here needs the structure. Text outside a
// span still counts towards the total, so a book converted by something that
// spans less than ours does is still measured correctly overall.
func measure(r io.Reader, source string, before int) measured {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity

	out := measured{}
	seen := map[int]bool{}
	skip := 0

	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch strings.ToLower(t.Name.Local) {
			case "head", "script", "style":
				skip++
			case "span":
				if block, ok := spanBlock(t); ok && !seen[block] {
					seen[block] = true
					out.blocks = append(out.blocks, store.TextBlock{
						Source: source, Block: block, Before: before + out.words,
					})
				}
			}
		case xml.EndElement:
			switch strings.ToLower(t.Name.Local) {
			case "head", "script", "style":
				if skip > 0 {
					skip--
				}
			}
		case xml.CharData:
			if skip == 0 {
				out.words += CountWords(string(t))
			}
		}
	}
}

func spanBlock(el xml.StartElement) (int, bool) {
	var id string
	kobo := false
	for _, a := range el.Attr {
		switch strings.ToLower(a.Name.Local) {
		case "class":
			kobo = strings.Contains(a.Value, "koboSpan")
		case "id":
			id = a.Value
		}
	}
	if !kobo {
		return 0, false
	}
	rest, ok := strings.CutPrefix(id, "kobo.")
	if !ok {
		return 0, false
	}
	block, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(block)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// CountWords counts what a reader gets through.
//
// Whitespace-separated runs, except that a Han, Hiragana or Katakana character
// is a word on its own: those scripts are written without spaces, and counting
// a whole paragraph as one word would put a reading speed at three words a
// minute.
func CountWords(s string) int {
	words, inWord := 0, false
	for _, r := range s {
		switch {
		case isIdeograph(r):
			words++
			inWord = false
		case unicode.IsSpace(r):
			inWord = false
		default:
			if !inWord {
				words++
				inWord = true
			}
		}
	}
	return words
}

func isIdeograph(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}
