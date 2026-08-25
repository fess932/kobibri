// Package calibretest builds throwaway Calibre libraries on disk: a real
// metadata.db with Calibre's schema, a real directory tree, and real (tiny)
// EPUB files. Ingest and sync tests depend on it, so it lives outside _test.go.
package calibretest

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// schema is the subset of Calibre's DDL that kobibri reads. Column types and
// defaults are kept as Calibre writes them so the reader is exercised against
// realistic values (TEXT timestamps, NOCASE collations, nullable sort columns).
const schema = `
CREATE TABLE books (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL DEFAULT 'Unknown',
  sort TEXT COLLATE NOCASE,
  timestamp TIMESTAMP,
  pubdate TIMESTAMP,
  series_index REAL NOT NULL DEFAULT 1.0,
  author_sort TEXT COLLATE NOCASE,
  isbn TEXT DEFAULT '' COLLATE NOCASE,
  lccn TEXT DEFAULT '' COLLATE NOCASE,
  path TEXT NOT NULL DEFAULT '',
  flags INTEGER NOT NULL DEFAULT 1,
  uuid TEXT,
  has_cover BOOL DEFAULT 0,
  last_modified TIMESTAMP NOT NULL DEFAULT '2000-01-01 00:00:00+00:00'
);
CREATE TABLE authors (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE,
  sort TEXT, link TEXT NOT NULL DEFAULT ''
);
CREATE TABLE books_authors_link (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL, author INTEGER NOT NULL
);
CREATE TABLE series (id INTEGER PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE, sort TEXT);
CREATE TABLE books_series_link (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL, series INTEGER NOT NULL
);
CREATE TABLE publishers (id INTEGER PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE, sort TEXT);
CREATE TABLE books_publishers_link (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL, publisher INTEGER NOT NULL
);
CREATE TABLE languages (id INTEGER PRIMARY KEY, lang_code TEXT NOT NULL COLLATE NOCASE);
CREATE TABLE books_languages_link (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL, lang_code INTEGER NOT NULL,
  item_order INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL COLLATE NOCASE);
CREATE TABLE books_tags_link (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL, tag INTEGER NOT NULL
);
CREATE TABLE comments (id INTEGER PRIMARY KEY, book INTEGER NOT NULL, text TEXT NOT NULL COLLATE NOCASE);
CREATE TABLE data (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL, format TEXT NOT NULL COLLATE NOCASE,
  uncompressed_size INTEGER NOT NULL, name TEXT NOT NULL
);
CREATE TABLE identifiers (
  id INTEGER PRIMARY KEY, book INTEGER NOT NULL,
  type TEXT NOT NULL DEFAULT 'isbn' COLLATE NOCASE, val TEXT NOT NULL COLLATE NOCASE
);
CREATE TABLE custom_columns (
  id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT NOT NULL, name TEXT NOT NULL,
  datatype TEXT NOT NULL, mark_for_delete BOOL DEFAULT 0 NOT NULL,
  editable BOOL DEFAULT 1 NOT NULL, display TEXT DEFAULT '{}' NOT NULL,
  is_multiple BOOL DEFAULT 0 NOT NULL, normalized BOOL NOT NULL
);
`

// BookSpec describes one book to create.
type BookSpec struct {
	Title       string
	Authors     []string // display names, "Firstname Lastname"
	AuthorSort  string   // defaults to a sort form of the first author
	UUID        string   // defaults to a deterministic uuid derived from the title
	Series      string
	SeriesIndex float64
	Description string
	Publisher   string
	Languages   []string
	Tags        []string
	// Columns are values of the library's own custom columns, keyed by label
	// without the leading "#". The column is created on first use, normalized
	// the way Calibre creates a multi-value text column.
	Columns      map[string][]string
	Identifiers  map[string]string
	PubDate      time.Time
	LastModified time.Time

	// Formats to write. The key is the format name (EPUB, PDF, ...); the value
	// selects the file's content shape.
	Formats []FormatSpec

	// Cover writes a cover.jpg and sets has_cover.
	Cover bool
	// CoverInDBOnly sets has_cover without writing the file, reproducing a
	// library where the cover was deleted by hand.
	CoverInDBOnly bool
}

