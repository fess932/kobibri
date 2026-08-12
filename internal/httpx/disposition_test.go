package httpx

import (
	"mime"
	"strings"
	"testing"
)

// The header has to survive being parsed back. A name encoded the wrong way is
// not rejected by anything — it is simply shown to the person, character for
// character, as the name of their download.
func TestContentDispositionRoundTrips(t *testing.T) {
	tests := []string{
		"Долгие сумерки Земли - Брайан Олдисс.kepub.epub",
		"Рупеджия (Новелла) - Manasong.kepub.epub",
		"Plain English Title.epub",
		`Awkward "quoted" \ name.epub`,
		"Ünicode Ärger.epub",
	}

	for _, name := range tests {
		header := ContentDisposition(name)

		_, params, err := mime.ParseMediaType(header)
		if err != nil {
			t.Errorf("%q produced a header that will not parse: %v\n  %s", name, err, header)
			continue
		}
		// Go folds the RFC 2231 extended parameter into the plain name once it
		// has decoded it, which is exactly what a browser does.
		if got := params["filename"]; got != name {
			t.Errorf("the name round-tripped to %q, want %q", got, name)
		}
		// The literal =?utf-8?q?…?= form belongs to mail headers. A browser
		// handed one shows it as the filename, which is exactly the bug this
		// guards.
		if strings.Contains(header, "=?utf-8?") || strings.Contains(header, "=?UTF-8?") {
			t.Errorf("the header carries a mail-style encoded word:\n  %s", header)
		}
	}
}

// The ASCII fallback is read by anything that ignores filename*, so it has to be
// a name rather than a row of underscores.
func TestTheASCIIFallbackIsReadable(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Долгие сумерки Земли.epub", "Dolgie sumerki Zemli.epub"},
		{"Рупеджия (Новелла) - Manasong.kepub.epub", "Rupedzhiya (Novella) - Manasong.kepub.epub"},
		{"Plain English.epub", "Plain English.epub"},
		{"Ёжик №1 «Тест».epub", "Yozhik No1 Test.epub"},
	}

	for _, tt := range tests {
		if got := asciiName(tt.in); got != tt.want {
			t.Errorf("asciiName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A name with nothing usable left still has to be a filename.
func TestAnUnusableNameFallsBackToSomething(t *testing.T) {
	got := asciiName("日本語.epub")
	if !strings.HasSuffix(got, ".epub") {
		t.Errorf("asciiName lost the extension: %q", got)
	}
	if strings.Trim(got, "-. ") == "" {
		t.Errorf("asciiName produced nothing usable: %q", got)
	}
}

// The quoted fallback must not be able to end the quoted string early.
func TestTheFallbackCannotBreakTheHeader(t *testing.T) {
	header := ContentDisposition(`evil".epub`)
	if _, _, err := mime.ParseMediaType(header); err != nil {
		t.Fatalf("a quote in the name broke the header: %v\n  %s", err, header)
	}
}
