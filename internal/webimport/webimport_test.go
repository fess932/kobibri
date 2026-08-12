package webimport

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/novelkit/novel"

	"github.com/fess932/kobibri/internal/store"
)

// fakeSource is a site that exists only in this test: no network, no rate
// limits, and a chapter count the test controls.
type fakeSource struct {
	chapters int
	fetched  int // how many chapter downloads it actually served
}

const fakeURL = "https://example.test/book/soak-1"

func (f *fakeSource) ID() string { return "faketest" }

func (f *fakeSource) Supports(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://example.test/book/")
}

func (f *fakeSource) ParseRef(rawURL string) (string, bool) {
	id := strings.TrimPrefix(rawURL, "https://example.test/book/")
	return id, id != "" && id != rawURL
}

func (f *fakeSource) Search(context.Context, string) ([]novel.Book, error) {
	return nil, novel.ErrUnsupported
}

func (f *fakeSource) Book(_ context.Context, bookID string) (*novel.Book, error) {
	return &novel.Book{
		ID:          bookID,
		Title:       "A Serial Story",
		Authors:     []string{"Web Author"},
		Description: "Published a chapter at a time.",
		Language:    "en",
		URL:         fakeURL,
		Editions:    []novel.Edition{{ID: "main", Name: "Main", Chapters: f.chapters}},
	}, nil
}

func (f *fakeSource) Chapters(_ context.Context, _, _ string) ([]novel.ChapterInfo, error) {
	out := make([]novel.ChapterInfo, 0, f.chapters)
	for i := 1; i <= f.chapters; i++ {
		out = append(out, novel.ChapterInfo{
			ID: fmt.Sprintf("c%d", i), Index: i,
			Number: fmt.Sprint(i), Name: fmt.Sprintf("Chapter %d", i),
		})
	}
	return out, nil
}

func (f *fakeSource) Chapter(_ context.Context, _, _ string, ci novel.ChapterInfo) (*novel.Chapter, error) {
	f.fetched++
	raw, _ := json.Marshal(map[string]any{"info": ci, "text": ci.Name + " body text."})
	return &novel.Chapter{Info: ci, Content: plainContent(ci.Name + " body text."), Raw: raw}, nil
}

