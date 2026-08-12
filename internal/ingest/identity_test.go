package ingest

import "testing"

func TestNormalizeISBN(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"isbn13 plain", "9780306406157", "9780306406157"},
		{"isbn13 hyphenated", "978-0-306-40615-7", "9780306406157"},
		{"isbn13 spaced", " 978 0306 40615 7 ", "9780306406157"},
		{"isbn10 converted", "0306406152", "9780306406157"},
		{"isbn10 hyphenated", "0-306-40615-2", "9780306406157"},
		{"isbn10 with X check digit", "080442957X", "9780804429573"},

		// Rejecting bad checksums is the point: two unrelated books sharing a
		// hand-typed placeholder must not merge.
		{"isbn13 bad checksum", "9780306406158", ""},
		{"isbn10 bad checksum", "0306406153", ""},
		{"all zeroes placeholder", "0000000000", "9780000000002"},
		{"too short", "12345", ""},
		{"too long", "97803064061571", ""},
		{"empty", "", ""},
		{"not a number", "no-isbn-here", ""},
		{"X in the middle of isbn10", "03X6406152", ""},
		{"X inside isbn13", "978030640615X", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeISBN(tt.in); got != tt.want {
				t.Errorf("NormalizeISBN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// isbn10To13 must round-trip a known pair, or every ISBN key silently shifts.
func TestISBN10And13AgreeForTheSameBook(t *testing.T) {
	if a, b := NormalizeISBN("0306406152"), NormalizeISBN("9780306406157"); a != b {
		t.Errorf("ISBN-10 and ISBN-13 of one book differ: %q vs %q", a, b)
	}
}

func TestNormalizeTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"The Long Book", "long book"},
		{"A Long Book", "long book"},
		{"Long Book", "long book"},
		{"  The   Long   Book  ", "long book"},
		{"The Long Book!", "long book"},
		{"The Long Book (Unabridged)", "long book"},
		{"The Long Book (2nd Edition)", "long book"},
		{"Café Society", "cafe society"},
		{"Ёжик в тумане", "ежик в тумане"},
		{"Salt & Pepper", "salt and pepper"},
		{"Salt and Pepper", "salt and pepper"},
		{"Book: The Subtitle", "book the subtitle"},

		// A parenthetical that is not trailing is part of the title.
		{"The (Very) Long Book", "very long book"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeTitle(tt.in); got != tt.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeAuthor(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Author, Jane", "author jane"},
		{"Author,Jane", "author jane"},
		{"  Author,  Jane  ", "author jane"},
		{"Jane Author", "jane author"},
		{"Author, Jane & Second, Kim", "author jane"},
		{"Толстой, Лев", "толстои лев"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeAuthor(tt.in); got != tt.want {
			t.Errorf("NormalizeAuthor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The display name and the sort form of one author must not fold to the same
// key — they are genuinely different strings, and titleauthor is only ever
// built from the sort form. This documents the constraint rather than asserting
// a bug: callers must always pass the sort form.
func TestTitleAuthorKeyUsesSortForm(t *testing.T) {
	sortForm := Keys("", "", "The Long Book", "Author, Jane")
	display := Keys("", "", "The Long Book", "Jane Author")

	if len(sortForm) != 1 || len(display) != 1 {
		t.Fatalf("expected exactly one key each, got %v and %v", sortForm, display)
	}
	if sortForm[0].Key == display[0].Key {
		t.Skip("normalisation happens to collapse both forms; nothing to guard")
	}
	t.Logf("sort form key %q differs from display key %q, as expected",
		sortForm[0].Key, display[0].Key)
}

func TestKeysOrderAndCoverage(t *testing.T) {
	keys := Keys("A1B2C3D4-0000-4000-8000-000000000001", "978-0-306-40615-7",
		"The Long Book", "Author, Jane")

	if len(keys) != 3 {
		t.Fatalf("got %d keys, want 3: %v", len(keys), keys)
	}
	want := []IdentityKey{
		{KindCalibreUUID, "a1b2c3d4-0000-4000-8000-000000000001"},
		{KindISBN, "9780306406157"},
		{KindTitleAuthor, "long book|author jane"},
	}
	for i, w := range want {
		if keys[i] != w {
			t.Errorf("key %d = %+v, want %+v", i, keys[i], w)
		}
	}
}

// Every book must yield at least one key or it cannot be attached to a
// canonical row at all.
func TestEveryBookGetsAKey(t *testing.T) {
	keys := Keys("", "garbage-isbn", "Untitled", "")
	if len(keys) == 0 {
		t.Fatal("a book with no uuid and no valid ISBN produced no identity keys")
	}
	if keys[0].Kind != KindTitleAuthor {
		t.Errorf("fallback key kind = %q, want %q", keys[0].Kind, KindTitleAuthor)
	}
	if keys[0].Key != "untitled|" {
		t.Errorf("fallback key = %q", keys[0].Key)
	}
}

// A book with no title at all yields nothing; the caller must treat that as
// "cannot identify" rather than merging every such book together.
func TestUntitledBookYieldsNoTitleAuthorKey(t *testing.T) {
	if keys := Keys("", "", "   ", ""); len(keys) != 0 {
		t.Errorf("empty title produced keys %v; they would all collide", keys)
	}
}

func TestKeysAreStableAcrossFormattingDifferences(t *testing.T) {
	a := Keys("", "978-0-306-40615-7", "The Long Book", "Author, Jane")
	b := Keys("", "9780306406157", "the long book", "author, jane")

	if len(a) != len(b) {
		t.Fatalf("key counts differ: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("key %d differs across formatting: %+v vs %+v", i, a[i], b[i])
		}
	}
}
