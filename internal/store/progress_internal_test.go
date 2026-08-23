package store

import "testing"

// Both percentages a Kobo sends are 0-100 and only one of them is the book.
//
// ProgressPercent is the whole book; ContentSourceProgressPercent is how far
// into the current spine file the reader is. Preferring the second showed a
// reader 19 pages into a 760-page book as 42% done, because they were 42% of
// the way through one chapter file. Payloads below are verbatim from a Kobo
// Libra Colour, fw 4.45.23697.
func TestPercentOfPrefersTheWholeBook(t *testing.T) {
	tests := []struct {
		name     string
		bookmark string
		want     float64
	}{
		{
			"19 pages of 760, deep into one chapter file",
			`{"ContentSourceProgressPercent":42,"ProgressPercent":2,` +
				`"Location":{"Source":"index_split_003.xhtml","Type":"KoboSpan","Value":"kobo.1.76"}}`,
			2,
		},
		{
			// The same reader on another book, at the start of a new file.
			"70% in, at the top of a chapter",
			`{"ContentSourceProgressPercent":0,"ProgressPercent":70}`,
			70,
		},
		{
			// Zero and absent are different answers: one page in is not
			// "the field was not sent".
			"one page in",
			`{"ContentSourceProgressPercent":8,"ProgressPercent":0}`,
			0,
		},
		{
			"only the content-source figure sent",
			`{"ContentSourceProgressPercent":37}`,
			37,
		},
		{"empty", "", 0},
		{"null", "null", 0},
		{"not json", "{", 0},
	}

	for _, tt := range tests {
		if got := percentOf(tt.bookmark); got != tt.want {
			t.Errorf("%s: percentOf() = %v, want %v", tt.name, got, tt.want)
		}
	}
}
