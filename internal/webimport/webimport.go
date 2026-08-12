// Package webimport brings in books that are published on the web rather than
// sitting in a Calibre library.
//
// The imported book joins the library through exactly the same path as any
// other: a source row, an identity key, a canonical book. Only where the file
// came from differs, so merging, conversion and sync never learn about it.
package webimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fess932/novelkit/job"
	"github.com/fess932/novelkit/novel"
	"github.com/fess932/novelkit/sources/ranobelib"

	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// SourceKind marks a source whose books come from the web.
const SourceKind = "web"

var (
	// ErrUnsupported means no provider recognises the site.
	ErrUnsupported = errors.New("no provider handles that site")
	// ErrUnreadableLink means the site is one we support but the link itself
	// could not be read. Worth telling apart from the above: the answer is a
	// different link, not a different tool.
	ErrUnreadableLink = errors.New("that link could not be read")
)

// resolve finds the provider for a link and extracts the book's slug from it.
//
// novelkit tells the two failures apart, and they are worth keeping apart here
// too: "we do not do that site" and "we do that site but cannot read that
// address" call for different things from the person holding the link.
func (im *Importer) resolve(rawURL string) (novel.Source, string, error) {
	src, id, err := im.registry.Resolve(rawURL)
	switch {
	case errors.Is(err, novel.ErrUnsupported):
		return nil, "", fmt.Errorf("%w: %s", ErrUnsupported, rawURL)
	case errors.Is(err, novel.ErrBadReference):
		return nil, "", fmt.Errorf("%w: %s recognises the site but found no book in this "+
			"address; try the link to the title's own page", ErrUnreadableLink, src.ID())
	case err != nil:
		return nil, "", err
	}
	return src, id, nil
}

// Importer downloads books from the web and files them in the library.
type Importer struct {
	store    *store.Store
	registry *novel.Registry
	jobs     *job.Store
	runner   *runner
	// booksDir is where the assembled EPUBs live. It doubles as the library
	// path of the web source, so the ordinary file resolution applies.
	booksDir string
}

type Options struct {
	Store *store.Store
	// Root holds both the download cache and the assembled books.
	Root string
}

func New(opts Options) (*Importer, error) {
	jobsDir := filepath.Join(opts.Root, "jobs")
	booksDir := filepath.Join(opts.Root, "books")
	for _, d := range []string{jobsDir, booksDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	jobs, err := job.OpenStore(jobsDir)
	if err != nil {
		return nil, fmt.Errorf("open download cache: %w", err)
	}

	registry := &novel.Registry{}
	registry.Register(ranobelib.NewSource())

	return &Importer{
		store: opts.Store, registry: registry, jobs: jobs,
		booksDir: booksDir, runner: newRunner(),
	}, nil
}

// BooksDir is the directory the web source's files live in.
func (im *Importer) BooksDir() string { return im.booksDir }

// Providers lists the sites that can be imported from.
func (im *Importer) Providers() []string {
	sources := im.registry.Sources()
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = s.ID()
	}
	return out
}

// Supports reports whether a link can be imported — both that the site is one
// we handle and that this particular address can be read.
func (im *Importer) Supports(rawURL string) bool {
	_, _, err := im.resolve(strings.TrimSpace(rawURL))
	return err == nil
}

// Result describes what an import produced.
type Result struct {
	BookID   string
	Title    string
	Chapters int
	Missing  int
	Size     int64
	// New is false when the link had already been imported and this run only
	// fetched newly published chapters.
	New bool
}

// Edition is one translation of a title. Sites that carry several are the norm
// rather than the exception, and they are genuinely different texts: different
// wording, and often different chapter numbering.
type Edition struct {
	ID       string
	Name     string
	Teams    []string
	Chapters int
}

// Editions lists the translations available for a link, so a person can choose
// before anything is downloaded.
func (im *Importer) Editions(ctx context.Context, rawURL string) ([]Edition, error) {
	src, remoteID, err := im.resolve(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, err
	}

	book, err := src.Book(ctx, remoteID)
	if err != nil {
		return nil, err
	}

	out := make([]Edition, 0, len(book.Editions))
	for _, e := range book.Editions {
		out = append(out, Edition{ID: e.ID, Name: e.Name, Teams: e.Teams, Chapters: e.Chapters})
	}
	return out, nil
}

// ImportOptions choose what to import.
type ImportOptions struct {
	// EditionID selects the translation. Empty takes whatever the site offers
	// by default, which is the only sensible choice when there is just one.
	EditionID string

	// onProgress reports each chapter as it lands, for the interface to show.
	onProgress func(job.Event)
}

