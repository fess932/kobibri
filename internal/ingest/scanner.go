package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/store"
)

// ErrSuspicious is returned when a scan would mark an implausible share of a
// source's books as vanished. The scan is rolled back and waits for an explicit
// confirmation in the web UI.
var ErrSuspicious = errors.New("scan would remove an implausible number of books")

// Vanish guard thresholds. A library legitimately loses a few books at a time;
// losing a fifth of it at once is far more likely to be a half-mounted share or
// a partially synced directory.
const (
	vanishFraction = 0.20
	vanishFloor    = 25
)

// Scanner reads Calibre libraries into the canonical library.
type Scanner struct {
	store  *store.Store
	tmpDir string
}

func NewScanner(st *store.Store, tmpDir string) *Scanner {
	return &Scanner{store: st, tmpDir: tmpDir}
}

// Result summarises one scan.
type Result struct {
	store.ScanCounts
	Skipped bool // metadata.db was unchanged since the last successful scan
}

// ScanOptions tunes one scan.
type ScanOptions struct {
	// Force reads the library even when its metadata.db is unchanged.
	Force bool
	// ConfirmVanish accepts a mass disappearance the guard would otherwise
	// refuse. Set by the operator from the web UI.
	ConfirmVanish bool
}

// Scan reads one source and updates the canonical library.
//
// Failure to reach the library is not a scan result: nothing in the database is
// touched, because an unmounted share must never be mistaken for a library that
// lost every book.
func (s *Scanner) Scan(ctx context.Context, sourceID int64, opts ScanOptions) (Result, error) {
	src, err := store.GetSource(ctx, s.store.Reader(), sourceID)
	if err != nil {
		return Result{}, err
	}

	// A web source has no metadata.db: its books arrive through webimport, not
	// through a scan.
	if src.Kind == store.SourceKindWeb {
		return Result{Skipped: true}, nil
	}

	sig, err := calibre.Stat(src.LibraryPath)
	if err != nil {
		s.recordFailure(ctx, sourceID, store.SourceStatusUnreachable, err)
		return Result{}, err
	}

	sigKey := fmt.Sprintf("source:%d:metadata_sig", sourceID)
	sigValue := fmt.Sprintf("%d:%d", sig.Size, sig.Mtime)
	if !opts.Force && src.LastStatus == store.SourceStatusOK {
		if prev, _ := store.GetKV(ctx, s.store.Reader(), sigKey); prev == sigValue {
			slog.Debug("skipping scan, metadata.db unchanged", "source", src.Name)
			return Result{Skipped: true}, nil
		}
	}

	runID, err := store.StartScanRun(ctx, s.store.Writer(), sourceID)
	if err != nil {
		return Result{}, err
	}
	if err := store.SetSourceStatus(ctx, s.store.Writer(), sourceID, store.SourceStatusRunning, ""); err != nil {
		return Result{}, err
	}

	res, err := s.scan(ctx, src, opts)
	if err != nil {
		status := store.SourceStatusError
		if errors.Is(err, calibre.ErrUnreachable) {
			status = store.SourceStatusUnreachable
		} else if errors.Is(err, ErrSuspicious) {
			status = store.SourceStatusSuspicious
		}
		store.FinishScanRun(ctx, s.store.Writer(), runID, status, err.Error(), res.ScanCounts)
		store.SetSourceStatus(ctx, s.store.Writer(), sourceID, status, err.Error())
		return res, err
	}

	if err := store.SetKV(ctx, s.store.Writer(), sigKey, sigValue); err != nil {
		return res, err
	}
	if err := store.FinishScanRun(ctx, s.store.Writer(), runID, store.SourceStatusOK, "", res.ScanCounts); err != nil {
		return res, err
	}
	if err := store.SetSourceStatus(ctx, s.store.Writer(), sourceID, store.SourceStatusOK, ""); err != nil {
		return res, err
	}

	s.rebuildCollections(ctx)
	return res, nil
}

// RebuildCollections mirrors the library's tags and series onto the readers'
// shelves.
//
// It runs after ingest rather than inside it: membership depends on which books
// ended up syncable, which is only settled once every contributor has been
// resolved.
func (s *Scanner) RebuildCollections(ctx context.Context) error {
	mode := CollectionsMode(ctx, s.store.Reader())
	if mode == CollectionsOff {
		return nil
	}
	return s.store.Tx(ctx, func(tx *sql.Tx) error {
		return RebuildCollections(ctx, tx, mode)
	})
}

// rebuildCollections is the same thing where a failure must not fail the scan:
// the books are ingested either way, and shelves are a convenience on top.
func (s *Scanner) rebuildCollections(ctx context.Context) {
	if err := s.RebuildCollections(ctx); err != nil {
		slog.Error("rebuilding collections", "err", err)
	}
}

