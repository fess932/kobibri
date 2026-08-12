package store

import (
	"database/sql"
	"time"
)

// TimeFormat is how every timestamp is stored: RFC3339 in UTC, so string
// comparison and time comparison agree.
const TimeFormat = time.RFC3339

// Now returns the current time in storage form.
func Now() string { return time.Now().UTC().Format(TimeFormat) }

// FormatTime renders t in storage form; the zero time becomes "".
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(TimeFormat)
}

// ParseTime reads a stored timestamp, returning the zero time for "".
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(TimeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// Source is one Calibre library kobibri reads from.
type Source struct {
	ID               int64
	Name             string
	LibraryPath      string
	Priority         int
	Enabled          bool
	ShareAll         bool
	ScanIntervalSec  int
	LastScanAt       string
	LastOKScanAt     string
	LastStatus       string
	LastError        string
	ConsecutiveFails int
	BookCount        int
	CreatedAt        string
}

// Source status values.
const (
	SourceStatusNever       = "never"
	SourceStatusRunning     = "running"
	SourceStatusOK          = "ok"
	SourceStatusUnreachable = "unreachable"
	SourceStatusError       = "error"
	SourceStatusSuspicious  = "suspicious"
)

// SourceBook is one Calibre row as kobibri stored it. Rows are never deleted by
// ingest: a book that disappears from the library is flagged Missing so that
// canonical ids and history survive.
type SourceBook struct {
	ID                  int64
	SourceID            int64
	CalibreID           int64
	CalibreUUID         string
	Title               string
	SortTitle           string
	AuthorsJSON         string
	AuthorSort          string
	SeriesName          string
	SeriesIndex         sql.NullFloat64
	DescriptionHTML     string
	Publisher           string
	PublishedAt         string
	Language            string
	ISBN13              string
	IdentifiersJSON     string
	TagsJSON            string
	RelPath             string
	CoverRelPath        string
	CoverMtime          int64
	CalibreLastModified string
	MetaHash            string
	BookID              string
	Missing             bool
	FirstSeenAt         string
	LastSeenAt          string
}

// SourceBookFile is one row of Calibre's `data` table as kobibri stored it.
type SourceBookFile struct {
	ID           int64
	SourceBookID int64
	Format       string
	RelPath      string
	Size         int64
	FileMtime    int64
	Layout       string
	EPUBVersion  string
	ProbedMtime  int64
	Present      bool
}

// Layout values for SourceBookFile.
const (
	LayoutUnknown      = ""
	LayoutReflowable   = "reflowable"
	LayoutPrePaginated = "pre-paginated"
)

// Book is the canonical merged book. Its ID is the only identifier a device
// ever sees, so it is issued once and never deleted or reissued.
type Book struct {
	ID                  string
	MergedInto          string
	Title               string
	SortTitle           string
	AuthorsJSON         string
	AuthorSort          string
	SeriesName          string
	SeriesIndex         sql.NullFloat64
	SeriesUUID          string
	DescriptionHTML     string
	Publisher           string
	PublishedAt         string
	Language            string
	ISBN13              string
	PrimarySourceBookID sql.NullInt64
	CoverSourceBookID   sql.NullInt64
	CoverImageID        string
	DownloadFormat      string
	DownloadSize        int64
	Available           bool
	Hidden              bool
	Syncable            bool
	ServingHash         string
	MetadataRev         int64
	CreatedAt           string
	UpdatedAt           string
	LastAvailableAt     string
}

// Download formats offered to a device. Exactly one is ever advertised per
// book: offering both KEPUB and EPUB invites the device to pick EPUB and lose
// span-level reading progress. See docs/kobo-protocol.md §7.
const (
	FormatKEPUB   = "KEPUB"
	FormatEPUB3FL = "EPUB3FL"
)
