package webimport

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	fakeURL  = "https://example.test/book/soak-1"
	coverURL = "https://example.test/covers/soak-1.png"
)

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
		CoverURL:    coverURL,
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

func (f *fakeSource) Fetch(_ context.Context, url string) ([]byte, string, error) {
	if url == coverURL {
		return pngPixel(), "image/png", nil
	}
	return nil, "", novel.ErrNotFound
}

// pngPixel is the smallest valid PNG, so a fixture can carry a real image.
func pngPixel() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0d, 0x0a, 0x2d, 0xb4,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
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
	t.Cleanup(func() { _ = st.Close() })

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
	defer func() { _ = rows.Close() }()
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

// hiddenSource stands for a title the site will not show without an account: it
// answers exactly as it does for a book that never existed.
type hiddenSource struct{ fakeSource }

func (h *hiddenSource) Book(context.Context, string) (*novel.Book, error) {
	return nil, novel.ErrNotFound
}

// A 404 from the site means one of two things and looks identical either way.
// Saying which is the difference between giving up and pasting in a token.
func TestANotFoundSaysATokenMightHelp(t *testing.T) {
	im, _ := newImporter(t, &hiddenSource{})

	_, err := im.Editions(context.Background(), fakeURL)
	if err == nil {
		t.Fatal("a hidden title looked fine")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want it to be ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "signed-in") {
		t.Errorf("the message does not mention an account: %q", err)
	}
}

// With a token set, the same failure must stop blaming the missing token.
func TestWithATokenTheMessageChanges(t *testing.T) {
	ctx := context.Background()
	im, _ := newImporter(t, &hiddenSource{})

	if im.HasToken() {
		t.Fatal("a fresh importer already has a token")
	}
	if err := im.SetToken(ctx, "  secret-token  "); err != nil {
		t.Fatal(err)
	}
	if !im.HasToken() {
		t.Fatal("the token was not stored")
	}

	// Setting a token rebuilds the providers, so put the fake one back.
	im.registry = &novel.Registry{}
	im.registry.Register(&hiddenSource{})

	_, err := im.Editions(ctx, fakeURL)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "access token is set") {
		t.Errorf("the message still blames a missing token: %q", err)
	}
}

// The token has to survive a restart: the daily check for new chapters runs long
// after anyone typed it.
func TestATokenOutlivesTheProcess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	first, err := New(Options{Store: st, Root: filepath.Join(dir, "imports")})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetToken(ctx, "secret-token"); err != nil {
		t.Fatal(err)
	}

	second, err := New(Options{Store: st, Root: filepath.Join(dir, "imports")})
	if err != nil {
		t.Fatal(err)
	}
	if !second.HasToken() {
		t.Error("a restart lost the token")
	}

	// And clearing it really clears it.
	if err := second.SetToken(ctx, ""); err != nil {
		t.Fatal(err)
	}
	third, err := New(Options{Store: st, Root: filepath.Join(dir, "imports")})
	if err != nil {
		t.Fatal(err)
	}
	if third.HasToken() {
		t.Error("a cleared token came back")
	}
}

// twoEditions is a book with named translations and no unnamed one, which is
// what a real site looks like.
type twoEditions struct{ fakeSource }

func (t *twoEditions) Book(_ context.Context, bookID string) (*novel.Book, error) {
	return &novel.Book{
		ID: bookID, Title: "A Serial Story", Language: "en", URL: fakeURL,
		Editions: []novel.Edition{
			{ID: "9824", Name: "First team", Chapters: 3},
			{ID: "9823", Name: "Second team", Chapters: 3},
		},
	}, nil
}

// Importing without naming a translation and then naming the very one that was
// used must not download the whole book a second time.
//
// This used to be wrong upstream — an unnamed edition got its own "--default"
// cache directory — so the test stays as a guard rather than as an accusation.
func TestAnUnnamedTranslationReusesItsDownload(t *testing.T) {
	ctx := context.Background()
	im, _ := newImporter(t, &twoEditions{fakeSource{chapters: 3}})

	if _, err := im.Import(ctx, fakeURL, ImportOptions{}); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := im.Import(ctx, fakeURL, ImportOptions{EditionID: "9824"}); err != nil {
		t.Fatalf("second import: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(im.jobs.Root()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the same translation was downloaded twice, into %v", names)
	}
}

// A book from the web carries its cover inside the assembled file and nowhere
// else. Without pulling it out, every imported serial is a blank rectangle on
// the reader.
func TestAnImportedBookKeepsItsCover(t *testing.T) {
	ctx := context.Background()
	im, st := newImporter(t, &fakeSource{chapters: 2})

	res, err := im.Import(ctx, fakeURL, ImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	book, err := store.GetBook(ctx, st.Reader(), res.BookID)
	if err != nil {
		t.Fatal(err)
	}
	if book.CoverImageID == "" {
		t.Fatal("the imported book has no cover")
	}

	var libraryPath, relPath string
	if err := st.Reader().QueryRowContext(ctx, `
		SELECT s.library_path, sb.cover_rel_path
		FROM source_books sb JOIN sources s ON s.id = sb.source_id
		WHERE sb.id = ?`, book.CoverSourceBookID.Int64).Scan(&libraryPath, &relPath); err != nil {
		t.Fatal(err)
	}
	path, err := store.CoverPath(libraryPath, relPath)
	if err != nil {
		t.Fatalf("the recorded cover is not on disk: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("the cover file is empty: %v", err)
	}
}
