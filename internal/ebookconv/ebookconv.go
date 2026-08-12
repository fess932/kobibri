// Package ebookconv turns the other formats a Calibre library holds — FB2,
// AZW3, MOBI and the rest — into EPUB, so they can go on to become KEPUB and
// reach a reader.
//
// The conversion itself is Calibre's `ebook-convert`. Fifteen years of
// per-format workarounds live in there, and the library kobibri reads is a
// Calibre library, so the tool is usually already on the machine. Rewriting any
// of that would be a bad trade.
package ebookconv

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/fess932/kobibri/internal/store"
)

// Convertible lists what is worth converting, best first.
//
// PDF, CBZ, CBR and DJVU are deliberately absent. Kobo does not accept them
// over sync at all, and converting a fixed-layout scan to EPUB produces
// something nobody wants to read.
var Convertible = []string{"AZW3", "MOBI", "FB2", "AZW", "LIT", "HTMLZ", "RTF", "DOCX", "TXT", "PDB"}

// IsConvertible reports whether a format is worth converting.
func IsConvertible(format string) bool {
	format = strings.ToUpper(format)
	for _, f := range Convertible {
		if f == format {
			return true
		}
	}
	return false
}

// BestConvertible picks which of a book's formats to convert from.
func BestConvertible(formats []string) string {
	have := map[string]bool{}
	for _, f := range formats {
		have[strings.ToUpper(f)] = true
	}
	for _, f := range Convertible {
		if have[f] {
			return f
		}
	}
	return ""
}

// convertTimeout is generous: `ebook-convert` on a large book is not quick, and
// giving up early only means doing it again later.
const convertTimeout = 10 * time.Minute

var (
	// ErrUnavailable means Calibre's converter is not on this machine, so a
	// book in another format cannot be made syncable.
	ErrUnavailable = errors.New("calibre's ebook-convert is not available")
	// ErrTooLarge means the source is beyond what we will convert.
	ErrTooLarge = errors.New("file is too large to convert")
)

const maxInputBytes = 500 << 20

// Cache converts to EPUB and remembers the result.
type Cache struct {
	dir   string
	bin   string
	store *store.Store

	sf  singleflight.Group
	sem *semaphore.Weighted
}

type Options struct {
	// Dir is where converted files live.
	Dir string
	// Store records what is cached, so eviction can find it again.
	Store *store.Store
	// Bin overrides the ebook-convert to use. Empty looks on PATH.
	Bin string
	// Concurrency caps simultaneous conversions; zero picks half the CPUs.
	Concurrency int
}

func New(opts Options) (*Cache, error) {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create epub cache dir: %w", err)
	}

	// Resolve the converter properly rather than trusting the setting. A path
	// that does not exist would otherwise look available, every book in another
	// format would be offered to a device, and every one of those downloads
	// would fail — the one outcome worse than not offering them.
	bin := ""
	switch {
	case opts.Bin != "":
		if found, err := exec.LookPath(opts.Bin); err == nil {
			bin = found
		} else {
			slog.Warn("the configured ebook-convert cannot be run",
				"path", opts.Bin, "err", err)
		}
	default:
		bin = findConverter()
	}

	n := opts.Concurrency
	if n <= 0 {
		n = max(1, runtime.NumCPU()/2)
	}

	c := &Cache{dir: opts.Dir, bin: bin, store: opts.Store, sem: semaphore.NewWeighted(int64(n))}
	if c.Available() {
		slog.Info("format conversion available", "ebook-convert", bin)
	} else {
		slog.Info("format conversion unavailable; only books already in EPUB can sync",
			"hint", "install Calibre, or set KOBIBRI_EBOOK_CONVERT")
	}
	return c, nil
}

// findConverter locates Calibre's converter.
//
// PATH first, then where Calibre actually installs itself. On macOS it lives
// inside the application bundle and is never on PATH, so a machine with Calibre
// installed and working looks to a naive lookup exactly like a machine without
// it — and every book in another format is silently never offered.
func findConverter() string {
	if found, err := exec.LookPath("ebook-convert"); err == nil {
		return found
	}

	for _, candidate := range converterLocations {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			slog.Info("found Calibre's converter outside PATH", "path", candidate)
			return candidate
		}
	}
	return ""
}

// ConverterLocations lists where Calibre is looked for when it is not on PATH.
func ConverterLocations() []string { return converterLocations }