func (s *Scanner) recordFailure(ctx context.Context, sourceID int64, status string, cause error) {
	if err := store.SetSourceStatus(ctx, s.store.Writer(), sourceID, status, cause.Error()); err != nil {
		slog.Error("recording source failure", "source", sourceID, "err", err)
	}
}

func (s *Scanner) scan(ctx context.Context, src *store.Source, opts ScanOptions) (Result, error) {
	db, err := calibre.Open(src.LibraryPath, s.tmpDir)
	if err != nil {
		return Result{}, err
	}
	defer db.Close()

	// Phase A: cheap stubs drive change and vanish detection.
	stubs, err := db.Stubs(ctx)
	if err != nil {
		return Result{}, err
	}
	stored, err := store.SourceStubs(ctx, s.store.Reader(), src.ID)
	if err != nil {
		return Result{}, err
	}

	var (
		res     Result
		toRead  []int64
		present = make(map[int64]bool, len(stubs))
	)
	res.Seen = len(stubs)

	for _, st := range stubs {
		present[st.ID] = true
		prev, known := stored[st.ID]
		switch {
		case !known:
			res.Added++
			toRead = append(toRead, st.ID)
		case prev.Missing || prev.CalibreLastModified != store.FormatTime(st.LastModified):
			res.Updated++
			toRead = append(toRead, st.ID)
		}
	}

	var vanished []int64
	for calibreID, prev := range stored {
		if !present[calibreID] && !prev.Missing {
			vanished = append(vanished, prev.ID)
		}
	}
	res.Vanished = len(vanished)

	if guard := vanishLimit(len(stored)); len(vanished) > guard && !opts.ConfirmVanish {
		return res, fmt.Errorf("%w: %d of %d books vanished (limit %d); confirm in the UI if this is real",
			ErrSuspicious, len(vanished), len(stored), guard)
	}

	// Phase B: full read, but only for what actually changed.
	books, err := db.Books(ctx, toRead)
	if err != nil {
		return res, err
	}

	touched := map[string]bool{}
	err = s.store.Tx(ctx, func(tx *sql.Tx) error {
		for _, b := range books {
			bookID, err := s.ingestBook(ctx, tx, src, b)
			if err != nil {
				return err
			}
			touched[bookID] = true
		}

		for _, id := range vanished {
			var bookID sql.NullString
			if err := tx.QueryRowContext(ctx,
				`SELECT book_id FROM source_books WHERE id = ?`, id).Scan(&bookID); err != nil {
				return err
			}
			if bookID.Valid && bookID.String != "" {
				touched[bookID.String] = true
			}
		}
		if err := store.MarkSourceBooksMissing(ctx, tx, vanished); err != nil {
			return err
		}

		// Re-resolve after every source row is in place, so a merge performed
		// halfway through does not leave an earlier book resolved against a
		// stale candidate set.
		for bookID := range touched {
			resolved, err := store.ResolveBookID(ctx, tx, bookID)
			if err != nil {
				return err
			}
			if err := Resolve(ctx, tx, resolved); err != nil {
				return err
			}
		}
		return nil
	})
	return res, err
}

func vanishLimit(total int) int {
	return max(int(float64(total)*vanishFraction), vanishFloor)
}

// ingestBook writes one Calibre row and attaches it to a canonical book.
func (s *Scanner) ingestBook(ctx context.Context, tx *sql.Tx, src *store.Source, b *calibre.Book) (string, error) {
	sb, files := toSourceBook(src.ID, b)

	if _, err := store.UpsertSourceBook(ctx, tx, sb); err != nil {
		return "", err
	}
	if err := store.ReplaceSourceBookFiles(ctx, tx, sb.ID, files); err != nil {
		return "", err
	}
	s.probeEPUBs(ctx, tx, src, sb.ID, files)

	bookID, err := Attach(ctx, tx, sb)
	if err != nil {
		return "", err
	}
	if err := store.SetSourceBookBookID(ctx, tx, sb.ID, bookID); err != nil {
		return "", err
	}
	return bookID, nil
}

