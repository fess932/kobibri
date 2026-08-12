package kepubconv

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

// EPUBSource hands back an EPUB for a book, converting from another format when
// that is what the library holds. It is an interface so this package does not
// have to know how that happens.
type EPUBSource interface {
	EPUBFor(ctx context.Context, book *store.Book) (string, error)
}

// Prewarmer converts books ahead of anyone asking for them.
//
// The goal is that every book has a KEPUB ready: the browser can then offer the
// converted file for download, and a device never waits on a conversion
// mid-sync — which matters most for a book the library holds in another format,
// where two conversions run back to back.
//
// It runs in the background rather than inside a scan on purpose. Converting a
// whole library synchronously is what makes other implementations' first sync
// take an age, and it would hold the writer connection for the duration.
type Prewarmer struct {
	cache *Cache
	store *store.Store
	epub  EPUBSource

	mu      sync.Mutex
	running bool
	wake    chan struct{}
}

func NewPrewarmer(cache *Cache, st *store.Store, epub EPUBSource) *Prewarmer {
	return &Prewarmer{cache: cache, store: st, epub: epub, wake: make(chan struct{}, 1)}
}

// Run works the queue until the context is cancelled.
func (p *Prewarmer) Run(ctx context.Context) {
	// A slow first pass on a large library should not delay startup.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-timer.C:
		}

		if n, err := p.Pass(ctx); err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("prewarming kepubs", "err", err)
			}
		} else if n > 0 {
			slog.Info("prewarmed kepubs", "converted", n)
		}

		timer.Reset(15 * time.Minute)
	}
}

// Trigger asks for a pass as soon as one can start. It never blocks: a request
// arriving while one is pending is folded into it.
func (p *Prewarmer) Trigger() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Pass converts every syncable book that has no cached KEPUB yet, and returns
// how many were converted.
//
// Books whose conversion previously failed are skipped: they are served as
// their original EPUB, and retrying them on every pass would be pure waste.
func (p *Prewarmer) Pass(ctx context.Context) (int, error) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return 0, nil
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	pending, err := p.pending(ctx)
	if err != nil {
		return 0, err
	}

	var converted int
	for _, b := range pending {
		select {
		case <-ctx.Done():
			return converted, ctx.Err()
		default:
		}

		if p.cache.Failed(ctx, b.bookID, b.path) {
			continue
		}
		if _, _, err := p.cache.Path(ctx, b.bookID, b.path); err != nil {
			// Already recorded as a failure by the cache; the book stays
			// servable as its original EPUB.
			continue
		}
		converted++
	}
	return converted, nil
}

type pendingBook struct {
	bookID string
	path   string
}

// pending lists syncable KEPUB books with no cached conversion yet.
//
// The join on kepub_cache is by book alone rather than by fingerprint, because
// the fingerprint depends on the file's current size and mtime, which SQL
// cannot compute. A stale entry is caught by Path, which fingerprints for real.
func (p *Prewarmer) pending(ctx context.Context) ([]pendingBook, error) {
	rows, err := p.store.Reader().QueryContext(ctx, `
		SELECT id FROM books
		WHERE merged_into IS NULL AND syncable = 1 AND download_format = ?
		  AND convert_from <> 'KEPUB'
		  AND NOT EXISTS (SELECT 1 FROM kepub_cache c WHERE c.book_id = books.id)
		ORDER BY updated_at DESC`, store.FormatKEPUB)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Resolving the file goes through the EPUB source, so a book the library
	// holds only as FB2 or AZW3 is converted here rather than waiting for a
	// device to ask for it mid-sync.
	out := make([]pendingBook, 0, len(ids))
	for _, id := range ids {
		book, err := store.GetBook(ctx, p.store.Reader(), id)
		if err != nil {
			continue
		}
		path, err := p.epubFor(ctx, book)
		if err != nil {
			slog.Debug("no epub to convert from", "book", id, "err", err)
			continue
		}
		out = append(out, pendingBook{bookID: id, path: path})
	}
	return out, nil
}

func (p *Prewarmer) epubFor(ctx context.Context, book *store.Book) (string, error) {
	if p.epub != nil {
		if path, err := p.epub.EPUBFor(ctx, book); err == nil {
			return path, nil
		}
	}
	return store.BookFilePath(ctx, p.store.Reader(), book, "EPUB")
}