func (f *fakeSource) DecodeChapter(raw []byte) (*novel.Chapter, error) {
	var stored struct {
		Info novel.ChapterInfo `json:"info"`
		Text string            `json:"text"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	return &novel.Chapter{Info: stored.Info, Content: plainContent(stored.Text), Raw: raw}, nil
}

func (f *fakeSource) Fetch(context.Context, string) ([]byte, string, error) {
	return nil, "", novel.ErrNotFound
}

// plainContent is a chapter body with no markup worth speaking of.
type plainContent string

func (p plainContent) XHTML(novel.ImageResolver) string { return "<p>" + string(p) + "</p>" }
func (p plainContent) PlainText() string                { return string(p) }

func newImporter(t *testing.T, src novel.Source) (*Importer, *store.Store) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(context.Background(), filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	im, err := New(Options{Store: st, Root: filepath.Join(dir, "imports")})
	if err != nil {
		t.Fatalf("new importer: %v", err)
	}
	// Replace the real sites with one that cannot reach the network.
	im.registry = &novel.Registry{}
	im.registry.Register(src)

	return im, st
}

// An imported book has to end up indistinguishable from any other: a canonical
// book with a readable EPUB, ready to sync.
func TestImportFilesABook(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	res, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !res.New {
		t.Error("the first import did not report the book as new")
	}
	if res.Chapters != 3 {
		t.Errorf("assembled %d chapters, want 3", res.Chapters)
	}
	if res.Title != "A Serial Story" {
		t.Errorf("title = %q", res.Title)
	}

	book, err := store.GetBook(ctx, st.Reader(), res.BookID)
	if err != nil {
		t.Fatalf("the imported book is not in the library: %v", err)
	}
	if !book.Syncable {
		t.Error("the imported book is not syncable")
	}
	if book.DownloadFormat != store.FormatKEPUB {
		t.Errorf("DownloadFormat = %q, want KEPUB", book.DownloadFormat)
	}
	if book.Title != "A Serial Story" {
		t.Errorf("canonical title = %q", book.Title)
	}

	// The file it points at has to exist and be a real EPUB.
	path, err := store.BookFilePath(ctx, st.Reader(), book, "EPUB")
	if err != nil {
		t.Fatalf("no file behind the imported book: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("assembled file is missing or empty: %v", err)
	}
}

// Importing the same link again must land on the same book and fetch only what
// has been published since.
func TestReimportPicksUpNewChapters(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 3}
	im, st := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fetchedFirst := src.fetched

	// Two more chapters appear on the site.
	src.chapters = 5

	second, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.New {
		t.Error("re-importing the same link reported the book as new")
	}
	if second.BookID != first.BookID {
		t.Fatalf("re-import produced a different book: %s -> %s", first.BookID, second.BookID)
	}
	if second.Chapters != 5 {
		t.Errorf("assembled %d chapters, want 5", second.Chapters)
	}

	// Only the two new chapters should have been downloaded.
	if newlyFetched := src.fetched - fetchedFirst; newlyFetched != 2 {
		t.Errorf("downloaded %d chapters on the second run, want only the 2 new ones", newlyFetched)
	}

	// The library must still hold one book, not two.
	var n int
	if err := st.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM books WHERE merged_into IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the library holds %d books after re-importing one link, want 1", n)
	}

	// A longer book means a different file, so the device is told to fetch it.
	book, err := store.GetBook(ctx, st.Reader(), second.BookID)
	if err != nil {
		t.Fatal(err)
	}
	if book.MetadataRev < 2 {
		t.Errorf("metadata_rev = %d after new chapters arrived, want it to have moved",
			book.MetadataRev)
	}
}

// Refresh finds the link behind a book and re-runs the import.
func TestRefreshByBookID(t *testing.T) {
	ctx := context.Background()
	src := &fakeSource{chapters: 2}
	im, _ := newImporter(t, src)

	first, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	src.chapters = 4
	refreshed, err := im.Refresh(ctx, first.BookID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.BookID != first.BookID {
		t.Errorf("refresh moved the book: %s -> %s", first.BookID, refreshed.BookID)
	}
	if refreshed.Chapters != 4 {
		t.Errorf("assembled %d chapters after refresh, want 4", refreshed.Chapters)
	}
}

// A link nobody handles has to be refused clearly, before anything is created.
func TestUnsupportedLink(t *testing.T) {
	ctx := context.Background()
	im, st := newImporter(t, &fakeSource{chapters: 1})

	if im.Supports("https://elsewhere.invalid/book/1") {
		t.Error("Supports accepted a link no provider handles")
	}
	if _, err := im.Import(ctx, "https://elsewhere.invalid/book/1", ImportOptions{}); err == nil {
		t.Fatal("importing an unsupported link succeeded")
	}

	var sources int
	if err := st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM sources`).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if sources != 0 {
		t.Errorf("a refused import still created %d source(s)", sources)
	}
}

// The web source must never be scanned off the filesystem: it has no
// metadata.db, and a scan that tried would mark every imported book as vanished.
func TestWebSourceIsNotScanned(t *testing.T) {
	ctx := context.Background()
	im, st := newImporter(t, &fakeSource{chapters: 2})

	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatal(err)
	}

	sources, err := store.ListSources(ctx, st.Reader())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	if sources[0].Kind != store.SourceKindWeb {
		t.Errorf("source kind = %q, want %q", sources[0].Kind, store.SourceKindWeb)
	}

	// The imported book keeps its link as an identity key, so re-importing lands
	// on the same canonical book even if the title changes.
	var kinds []string
	rows, err := st.Reader().QueryContext(ctx,
		`SELECT kind FROM book_identities ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if !containsString(kinds, "weburl") {
		t.Errorf("identity kinds = %v, want one of them to be weburl", kinds)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