// probeEPUBs records the layout of each EPUB, so the very first sync already
// advertises the right format.
//
// ReplaceSourceBookFiles carries a previous probe forward while the file itself
// is unchanged, so this only opens files that are actually new or modified.
func (s *Scanner) probeEPUBs(ctx context.Context, tx *sql.Tx, src *store.Source, sourceBookID int64, files []store.SourceBookFile) {
	for _, f := range files {
		if f.Format != "EPUB" || !f.Present || f.Layout != store.LayoutUnknown {
			continue
		}

		abs := filepath.Join(src.LibraryPath, filepath.FromSlash(f.RelPath))
		info, err := Probe(abs)
		if err != nil {
			// A book we cannot open is left unprobed rather than failing the
			// scan; it will simply be offered as KEPUB, and the conversion will
			// report the real problem.
			slog.Debug("probing epub layout", "path", f.RelPath, "err", err)
			continue
		}
		if err := store.SetFileProbe(ctx, tx, sourceBookID, f.Format,
			info.Layout, info.Version, f.FileMtime); err != nil {
			slog.Debug("recording epub probe", "path", f.RelPath, "err", err)
		}
	}
}

func toSourceBook(sourceID int64, b *calibre.Book) (*store.SourceBook, []store.SourceBookFile) {
	authors, _ := json.Marshal(b.AuthorNames())
	tags, _ := json.Marshal(b.Tags)
	identifiers, _ := json.Marshal(b.Identifiers)

	language := ""
	if len(b.Languages) > 0 {
		language = b.Languages[0]
	}

	sb := &store.SourceBook{
		SourceID:            sourceID,
		CalibreID:           b.ID,
		CalibreUUID:         b.UUID,
		Title:               b.Title,
		SortTitle:           b.SortTitle,
		AuthorsJSON:         string(authors),
		AuthorSort:          b.PrimaryAuthorSort(),
		SeriesName:          b.SeriesName,
		DescriptionHTML:     b.Description,
		Publisher:           b.Publisher,
		PublishedAt:         store.FormatTime(b.PubDate),
		Language:            language,
		ISBN13:              NormalizeISBN(b.Identifiers["isbn"]),
		IdentifiersJSON:     string(identifiers),
		TagsJSON:            string(tags),
		RelPath:             b.RelPath,
		CoverRelPath:        b.CoverRelPath,
		CoverMtime:          b.CoverMtime,
		CalibreLastModified: store.FormatTime(b.LastModified),
	}
	if b.HasSeries {
		sb.SeriesIndex = sql.NullFloat64{Float64: b.SeriesIndex, Valid: true}
	}
	sb.MetaHash = metaHash(sb)

	files := make([]store.SourceBookFile, 0, len(b.Formats))
	for _, f := range b.Formats {
		if f.RelPath == "" {
			continue // refused by the path guard
		}
		files = append(files, store.SourceBookFile{
			Format:  strings.ToUpper(f.Format),
			RelPath: f.RelPath,
			Size:    f.Size,
			// Mtime is 0 for a file Calibre lists but that is not on disk; the
			// probe cache keys on it, so a zero never matches a real probe.
			FileMtime: f.Mtime,
			Present:   f.Present,
		})
	}
	return sb, files
}

// metaHash fingerprints a source row so an unchanged row can be recognised even
// if Calibre bumped last_modified without changing anything we care about.
func metaHash(sb *store.SourceBook) string {
	h := sha256.New()
	for _, part := range []string{
		sb.CalibreUUID, sb.Title, sb.SortTitle, sb.AuthorsJSON, sb.AuthorSort,
		sb.SeriesName, sb.DescriptionHTML, sb.Publisher, sb.PublishedAt,
		sb.Language, sb.ISBN13, sb.IdentifiersJSON, sb.TagsJSON,
		sb.RelPath, sb.CoverRelPath,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	fmt.Fprintf(h, "%v|%d", sb.SeriesIndex, sb.CoverMtime)
	return hex.EncodeToString(h.Sum(nil))
}

// SetSourceEnabled turns a source on or off and re-resolves everything it
// contributes to.
//
// The two halves are deliberately one call: flipping the flag changes which
// source rows are live, and a canonical book whose winner just appeared or
// disappeared is wrong until it is re-resolved. A scan will not fix it either,
// since nothing in Calibre changed and the book would never enter the changed
// set.
func (s *Scanner) SetSourceEnabled(ctx context.Context, sourceID int64, enabled bool) error {
	if err := store.SetSourceEnabled(ctx, s.store.Writer(), sourceID, enabled); err != nil {
		return err
	}
	return s.ResolveSource(ctx, sourceID)
}

// ResolveSource re-resolves every canonical book a source contributes to. It is
// what makes enabling or disabling a source take effect without a full rescan.
func (s *Scanner) ResolveSource(ctx context.Context, sourceID int64) error {
	ids, err := store.BooksTouchedBySource(ctx, s.store.Reader(), sourceID)
	if err != nil {
		return err
	}
	err = s.store.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			resolved, err := store.ResolveBookID(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := Resolve(ctx, tx, resolved); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	s.rebuildCollections(ctx)
	return nil
}