// converterLocations is where Calibre puts it on each platform. Listing them is
// worth more than a line in the README telling someone to fix their PATH.
var converterLocations = []string{
	// macOS: the application bundle.
	"/Applications/calibre.app/Contents/MacOS/ebook-convert",
	os.ExpandEnv("$HOME/Applications/calibre.app/Contents/MacOS/ebook-convert"),
	// Linux: the official installer, distribution packages, snap and flatpak.
	"/opt/calibre/ebook-convert",
	"/usr/bin/ebook-convert",
	"/usr/local/bin/ebook-convert",
	"/snap/bin/calibre.ebook-convert",
	"/var/lib/flatpak/exports/bin/com.calibre_ebook.calibre",
}

// Available reports whether conversion can happen at all.
func (c *Cache) Available() bool { return c != nil && c.bin != "" }

// Bin is the converter in use, for the interface to show.
func (c *Cache) Bin() string {
	if c == nil {
		return ""
	}
	return c.bin
}

// Fingerprint identifies the exact source file a conversion came from. Size
// sits alongside the modification time so a file swapped in place with a
// preserved timestamp still invalidates the cache.
func Fingerprint(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	fmt.Fprintf(h, "%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Path returns an EPUB made from srcPath, converting if it is not cached.
//
// Concurrent requests for the same book convert once.
func (c *Cache) Path(ctx context.Context, bookID, srcPath, srcFormat string) (string, error) {
	if !c.Available() {
		return "", ErrUnavailable
	}

	fp, err := Fingerprint(srcPath)
	if err != nil {
		return "", err
	}

	result, err, _ := c.sf.Do(bookID+":"+fp, func() (any, error) {
		return c.convert(ctx, bookID, srcPath, srcFormat, fp)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (c *Cache) convert(ctx context.Context, bookID, srcPath, srcFormat, fp string) (string, error) {
	dst := c.pathFor(bookID, fp)

	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		c.touch(ctx, bookID, fp)
		return dst, nil
	}

	src, err := os.Stat(srcPath)
	if err != nil {
		return "", err
	}
	if src.Size() > maxInputBytes {
		return "", fmt.Errorf("%w: %d bytes", ErrTooLarge, src.Size())
	}

	if err := c.sem.Acquire(ctx, 1); err != nil {
		return "", err
	}
	defer c.sem.Release(1)

	// Another request may have finished while we waited for a slot.
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		c.touch(ctx, bookID, fp)
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}

	convCtx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()

	// Convert to a temporary name and rename into place, so an interrupted run
	// cannot leave a truncated file that later looks cached.
	tmp := dst + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".epub"
	start := time.Now()

	cmd := exec.CommandContext(convCtx, c.bin, srcPath, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		wrapped := fmt.Errorf("ebook-convert %s: %w: %s",
			filepath.Base(srcPath), err, lastLines(string(out), 3))
		c.recordFailure(ctx, bookID, fp, wrapped)
		return "", wrapped
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}

	fi, err := os.Stat(dst)
	if err != nil {
		return "", err
	}
	slog.Info("converted to epub", "book", bookID, "from", srcFormat,
		"bytes", fi.Size(), "took", time.Since(start).Round(time.Second))

	c.record(ctx, bookID, fp, srcFormat, dst, fi.Size())
	return dst, nil
}

// Failed reports whether converting this exact file already failed, so the
// caller does not retry it on every request.
func (c *Cache) Failed(ctx context.Context, bookID, srcPath string) bool {
	fp, err := Fingerprint(srcPath)
	if err != nil {
		return false
	}
	var n int
	err = c.store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM epub_failures WHERE book_id = ? AND src_fp = ?`, bookID, fp).Scan(&n)
	return err == nil && n > 0
}

func (c *Cache) pathFor(bookID, fp string) string {
	shard := bookID
	if len(shard) > 2 {
		shard = shard[:2]
	}
	return filepath.Join(c.dir, shard, bookID+"."+fp+".epub")
}

func (c *Cache) record(ctx context.Context, bookID, fp, srcFormat, path string, size int64) {
	now := store.Now()
	_, err := c.store.Writer().ExecContext(ctx, `
		INSERT INTO epub_cache (book_id, src_fp, src_format, path, size, created_at, last_used_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(book_id, src_fp) DO UPDATE SET
			path = excluded.path, size = excluded.size, last_used_at = excluded.last_used_at`,
		bookID, fp, srcFormat, path, size, now, now)
	if err != nil {
		slog.Debug("recording epub cache entry", "err", err)
	}
	c.store.Writer().ExecContext(ctx,
		`DELETE FROM epub_failures WHERE book_id = ? AND src_fp = ?`, bookID, fp)
}

func (c *Cache) touch(ctx context.Context, bookID, fp string) {
	c.store.Writer().ExecContext(ctx,
		`UPDATE epub_cache SET last_used_at = ? WHERE book_id = ? AND src_fp = ?`,
		store.Now(), bookID, fp)
}

func (c *Cache) recordFailure(ctx context.Context, bookID, fp string, cause error) {
	slog.Warn("format conversion failed; the book cannot be synced",
		"book", bookID, "err", cause)
	c.store.Writer().ExecContext(ctx, `
		INSERT INTO epub_failures (book_id, src_fp, err, at) VALUES (?,?,?,?)
		ON CONFLICT(book_id, src_fp) DO UPDATE SET err = excluded.err, at = excluded.at`,
		bookID, fp, cause.Error(), store.Now())
}

// Evict trims the cache to a byte budget, least recently used first.
func (c *Cache) Evict(ctx context.Context, budget int64) error {
	if c == nil {
		return nil
	}
	rows, err := c.store.Reader().QueryContext(ctx,
		`SELECT book_id, src_fp, path, size FROM epub_cache ORDER BY last_used_at DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type entry struct {
		bookID, fp, path string
		size             int64
	}
	var (
		keep  int64
		stale []entry
	)
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.bookID, &e.fp, &e.path, &e.size); err != nil {
			return err
		}
		if keep+e.size <= budget {
			keep += e.size
			continue
		}
		stale = append(stale, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, e := range stale {
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			slog.Debug("removing converted epub", "path", e.path, "err", err)
		}
		c.store.Writer().ExecContext(ctx,
			`DELETE FROM epub_cache WHERE book_id = ? AND src_fp = ?`, e.bookID, e.fp)
	}
	return nil
}

// lastLines keeps the tail of a converter's output, which is where it says what
// went wrong.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}

// EPUBFor returns an EPUB for a book: the one in the library if there is one,
// otherwise a converted copy, made on demand.
//
// Everything downstream — KEPUB conversion, the download handlers, the browser —
// goes through here, so nothing else has to know that some books arrive in
// another format.
func (c *Cache) EPUBFor(ctx context.Context, book *store.Book) (string, error) {
	if c == nil {
		return "", ErrUnavailable
	}
	if !book.PrimarySourceBookID.Valid {
		return "", fmt.Errorf("book %s has no primary source", book.ID)
	}

	path, format, err := sourceFile(ctx, c.store, book)
	if err != nil {
		return "", err
	}
	// A KEPUB is an EPUB with extra markup, so it needs no conversion to be
	// read, downloaded, or served.
	if format == "EPUB" || format == store.FormatKEPUB {
		return path, nil
	}
	return c.Path(ctx, book.ID, path, format)
}

// sourceFile locates the file to serve or convert: the library's EPUB if it has
// one, otherwise whatever format the book was resolved to convert from.
func sourceFile(ctx context.Context, st *store.Store, book *store.Book) (path, format string, err error) {
	want := "EPUB"
	if book.ConvertFrom != "" {
		want = book.ConvertFrom
	}

	var libraryPath, relPath string
	err = st.Reader().QueryRowContext(ctx, `
		SELECT s.library_path, f.rel_path
		FROM source_book_files f
		JOIN source_books sb ON sb.id = f.source_book_id
		JOIN sources s ON s.id = sb.source_id
		WHERE f.source_book_id = ? AND f.format = ? AND f.present = 1
		LIMIT 1`, book.PrimarySourceBookID.Int64, want).Scan(&libraryPath, &relPath)
	if err != nil {
		return "", "", err
	}

	full := filepath.Join(libraryPath, filepath.FromSlash(relPath))
	// The path came from a database we do not control; refuse anything that
	// escapes its library root.
	clean := filepath.Clean(libraryPath)
	if full != clean && !strings.HasPrefix(full, clean+string(filepath.Separator)) {
		return "", "", fmt.Errorf("file path escapes its library root")
	}
	return full, want, nil
}