// FormatSpec is one entry in Calibre's `data` table.
type FormatSpec struct {
	Format string // EPUB, KEPUB, PDF, AZW3...
	// Kind selects the generated content: "reflowable" (default),
	// "pre-paginated", "epub2", "broken" (not a valid zip).
	Kind string
	// Missing records the row in the database without writing the file, which
	// is what a library looks like after files are moved by hand.
	Missing bool
}

// Library is a Calibre library on disk.
type Library struct {
	Path string
	t    *testing.T
}

// New creates a library containing the given books and returns it.
func New(t *testing.T, books ...BookSpec) *Library {
	t.Helper()
	return NewAt(t, t.TempDir(), books...)
}

// NewAt creates a library at a specific directory, which lets a test build two
// libraries that deliberately overlap.
func NewAt(t *testing.T, dir string, books ...BookSpec) *Library {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}

	lib := &Library{Path: dir, t: t}
	db := lib.openDB()
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create calibre schema: %v", err)
	}
	for i, spec := range books {
		lib.insert(db, int64(i+1), spec)
	}
	return lib
}

// Add appends a book to an existing library, as if the user imported it.
func (l *Library) Add(spec BookSpec) int64 {
	l.t.Helper()
	db := l.openDB()
	defer func() { _ = db.Close() }()

	var maxID int64
	if err := db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM books`).Scan(&maxID); err != nil {
		l.t.Fatalf("max id: %v", err)
	}
	id := maxID + 1
	l.insert(db, id, spec)
	return id
}

// Touch bumps a book's last_modified, which is how a scan notices a change.
func (l *Library) Touch(id int64, at time.Time) {
	l.t.Helper()
	l.Exec(`UPDATE books SET last_modified = ? WHERE id = ?`, formatTime(at), id)
}

// Remove deletes a book row and its links, as if the user deleted the book.
func (l *Library) Remove(id int64) {
	l.t.Helper()
	for _, q := range []string{
		`DELETE FROM books WHERE id = ?`,
		`DELETE FROM books_authors_link WHERE book = ?`,
		`DELETE FROM books_series_link WHERE book = ?`,
		`DELETE FROM books_publishers_link WHERE book = ?`,
		`DELETE FROM books_languages_link WHERE book = ?`,
		`DELETE FROM books_tags_link WHERE book = ?`,
		`DELETE FROM comments WHERE book = ?`,
		`DELETE FROM identifiers WHERE book = ?`,
		`DELETE FROM data WHERE book = ?`,
	} {
		l.Exec(q, id)
	}
}

// Exec runs a statement against the library database, for tests that need to
// bend it into an unusual shape.
func (l *Library) Exec(query string, args ...any) {
	l.t.Helper()
	db := l.openDB()
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(query, args...); err != nil {
		l.t.Fatalf("exec %q: %v", query, err)
	}
}

// LeaveDirtyWAL commits a change that stays in metadata.db-wal rather than the
// main database file, reproducing a library Calibre is holding open. A reader
// that copies metadata.db without its -wal sidecar reads back the stale value.
//
// The connection is deliberately kept open until the test ends: SQLite
// checkpoints and deletes the WAL when the last connection closes, which would
// destroy exactly the condition under test.
func (l *Library) LeaveDirtyWAL() {
	l.t.Helper()

	db, err := sql.Open("sqlite",
		"file:"+l.dbPath()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		l.t.Fatalf("open for dirty wal: %v", err)
	}
	db.SetMaxOpenConns(1)
	l.t.Cleanup(func() { _ = db.Close() })

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		l.t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		l.t.Fatalf("journal_mode = %q, want wal", mode)
	}
	if _, err := db.Exec(`UPDATE books SET last_modified = '2030-01-01 00:00:00+00:00'`); err != nil {
		l.t.Fatalf("dirty wal write: %v", err)
	}

	if _, err := os.Stat(l.dbPath() + "-wal"); err != nil {
		l.t.Fatalf("no -wal file after a WAL-mode write: %v", err)
	}
}

func (l *Library) dbPath() string { return filepath.Join(l.Path, "metadata.db") }

func (l *Library) openDB() *sql.DB {
	l.t.Helper()
	db, err := sql.Open("sqlite", "file:"+l.dbPath()+"?_pragma=foreign_keys(0)")
	if err != nil {
		l.t.Fatalf("open metadata.db: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func (l *Library) insert(db *sql.DB, id int64, spec BookSpec) {
	l.t.Helper()
	spec = spec.withDefaults(id)

	bookDir := filepath.Join(spec.Authors[0], fmt.Sprintf("%s (%d)", spec.Title, id))
	relPath := filepath.ToSlash(bookDir)
	absDir := filepath.Join(l.Path, bookDir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		l.t.Fatalf("mkdir book dir: %v", err)
	}

	hasCover := spec.Cover || spec.CoverInDBOnly
	l.mustExec(db, `INSERT INTO books
		(id, title, sort, timestamp, pubdate, series_index, author_sort, path, uuid, has_cover, last_modified)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, spec.Title, sortTitle(spec.Title), formatTime(spec.LastModified),
		formatTime(spec.PubDate), spec.SeriesIndex, spec.AuthorSort, relPath,
		spec.UUID, boolInt(hasCover), formatTime(spec.LastModified))

	for _, name := range spec.Authors {
		authorID := l.upsert(db, "authors", name, sortAuthor(name))
		l.mustExec(db, `INSERT INTO books_authors_link (book, author) VALUES (?,?)`, id, authorID)
	}
	if spec.Series != "" {
		seriesID := l.upsert(db, "series", spec.Series, spec.Series)
		l.mustExec(db, `INSERT INTO books_series_link (book, series) VALUES (?,?)`, id, seriesID)
	}
	if spec.Publisher != "" {
		pubID := l.upsert(db, "publishers", spec.Publisher, spec.Publisher)
		l.mustExec(db, `INSERT INTO books_publishers_link (book, publisher) VALUES (?,?)`, id, pubID)
	}
	for i, lang := range spec.Languages {
		langID := l.upsertLang(db, lang)
		l.mustExec(db, `INSERT INTO books_languages_link (book, lang_code, item_order) VALUES (?,?,?)`,
			id, langID, i)
	}
	for _, tag := range spec.Tags {
		tagID := l.upsertTag(db, tag)
		l.mustExec(db, `INSERT INTO books_tags_link (book, tag) VALUES (?,?)`, id, tagID)
	}
	for label, values := range spec.Columns {
		l.setColumn(db, id, label, values)
	}
	if spec.Description != "" {
		l.mustExec(db, `INSERT INTO comments (book, text) VALUES (?,?)`, id, spec.Description)
	}
	for typ, val := range spec.Identifiers {
		l.mustExec(db, `INSERT INTO identifiers (book, type, val) VALUES (?,?,?)`, id, typ, val)
	}

	fileBase := fmt.Sprintf("%s - %s", spec.Title, spec.Authors[0])
	for _, f := range spec.Formats {
		content := epubBytes(l.t, f.Kind, spec.Title)
		l.mustExec(db, `INSERT INTO data (book, format, uncompressed_size, name) VALUES (?,?,?,?)`,
			id, strings.ToUpper(f.Format), len(content), fileBase)
		if f.Missing {
			continue
		}
		name := fileBase + "." + strings.ToLower(f.Format)
		if err := os.WriteFile(filepath.Join(absDir, name), content, 0o644); err != nil {
			l.t.Fatalf("write %s: %v", name, err)
		}
	}

	if spec.Cover {
		if err := os.WriteFile(filepath.Join(absDir, "cover.jpg"), coverBytes(l.t), 0o644); err != nil {
			l.t.Fatalf("write cover: %v", err)
		}
	}
}

