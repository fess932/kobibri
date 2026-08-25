// Package kepubconv converts EPUB files to Kobo's KEPUB format, lazily and with
// a cache on disk.
//
// Conversion happens on download rather than during a scan. Converting a whole
// library up front is what makes other implementations' first sync so slow, and
// most books in a library are never opened.
package kepubconv

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/fess932/kobibri/internal/store"
)

// KepubSuffix is load-bearing. Kobo's reader only uses its KEPUB renderer for
// files named *.kepub.epub, so the suffix must survive from the cache path
// through to the Content-Disposition filename. See docs/NOTES.md.
const KepubSuffix = ".kepub.epub"

// Limits chosen so one pathological book cannot stall every other download.
const (
	convertTimeout = 2 * time.Minute
	maxInputBytes  = 300 << 20
)

// ErrTooLarge means the source EPUB is beyond what we will convert; the caller
// should serve the original file instead.
var ErrTooLarge = errors.New("epub is too large to convert")

// Converter turns an EPUB on disk into a KEPUB on disk.
type Converter interface {
	Convert(ctx context.Context, srcPath, dstPath string) error
	Name() string
}

// Cache serves converted files, converting on first request.
type Cache struct {
	dir   string
	store *store.Store
	conv  Converter

	sf  singleflight.Group
	sem *semaphore.Weighted
}

type Options struct {
	// Dir is where converted files live.
	Dir string
	// Store records what is cached, so eviction can find it again after a restart.
	Store *store.Store
	// KepubifyBin runs an external kepubify instead of the built-in library.
	// An escape hatch for the day the library and the CLI disagree.
	KepubifyBin string
	// Converter picks the implementation. Ours is the default; "kepubify" is
	// only meaningful together with KepubifyBin, since the library itself is no
	// longer a dependency.
	Converter string
	// Concurrency caps simultaneous conversions; zero picks half the CPUs.
	Concurrency int
}

func NewCache(opts Options) (*Cache, error) {
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create kepub cache dir: %w", err)
	}

	var conv Converter
	if opts.KepubifyBin != "" {
		conv = &execConverter{bin: opts.KepubifyBin}
	} else {
		conv = newNativeConverter()
	}

	n := opts.Concurrency
	if n <= 0 {
		n = max(1, runtime.NumCPU()/2)
	}
	slog.Debug("kepub converter ready", "impl", conv.Name(), "concurrency", n)

	return &Cache{dir: opts.Dir, store: opts.Store, conv: conv, sem: semaphore.NewWeighted(int64(n))}, nil
}