// Import downloads a book and files it in the library. Called again with the
// same link and translation it fetches whatever has been published since.
func (im *Importer) Import(ctx context.Context, rawURL string, opts ImportOptions) (Result, error) {
	rawURL = strings.TrimSpace(rawURL)

	src, remoteID, err := im.resolve(rawURL)
	if err != nil {
		return Result{}, err
	}

	sourceID, err := im.webSource(ctx)
	if err != nil {
		return Result{}, err
	}

	existing, err := im.existingImport(ctx, rawURL, opts.EditionID)
	if err != nil {
		return Result{}, err
	}

	// Whether the book is already known is a question about our library; the
	// download cache is novelkit's business. Planning again is the right move
	// either way: the cache directory is derived from the book, so an existing
	// one is reused, already-downloaded chapters are kept, and any newly
	// published ones are added to the list.
	alreadyKnown := existing != nil

	j, err := im.jobs.Plan(ctx, src, job.Request{
		BookID: remoteID, EditionID: opts.EditionID, WithImages: true,
	})
	if err != nil {
		return Result{}, fmt.Errorf("plan the download: %w", err)
	}

	if err := j.Download(ctx, src, job.DownloadOptions{OnChapter: opts.onProgress}); err != nil {
		// A partial download is still worth assembling: a serial that is missing
		// its newest chapter is better on the reader than nothing at all.
		slog.Warn("download did not finish", "url", rawURL, "err", err)
		im.recordError(ctx, existing, err)
	}

	state := j.State()
	epubPath := im.bookPath(j.Dir(), state.Book.Title)

	built, err := j.BuildFile(ctx, src, epubPath, job.BuildOptions{
		OnWarning: func(msg string) { slog.Debug("assembling book", "url", rawURL, "warning", msg) },
	})
	if err != nil {
		return Result{}, fmt.Errorf("assemble the book: %w", err)
	}

	bookID, err := im.record(ctx, sourceID, src.ID(), remoteID, rawURL, opts.EditionID, j, epubPath, state)
	if err != nil {
		return Result{}, err
	}

	return Result{
		BookID:   bookID,
		Title:    state.Book.Title,
		Chapters: built.Chapters,
		Missing:  built.Missing,
		Size:     built.Size,
		New:      !alreadyKnown,
	}, nil
}

// Refresh re-runs the import behind a book, picking up new chapters.
func (im *Importer) Refresh(ctx context.Context, bookID string) (Result, error) {
	var url, editionID string
	err := im.store.Reader().QueryRowContext(ctx, `
		SELECT w.url, w.edition_id FROM web_imports w
		JOIN source_books sb ON sb.id = w.source_book_id
		WHERE sb.book_id = ? LIMIT 1`, bookID).Scan(&url, &editionID)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, fmt.Errorf("that book was not imported from a link")
	}
	if err != nil {
		return Result{}, err
	}
	return im.Import(ctx, url, ImportOptions{EditionID: editionID})
}

// Imported is a book that came from a link, as the interface lists it.
type Imported struct {
	BookID        string
	SourceBookID  int64
	URL           string
	Provider      string
	JobDir        string
	Title         string
	ChaptersTotal int
	ChaptersDone  int
	LastError     string
	UpdatedAt     string
}

