package textindex

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// EPUBSource hands back an EPUB for a book, converting from another format when
// that is what the library holds.
type EPUBSource interface {
	EPUBFor(ctx context.Context, book *store.Book) (string, error)
}

// Builder keeps the index in step with the file a device receives.
type Builder struct {
	store *store.Store
	kepub *kepubconv.Cache
	ebook EPUBSource
	sf    singleflight.Group

	mu     sync.Mutex
	failed map[string]time.Time
}

func New(st *store.Store, kepub *kepubconv.Cache, ebook EPUBSource) *Builder {
	return &Builder{store: st, kepub: kepub, ebook: ebook, failed: map[string]time.Time{}}
}

// retryAfter is how long a book that could not be measured is left alone. A
// book with no servable file at all would otherwise be opened on every progress
// report for the rest of its life.
const retryAfter = time.Hour

// EnsureAsync builds the index off the caller's goroutine.
//
// It is called from the reading-state handler, where the device is waiting on
// the response and the book may be a hundred thousand words. Nothing depends on
// the index being ready: an unresolved position simply produces no speed figure
// until the next report.
func (b *Builder) EnsureAsync(bookID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := b.Ensure(ctx, bookID); err != nil {
			slog.Debug("measuring book", "book", bookID, "err", err)
		}
	}()
}

// Ensure builds the index when it is missing or was built from another file.
func (b *Builder) Ensure(ctx context.Context, bookID string) error {
	b.mu.Lock()
	if until, ok := b.failed[bookID]; ok && time.Now().Before(until) {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	_, err, _ := b.sf.Do(bookID, func() (any, error) {
		return nil, b.build(ctx, bookID)
	})
	if err != nil {
		b.mu.Lock()
		b.failed[bookID] = time.Now().Add(retryAfter)
		b.mu.Unlock()
	}
	return err
}

func (b *Builder) build(ctx context.Context, bookID string) error {
	book, err := store.GetBook(ctx, b.store.Reader(), bookID)
	if err != nil {
		return err
	}

	path, err := b.servedFile(ctx, book)
	if err != nil {
		return err
	}

	fp, err := kepubconv.Fingerprint(path)
	if err != nil {
		return err
	}

	have, err := store.TextIndexFingerprint(ctx, b.store.Reader(), bookID)
	if err != nil {
		return err
	}
	if have == fp {
		return nil
	}

	ix, err := Build(path, fp)
	if err != nil {
		return err
	}
	if err := b.store.SaveTextIndex(ctx, bookID, ix); err != nil {
		return err
	}

	slog.Debug("measured book", "book", bookID, "words", ix.Words,
		"documents", len(ix.Docs), "spanned", ix.Spanned)
	return nil
}

// servedFile finds the file a device is given, which is the only one whose
// koboSpan ids match the positions it reports back. A library that holds its
// own KEPUB is served that file untouched, so it is measured untouched too.
func (b *Builder) servedFile(ctx context.Context, book *store.Book) (string, error) {
	src, err := b.epubPath(ctx, book)
	if err != nil {
		return "", err
	}
	if book.DownloadFormat != store.FormatKEPUB || b.kepub == nil ||
		book.ConvertFrom == store.FormatKEPUB {
		return src, nil
	}
	if b.kepub.Failed(ctx, book.ID, src) {
		return src, nil
	}
	path, _, err := b.kepub.Path(ctx, book.ID, src)
	if err != nil {
		return src, nil
	}
	return path, nil
}

func (b *Builder) epubPath(ctx context.Context, book *store.Book) (string, error) {
	if b.ebook != nil {
		if path, err := b.ebook.EPUBFor(ctx, book); err == nil {
			return path, nil
		}
	}
	return store.BookFilePath(ctx, b.store.Reader(), book, "EPUB")
}

// Sweep measures books someone has read that have never been measured. It is
// what catches up a library that was being read before any of this existed.
func (b *Builder) Sweep(ctx context.Context, limit int) int {
	ids, err := store.BooksNeedingTextIndex(ctx, b.store.Reader(), limit)
	if err != nil {
		slog.Debug("listing books to measure", "err", err)
		return 0
	}

	var done int
	for _, id := range ids {
		if ctx.Err() != nil {
			return done
		}
		if err := b.Ensure(ctx, id); err == nil {
			done++
		}
	}
	return done
}