// Fingerprint identifies the exact source file a conversion came from.
//
// Size is included alongside the modification time so a file swapped in place
// with a preserved timestamp — which rsync and some sync tools do — still
// invalidates the cache.
func Fingerprint(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	h := sha1.New()
	_, _ = fmt.Fprintf(h, "%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Path returns a converted file for the given book, converting if needed.
//
// Concurrent requests for the same book convert once: a device that starts
// several downloads at once is normal.
func (c *Cache) Path(ctx context.Context, bookID, srcPath string) (string, int64, error) {
	fp, err := Fingerprint(srcPath)
	if err != nil {
		return "", 0, err
	}

	key := bookID + ":" + fp
	result, err, _ := c.sf.Do(key, func() (any, error) {
		return c.convert(ctx, bookID, srcPath, fp)
	})
	if err != nil {
		return "", 0, err
	}

	out := result.(cached)
	return out.path, out.size, nil
}

type cached struct {
	path string
	size int64
}

func (c *Cache) convert(ctx context.Context, bookID, srcPath, fp string) (cached, error) {
	dst := c.pathFor(bookID, fp)

	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		c.touch(ctx, bookID, fp)
		return cached{path: dst, size: fi.Size()}, nil
	}

	src, err := os.Stat(srcPath)
	if err != nil {
		return cached{}, err
	}
	if src.Size() > maxInputBytes {
		return cached{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, src.Size())
	}

	if err := c.sem.Acquire(ctx, 1); err != nil {
		return cached{}, err
	}
	defer c.sem.Release(1)

	// Another request may have finished while we waited for a slot.
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		c.touch(ctx, bookID, fp)
		return cached{path: dst, size: fi.Size()}, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return cached{}, err
	}

	convCtx, cancel := context.WithTimeout(ctx, convertTimeout)
	defer cancel()

	// Convert to a temporary name and rename into place, so a crash or a
	// timeout can never leave a truncated file that later looks cached.
	tmp := dst + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	start := time.Now()
	if err := c.conv.Convert(convCtx, srcPath, tmp); err != nil {
		_ = os.Remove(tmp)
		c.recordFailure(ctx, bookID, fp, err)
		return cached{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return cached{}, err
	}

	fi, err := os.Stat(dst)
	if err != nil {
		return cached{}, err
	}
	slog.Info("converted to kepub", "book", bookID, "bytes", fi.Size(),
		"took", time.Since(start).Round(time.Millisecond))

	c.record(ctx, bookID, fp, dst, fi.Size())
	return cached{path: dst, size: fi.Size()}, nil
}

// pathFor shards by id prefix so one directory does not accumulate a whole
// library's worth of entries.
func (c *Cache) pathFor(bookID, fp string) string {
	shard := bookID
	if len(shard) > 2 {
		shard = shard[:2]
	}
	return filepath.Join(c.dir, shard, bookID+"."+fp+KepubSuffix)
}

// Failed reports whether converting this exact file already failed, so the
// caller can serve the original EPUB without retrying on every request.
func (c *Cache) Failed(ctx context.Context, bookID, srcPath string) bool {
	fp, err := Fingerprint(srcPath)
	if err != nil {
		return false
	}
	var n int
	err = c.store.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM kepub_failures WHERE book_id = ? AND src_fp = ?`, bookID, fp).Scan(&n)
	return err == nil && n > 0
}

func (c *Cache) record(ctx context.Context, bookID, fp, path string, size int64) {
	now := store.Now()
	_, err := c.store.Writer().ExecContext(ctx, `
		INSERT INTO kepub_cache (book_id, src_fp, path, size, created_at, last_used_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(book_id, src_fp) DO UPDATE SET
			path = excluded.path, size = excluded.size, last_used_at = excluded.last_used_at`,
		bookID, fp, path, size, now, now)
	if err != nil {
		slog.Debug("recording kepub cache entry", "err", err)
	}
	_, _ = c.store.Writer().ExecContext(ctx,
		`DELETE FROM kepub_failures WHERE book_id = ? AND src_fp = ?`, bookID, fp)
}

func (c *Cache) touch(ctx context.Context, bookID, fp string) {
	_, _ = c.store.Writer().ExecContext(ctx,
		`UPDATE kepub_cache SET last_used_at = ? WHERE book_id = ? AND src_fp = ?`,
		store.Now(), bookID, fp)
}

func (c *Cache) recordFailure(ctx context.Context, bookID, fp string, cause error) {
	slog.Warn("kepub conversion failed; the original EPUB will be served instead",
		"book", bookID, "err", cause)
	_, _ = c.store.Writer().ExecContext(ctx, `
		INSERT INTO kepub_failures (book_id, src_fp, err, at) VALUES (?,?,?,?)
		ON CONFLICT(book_id, src_fp) DO UPDATE SET err = excluded.err, at = excluded.at`,
		bookID, fp, cause.Error(), store.Now())
}

// Evict trims the cache to a byte budget, oldest use first, and drops entries
// whose source file has since changed.
func (c *Cache) Evict(ctx context.Context, budget int64) error {
	rows, err := c.store.Reader().QueryContext(ctx,
		`SELECT book_id, src_fp, path, size FROM kepub_cache ORDER BY last_used_at DESC`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

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
			slog.Debug("removing cached kepub", "path", e.path, "err", err)
		}
		_, _ = c.store.Writer().ExecContext(ctx,
			`DELETE FROM kepub_cache WHERE book_id = ? AND src_fp = ?`, e.bookID, e.fp)
	}
	if len(stale) > 0 {
		slog.Info("evicted cached kepubs", "count", len(stale), "kept_bytes", keep)
	}
	return nil
}

// Impl names the conversion backend in use, for diagnostics.
func (c *Cache) Impl() string { return c.conv.Name() }