func (im *Importer) List(ctx context.Context) ([]Imported, error) {
	rows, err := im.store.Reader().QueryContext(ctx, `
		SELECT COALESCE(sb.book_id, ''), w.source_book_id, w.url, w.provider, w.job_dir,
		       sb.title, w.chapters_total, w.chapters_done, w.last_error, w.updated_at
		FROM web_imports w
		JOIN source_books sb ON sb.id = w.source_book_id
		ORDER BY w.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Imported
	for rows.Next() {
		var i Imported
		if err := rows.Scan(&i.BookID, &i.SourceBookID, &i.URL, &i.Provider, &i.JobDir,
			&i.Title, &i.ChaptersTotal, &i.ChaptersDone, &i.LastError, &i.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

type importRow struct {
	SourceBookID int64
	JobDir       string
}

func (im *Importer) existingImport(ctx context.Context, rawURL, editionID string) (*importRow, error) {
	var r importRow
	err := im.store.Reader().QueryRowContext(ctx,
		`SELECT source_book_id, job_dir FROM web_imports WHERE url = ? AND edition_id = ?`,
		rawURL, editionID).Scan(&r.SourceBookID, &r.JobDir)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// webSource returns the single source that owns imported books, creating it on
// first use. Its library path is where the assembled EPUBs live, so ordinary
// file resolution finds them.
func (im *Importer) webSource(ctx context.Context) (int64, error) {
	var id int64
	err := im.store.Reader().QueryRowContext(ctx,
		`SELECT id FROM sources WHERE kind = ? LIMIT 1`, SourceKind).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	src := &store.Source{
		Name: "Imported from the web", LibraryPath: im.booksDir,
		// A high number so a real Calibre copy of the same book wins.
		Priority: 900, Enabled: true, ShareAll: true, ScanIntervalSec: 24 * 3600,
	}
	if id, err = store.CreateSource(ctx, im.store.Writer(), src); err != nil {
		return 0, err
	}
	if _, err := im.store.Writer().ExecContext(ctx,
		`UPDATE sources SET kind = ?, last_status = ? WHERE id = ?`,
		SourceKind, store.SourceStatusOK, id); err != nil {
		return 0, err
	}
	return id, nil
}

// record files the assembled book as an ordinary source row and attaches it to
// a canonical book.
func (im *Importer) record(ctx context.Context, sourceID int64, provider, remoteID, rawURL, editionID string,
	j *job.Job, epubPath string, state job.State) (string, error) {

	fi, err := os.Stat(epubPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(im.booksDir, epubPath)
	if err != nil {
		return "", err
	}

	authors, _ := json.Marshal(state.Book.Authors)
	progress := j.Progress()

	sb := &store.SourceBook{
		SourceID: sourceID,
		// Web books have no Calibre id of their own. A stable hash of the link
		// gives each one a distinct slot in the (source, calibre_id) key.
		CalibreID:       calibreIDFor(identityURL(rawURL, editionID)),
		Title:           state.Book.Title,
		SortTitle:       state.Book.Title,
		AuthorsJSON:     string(authors),
		AuthorSort:      firstAuthor(state.Book.Authors),
		DescriptionHTML: state.Book.Description,
		Publisher:       state.Book.Publisher,
		Language:        state.Book.Language,
		IdentifiersJSON: `{}`,
		TagsJSON:        `[]`,
		RelPath:         filepath.ToSlash(filepath.Dir(rel)),
		WebURL:          identityURL(rawURL, editionID),
	}
	sb.MetaHash = fmt.Sprintf("%s|%d|%d", rawURL, progress.Done, fi.Size())

	var bookID string
	err = im.store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.UpsertSourceBook(ctx, tx, sb); err != nil {
			return err
		}
		files := []store.SourceBookFile{{
			Format:    "EPUB",
			RelPath:   filepath.ToSlash(rel),
			Size:      fi.Size(),
			FileMtime: fi.ModTime().UnixNano(),
			Present:   true,
			// Assembled by novelkit, so it is reflowable by construction; no
			// need to open it again to find that out.
			Layout: store.LayoutReflowable,
		}}
		if err := store.ReplaceSourceBookFiles(ctx, tx, sb.ID, files); err != nil {
			return err
		}

		if bookID, err = ingest.Attach(ctx, tx, sb); err != nil {
			return err
		}
		if err := store.SetSourceBookBookID(ctx, tx, sb.ID, bookID); err != nil {
			return err
		}

		now := store.Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO web_imports (source_book_id, url, provider, remote_book_id,
			                         edition_id, job_dir, chapters_total, chapters_done,
			                         last_error, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,'',?,?)
			ON CONFLICT(source_book_id) DO UPDATE SET
				chapters_total = excluded.chapters_total,
				chapters_done  = excluded.chapters_done,
				job_dir        = excluded.job_dir,
				last_error     = '',
				updated_at     = excluded.updated_at`,
			sb.ID, rawURL, provider, remoteID, editionID,
			filepath.Base(j.Dir()), progress.Total, progress.Done, now, now); err != nil {
			return err
		}

		resolved, err := store.ResolveBookID(ctx, tx, bookID)
		if err != nil {
			return err
		}
		return ingest.Resolve(ctx, tx, resolved)
	})
	return bookID, err
}

func (im *Importer) recordError(ctx context.Context, existing *importRow, cause error) {
	if existing == nil {
		return
	}
	im.store.Writer().ExecContext(ctx,
		`UPDATE web_imports SET last_error = ?, updated_at = ? WHERE source_book_id = ?`,
		cause.Error(), store.Now(), existing.SourceBookID)
}

// bookPath keeps each book in its own directory named after the download cache,
// so two titles with the same name cannot collide.
func (im *Importer) bookPath(jobDir, title string) string {
	dir := filepath.Base(jobDir)
	return filepath.Join(im.booksDir, dir, safeFilename(title)+".epub")
}

func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 100 {
		out = strings.TrimSpace(out[:100])
	}
	if out == "" {
		out = "book"
	}
	return out
}

func firstAuthor(authors []string) string {
	if len(authors) == 0 {
		return ""
	}
	return authors[0]
}

// identityURL is what an imported book is identified by. The translation is
// part of it: two translations of one title are different texts, with different
// wording and often different chapter numbering, so treating them as one book
// would mean each import silently replaced the other.
func identityURL(rawURL, editionID string) string {
	if editionID == "" {
		return rawURL
	}
	return rawURL + "#" + editionID
}

// calibreIDFor derives a stable positive integer from a link, so an imported
// book keeps the same (source, calibre_id) slot across runs.
func calibreIDFor(rawURL string) int64 {
	var h uint64 = 1469598103934665603
	for i := range len(rawURL) {
		h ^= uint64(rawURL[i])
		h *= 1099511628211
	}
	return int64(h&0x7fffffffffff) + 1
}

var _ = time.Now