func (spec BookSpec) withDefaults(id int64) BookSpec {
	if spec.Title == "" {
		spec.Title = fmt.Sprintf("Book %d", id)
	}
	if len(spec.Authors) == 0 {
		spec.Authors = []string{"Unknown Author"}
	}
	if spec.AuthorSort == "" {
		spec.AuthorSort = sortAuthor(spec.Authors[0])
	}
	if spec.UUID == "" {
		spec.UUID = deterministicUUID(spec.Title)
	}
	if spec.SeriesIndex == 0 {
		spec.SeriesIndex = 1.0
	}
	if spec.LastModified.IsZero() {
		spec.LastModified = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}
	if spec.PubDate.IsZero() {
		spec.PubDate = time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)
	}
	if spec.Formats == nil {
		spec.Formats = []FormatSpec{{Format: "EPUB"}}
	}
	return spec
}

func (l *Library) upsert(db *sql.DB, table, name, sort string) int64 {
	l.t.Helper()
	var id int64
	err := db.QueryRow(`SELECT id FROM `+table+` WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return id
	}
	res := l.mustExec(db, `INSERT INTO `+table+` (name, sort) VALUES (?,?)`, name, sort)
	id, _ = res.LastInsertId()
	return id
}

func (l *Library) upsertLang(db *sql.DB, code string) int64 {
	l.t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM languages WHERE lang_code = ?`, code).Scan(&id); err == nil {
		return id
	}
	res := l.mustExec(db, `INSERT INTO languages (lang_code) VALUES (?)`, code)
	id, _ = res.LastInsertId()
	return id
}

