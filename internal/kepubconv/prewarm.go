package kepubconv

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

// Prewarmer converts books ahead of anyone asking for them.
//
// The goal is that every imported book has a KEPUB ready: the web UI can then
// offer the original and the converted file for download, and a device never
// waits on a conversion mid-sync.
//
// It runs in the background rather than inside a scan on purpose. Converting a
// whole library synchronously at import is what makes other implementations'
// first sync take an age, and it would hold the writer connection for the
// duration. Here the scan finishes immediately and the files appear shortly
// after, which is the same outcome without the stall.
type Prewarmer struct {
	cache *Cache
	store *store.Store

	mu      sync.Mutex
	running bool
	wake    chan struct{}
}

func NewPrewarmer(cache *Cache, st *store.Store) *Prewarmer {
	return &Prewarmer{cache: cache, store: st, wake: make(chan struct{}, 1)}
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

// pending lists syncable KEPUB books whose source file has no cache entry.
//
// The join on kepub_cache is by book alone rather than by fingerprint, because
// the fingerprint depends on the file's current size and mtime, which SQL
// cannot compute. A stale entry is caught by Path, which fingerprints for real.
func (p *Prewarmer) pending(ctx context.Context) ([]pendingBook, error) {
	rows, err := p.store.Reader().QueryContext(ctx, `
		SELECT b.id, s.library_path, f.rel_path
		FROM books b
		JOIN source_book_files f ON f.source_book_id = b.primary_source_book_id
		JOIN source_books sb ON sb.id = f.source_book_id
		JOIN sources s ON s.id = sb.source_id
		WHERE b.merged_into IS NULL
		  AND b.syncable = 1
		  AND b.download_format = ?
		  AND f.format = 'EPUB' AND f.present = 1
		  AND NOT EXISTS (SELECT 1 FROM kepub_cache c WHERE c.book_id = b.id)
		ORDER BY b.updated_at DESC`, store.FormatKEPUB)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []pendingBook
	for rows.Next() {
		var id, libraryPath, relPath string
		if err := rows.Scan(&id, &libraryPath, &relPath); err != nil {
			return nil, err
		}
		out = append(out, pendingBook{
			bookID: id,
			path:   filepath.Join(libraryPath, filepath.FromSlash(relPath)),
		})
	}
	return out, rows.Err()
}
