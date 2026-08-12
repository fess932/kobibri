// Package ingest turns Calibre libraries into kobibri's canonical library:
// scanning sources, deciding which rows across sources are the same book, and
// choosing which source wins for metadata and files.
package ingest

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Identity kinds, strongest first. A book always yields at least one key, so it
// can always be attached to a canonical row.
const (
	KindCalibreUUID = "calibre_uuid"
	KindISBN        = "isbn"
	KindTitleAuthor = "titleauthor"
	// KindWebURL identifies a book imported from a link. Such a book has neither
	// a Calibre uuid nor an ISBN, and its link is the only thing about it that
	// is genuinely stable.
	KindWebURL = "weburl"
)

// IdentityKey addresses a canonical book. Two source rows sharing any key are
// the same book.
type IdentityKey struct {
	Kind string
	Key  string
}

// Keys returns the identity keys for one source row, strongest first.
//
// calibre_uuid dominates in practice: multi-source setups are usually clones or
// backups of one library. ISBN is next but only when the checksum validates —
// Calibre libraries are full of hand-typed garbage in that field. titleauthor
// is the weakest and exists so that every book has a key at all; it is also the
// only one that can produce a false merge (different translations, "Selected
// Poems"), which is why the web UI shows contributing sources per book.
func Keys(calibreUUID, isbnRaw, title, authorSort string) []IdentityKey {
	var keys []IdentityKey

	if u := strings.ToLower(strings.TrimSpace(calibreUUID)); u != "" {
		keys = append(keys, IdentityKey{Kind: KindCalibreUUID, Key: u})
	}
	if isbn := NormalizeISBN(isbnRaw); isbn != "" {
		keys = append(keys, IdentityKey{Kind: KindISBN, Key: isbn})
	}

	t, a := NormalizeTitle(title), NormalizeAuthor(authorSort)
	if t != "" {
		keys = append(keys, IdentityKey{Kind: KindTitleAuthor, Key: t + "|" + a})
	}
	return keys
}

var articles = map[string]bool{
	"a": true, "an": true, "the": true,
	"le": true, "la": true, "les": true,
	"el": true, "los": true, "las": true,
	"der": true, "die": true, "das": true,
	"il": true, "lo": true, "de": true, "het": true,
	"und": true,
}

// Normalize folds a string down to comparable form: decomposed, stripped of
// diacritics and punctuation, lower case, single-spaced.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}

	// NFKD then drop combining marks, so "Ерёма" and "Ерема", "café" and "cafe"
	// land on the same key.
	folded, _, err := transform.String(
		transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFKC), s)
	if err == nil {
		s = folded
	}

	s = strings.ReplaceAll(s, "&", " and ")

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// NormalizeTitle additionally drops a trailing parenthetical and a leading
// article, both of which routinely differ between two copies of one book.
func NormalizeTitle(title string) string {
	title = strings.TrimSpace(title)

	// "A Book (Unabridged)" -> "A Book". Only a trailing group is dropped; a
	// parenthetical in the middle is likely part of the real title.
	if end := strings.LastIndexByte(title, ')'); end == len(title)-1 && end > 0 {
		if start := strings.LastIndexByte(title[:end], '('); start > 0 {
			title = strings.TrimSpace(title[:start])
		}
	}

	s := Normalize(title)
	if first, rest, ok := strings.Cut(s, " "); ok && articles[first] {
		s = rest
	}
	return s
}

// NormalizeAuthor folds an author into comparable form. Input is expected to be
// the "Lastname, Firstname" sort form; a display name is accepted too, since
// normalisation drops the comma either way.
func NormalizeAuthor(author string) string {
	// Calibre joins multiple authors with " & "; identity uses the first only,
	// because a second copy of the book often lists fewer contributors.
	if i := strings.Index(author, "&"); i >= 0 {
		author = author[:i]
	}
	return Normalize(author)
}

// NormalizeISBN converts an ISBN-10 or ISBN-13 to canonical ISBN-13 digits,
// returning "" when the value is not a valid ISBN. Rejecting bad checksums
// matters: an invalid ISBN shared by two unrelated books would merge them.
func NormalizeISBN(raw string) string {
	var digits []byte
	for i := range len(raw) {
		c := raw[i]
		switch {
		case c >= '0' && c <= '9':
			digits = append(digits, c)
		case c == 'x' || c == 'X':
			digits = append(digits, 'X')
		}
	}

	switch len(digits) {
	case 10:
		if !validISBN10(digits) {
			return ""
		}
		return isbn10To13(digits)
	case 13:
		if strings.ContainsRune(string(digits), 'X') || !validISBN13(digits) {
			return ""
		}
		return string(digits)
	default:
		return ""
	}
}

func validISBN10(d []byte) bool {
	sum := 0
	for i := range 10 {
		var v int
		switch {
		case d[i] == 'X':
			// X is only legal as the check digit.
			if i != 9 {
				return false
			}
			v = 10
		default:
			v = int(d[i] - '0')
		}
		sum += v * (10 - i)
	}
	return sum%11 == 0
}

func validISBN13(d []byte) bool {
	sum := 0
	for i := range 13 {
		v := int(d[i] - '0')
		if i%2 == 1 {
			v *= 3
		}
		sum += v
	}
	return sum%10 == 0
}

func isbn10To13(d []byte) string {
	body := append([]byte("978"), d[:9]...)
	sum := 0
	for i := range 12 {
		v := int(body[i] - '0')
		if i%2 == 1 {
			v *= 3
		}
		sum += v
	}
	check := (10 - sum%10) % 10
	return string(body) + string(byte('0'+check))
}