func (l *Library) upsertTag(db *sql.DB, name string) int64 {
	l.t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id); err == nil {
		return id
	}
	res := l.mustExec(db, `INSERT INTO tags (name) VALUES (?)`, name)
	id, _ = res.LastInsertId()
	return id
}

func (l *Library) mustExec(db *sql.DB, query string, args ...any) sql.Result {
	l.t.Helper()
	res, err := db.Exec(query, args...)
	if err != nil {
		l.t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

// epubBytes builds a minimal but structurally valid EPUB. "broken" returns
// bytes that are not a zip at all, which is what a truncated download looks like.
func epubBytes(t *testing.T, kind, title string) []byte {
	t.Helper()
	switch kind {
	case "broken":
		return []byte("this is not a zip file")
	case "fb2":
		// A real FB2 rather than an EPUB, for testing what happens to the
		// formats a library holds instead of EPUB.
		return fb2Bytes(title)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	write := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}

	write("mimetype", "application/epub+zip")
	write("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)

	version := "3.0"
	layoutMeta := ""
	spineProps := ""
	switch kind {
	case "epub2":
		version = "2.0"
	case "pre-paginated":
		layoutMeta = `<meta property="rendition:layout">pre-paginated</meta>`
		spineProps = ` properties="rendition:layout-pre-paginated"`
	}

	write("OEBPS/content.opf", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="%s" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:language>en</dc:language>
    %s
  </metadata>
  <manifest>
    <item id="c1" href="chapter1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="c1"%s/>
  </spine>
</package>`, version, title, layoutMeta, spineProps))

	write("OEBPS/chapter1.xhtml", `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Chapter</title></head>
<body><p>The first paragraph. The second sentence follows it.</p></body></html>`)

	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func coverBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 60, 80))
	for y := range 80 {
		for x := range 60 {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 3), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode cover: %v", err)
	}
	return buf.Bytes()
}

// formatTime writes the shape Calibre itself writes.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05.000000-07:00")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sortTitle(title string) string {
	for _, article := range []string{"The ", "A ", "An "} {
		if rest, ok := strings.CutPrefix(title, article); ok {
			return rest + ", " + strings.TrimSpace(article)
		}
	}
	return title
}

func sortAuthor(name string) string {
	parts := strings.Fields(name)
	if len(parts) < 2 {
		return name
	}
	last := parts[len(parts)-1]
	return last + ", " + strings.Join(parts[:len(parts)-1], " ")
}

// deterministicUUID derives a stable uuid from a seed so two fixtures can be
// made to share one on purpose.
func deterministicUUID(seed string) string {
	var h uint64 = 1469598103934665603
	for i := range len(seed) {
		h ^= uint64(seed[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x",
		uint32(h>>32), uint16(h>>16), uint16(h)&0xfff,
		uint16(h>>48)&0xfff, h&0xffffffffffff)
}

// setColumn writes a custom column value, creating the column the way Calibre
// does: a normalized multi-value text column, with its values in a table of
// their own and a link table beside it.
func (l *Library) setColumn(db *sql.DB, book int64, label string, values []string) {
	l.t.Helper()

	var colID int64
	err := db.QueryRow(`SELECT id FROM custom_columns WHERE label = ?`, label).Scan(&colID)
	if err != nil {
		res, err := db.Exec(`
			INSERT INTO custom_columns (label, name, datatype, is_multiple, normalized)
			VALUES (?,?,'text',1,1)`, label, label)
		if err != nil {
			l.t.Fatalf("create custom column %q: %v", label, err)
		}
		if colID, err = res.LastInsertId(); err != nil {
			l.t.Fatal(err)
		}
		l.mustExec(db, fmt.Sprintf(`CREATE TABLE custom_column_%d (
			id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT NOT NULL COLLATE NOCASE, UNIQUE(value))`, colID))
		l.mustExec(db, fmt.Sprintf(`CREATE TABLE books_custom_column_%d_link (
			id INTEGER PRIMARY KEY AUTOINCREMENT, book INTEGER NOT NULL, value INTEGER NOT NULL,
			UNIQUE(book, value))`, colID))
	}

	for _, value := range values {
		var valueID int64
		q := fmt.Sprintf(`SELECT id FROM custom_column_%d WHERE value = ?`, colID)
		if err := db.QueryRow(q, value).Scan(&valueID); err != nil {
			res, err := db.Exec(fmt.Sprintf(`INSERT INTO custom_column_%d (value) VALUES (?)`, colID), value)
			if err != nil {
				l.t.Fatalf("insert column value: %v", err)
			}
			if valueID, err = res.LastInsertId(); err != nil {
				l.t.Fatal(err)
			}
		}
		l.mustExec(db, fmt.Sprintf(
			`INSERT OR IGNORE INTO books_custom_column_%d_link (book, value) VALUES (?,?)`, colID),
			book, valueID)
	}
}

// fb2Bytes is a small but genuine FB2: XML, a cover inlined as base64, two
// sections. Written in UTF-8 rather than windows-1251 only because a fixture
// should be readable; the converter handles both.
func fb2Bytes(title string) []byte {
	// A one-pixel PNG, which is enough to be a cover.
	const cover = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk" +
		"YPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0"
             xmlns:l="http://www.w3.org/1999/xlink">
<description>
  <title-info>
    <book-title>` + title + `</book-title>
    <author><first-name>Jane</first-name><last-name>Author</last-name></author>
    <lang>ru</lang>
    <annotation><p>A short description.</p></annotation>
    <coverpage><image l:href="#cover.png"/></coverpage>
  </title-info>
  <publish-info><publisher>Some Press</publisher><year>2020</year></publish-info>
</description>
<body>
  <section>
    <title><p>Chapter One</p></title>
    <p>First sentence. Second sentence.</p>
    <p>Another paragraph with <emphasis>emphasis</emphasis> in it.</p>
  </section>
  <section>
    <title><p>Chapter Two</p></title>
    <p>More text.</p>
  </section>
</body>
<binary id="cover.png" content-type="image/png">` + cover + `</binary>
</FictionBook>`)
}
