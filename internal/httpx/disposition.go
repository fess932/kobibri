package httpx

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ContentDisposition builds the header a download is saved under.
//
// It carries the name twice, which is what the format is for. `filename*` is the
// real one, percent-encoded UTF-8 per RFC 5987, and every browser in use reads
// it. `filename` is the fallback for anything that does not, so it is ASCII —
// transliterated rather than blanked out, because a file called ________.epub
// helps nobody.
//
// The encoding is percent, not the `=?utf-8?q?…?=` form: that belongs to mail
// headers, and a browser handed one shows it literally as the filename.
func ContentDisposition(filename string) string {
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
		asciiName(filename), percentEncode(filename))
}

// percentEncode escapes everything outside the attr-char set of RFC 5987. That
// set is deliberately narrow — quotes, spaces and separators all have meaning in
// a header — so anything not plainly safe is encoded.
func percentEncode(s string) string {
	const safe = "!#$&+-.^_`|~"

	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// asciiName makes a readable ASCII version: Cyrillic is transliterated, anything
// else that will not fit becomes a dash, and runs of dashes collapse.
func asciiName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\\' || r < 0x20:
			// Would break the quoted string, or the header.
			b.WriteByte('-')
		case r < 0x80:
			b.WriteRune(r)
		default:
			if latin, ok := transliterate[r]; ok {
				b.WriteString(latin)
			} else {
				b.WriteByte('-')
			}
		}
	}

	out := collapse(b.String())
	if strings.Trim(out, "-_ .") == "" {
		return "book" + filepath.Ext(s)
	}
	return out
}

func collapse(s string) string {
	var b strings.Builder
	var lastDash bool
	for _, r := range s {
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "- ")
}

// transliterate covers Cyrillic, which is what this library is mostly full of.
// Anything else falls back to a dash; the real name is in `filename*` regardless.
var transliterate = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",

	'А': "A", 'Б': "B", 'В': "V", 'Г': "G", 'Д': "D", 'Е': "E", 'Ё': "Yo",
	'Ж': "Zh", 'З': "Z", 'И': "I", 'Й': "Y", 'К': "K", 'Л': "L", 'М': "M",
	'Н': "N", 'О': "O", 'П': "P", 'Р': "R", 'С': "S", 'Т': "T", 'У': "U",
	'Ф': "F", 'Х': "H", 'Ц': "Ts", 'Ч': "Ch", 'Ш': "Sh", 'Щ': "Sch",
	'Ъ': "", 'Ы': "Y", 'Ь': "", 'Э': "E", 'Ю': "Yu", 'Я': "Ya",

	// Ukrainian and Belarusian letters that share the alphabet.
	'і': "i", 'І': "I", 'ї': "yi", 'Ї': "Yi", 'є': "ye", 'Є': "Ye",
	'ґ': "g", 'Ґ': "G", 'ў': "u", 'Ў': "U",

	// Punctuation that turns up in titles.
	'—': "-", '–': "-", '«': "", '»': "", '„': "", '“': "", '”': "",
	'’': "'", '‘': "'", '…': "...", '№': "No",
}
