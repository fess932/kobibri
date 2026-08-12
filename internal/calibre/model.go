package calibre

import (
	"strings"
	"time"
)

// Stub is the cheap per-book row read on every scan. Comparing stubs against
// what we already stored is what detects new, changed and vanished books
// without touching the rest of the schema.
type Stub struct {
	ID           int64
	UUID         string
	LastModified time.Time
}

// Author keeps Calibre's display name and its sort form; the sort form is what
// identity matching normalises, because it is "Lastname, Firstname".
type Author struct {
	Name string
	Sort string
}

// Book is one fully-read Calibre row plus its linked tables.
type Book struct {
	ID           int64
	UUID         string
	Title        string
	SortTitle    string
	Authors      []Author
	AuthorSort   string
	SeriesName   string
	SeriesIndex  float64
	HasSeries    bool
	Description  string
	Publisher    string
	Languages    []string
	Tags         []string
	Identifiers  map[string]string
	PubDate      time.Time
	Timestamp    time.Time
	LastModified time.Time
	HasCover     bool

	// RelPath is Calibre's books.path, always slash-separated.
	RelPath string
	// CoverRelPath is set only when the cover file was found on disk.
	CoverRelPath string
	CoverMtime   int64
	CoverSize    int64

	Formats []Format
}

// Format is one row of Calibre's `data` table, resolved against the filesystem.
type Format struct {
	Format string // upper case: EPUB, KEPUB, PDF, AZW3...
	Name   string // filename without extension
	Size   int64  // Calibre's uncompressed_size, or the real size once statted

	RelPath string // slash-separated, relative to the library root
	Mtime   int64
	Present bool // the file actually exists on disk; Calibre's DB routinely lies
}

// AuthorNames returns the display names in Calibre's link order.
func (b *Book) AuthorNames() []string {
	out := make([]string, len(b.Authors))
	for i, a := range b.Authors {
		out[i] = a.Name
	}
	return out
}

// PrimaryAuthorSort returns the best available "Lastname, Firstname" form.
func (b *Book) PrimaryAuthorSort() string {
	if s := strings.TrimSpace(b.AuthorSort); s != "" {
		return s
	}
	if len(b.Authors) > 0 {
		if s := strings.TrimSpace(b.Authors[0].Sort); s != "" {
			return s
		}
		return strings.TrimSpace(b.Authors[0].Name)
	}
	return ""
}

// Format returns the named format, if the book has it.
func (b *Book) Format(format string) (Format, bool) {
	format = strings.ToUpper(format)
	for _, f := range b.Formats {
		if f.Format == format {
			return f, true
		}
	}
	return Format{}, false
}

// CustomColumn describes a user-defined Calibre column. v1 only lists them for
// the web UI; mapping one onto Kobo metadata is a later milestone.
type CustomColumn struct {
	ID         int64
	Label      string
	Name       string
	Datatype   string
	IsMultiple bool
}
