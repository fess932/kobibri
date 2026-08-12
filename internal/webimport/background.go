package webimport

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fess932/novelkit/job"

	"github.com/fess932/kobibri/internal/store"
)

// Status is a running import, as the interface shows it.
type Status struct {
	URL       string
	EditionID string
	Title     string
	Done      int
	Total     int
	ETA       time.Duration
	StartedAt time.Time
	Err       string
	Finished  bool
}

// Percent is how far along the download is.
func (s Status) Percent() int {
	if s.Total <= 0 {
		return 0
	}
	return s.Done * 100 / s.Total
}

// runner tracks imports that are under way.
//
// A serial can run to hundreds of chapters and the site has to be asked politely
// for each one, so an import is far too slow to hold a browser request open.
type runner struct {
	mu      sync.Mutex
	running map[string]*Status
}

func newRunner() *runner { return &runner{running: map[string]*Status{}} }

func key(url, editionID string) string { return url + "#" + editionID }

// Start begins an import in the background, unless the same one is already
// under way. It returns whether it started one.
//
// The context is deliberately not the request's: the browser navigating away
// must not abandon a download half done.
func (im *Importer) Start(ctx context.Context, rawURL string, opts ImportOptions) bool {
	k := key(rawURL, opts.EditionID)

	im.runner.mu.Lock()
	if st, ok := im.runner.running[k]; ok && !st.Finished {
		im.runner.mu.Unlock()
		return false
	}
	im.runner.running[k] = &Status{
		URL: rawURL, EditionID: opts.EditionID, StartedAt: time.Now(),
	}
	im.runner.mu.Unlock()

	go func() {
		res, err := im.importWithProgress(ctx, rawURL, opts)

		im.runner.mu.Lock()
		defer im.runner.mu.Unlock()
		st := im.runner.running[k]
		if st == nil {
			return
		}
		st.Finished = true
		if err != nil {
			st.Err = err.Error()
			slog.Error("import failed", "url", rawURL, "err", err)
			return
		}
		st.Title = res.Title
		st.Done, st.Total = res.Chapters, res.Chapters
		slog.Info("imported a book", "url", rawURL, "title", res.Title, "chapters", res.Chapters)
	}()
	return true
}

// Running returns a snapshot of the imports under way, plus those that finished
// recently enough to still be worth showing.
func (im *Importer) Running() []Status {
	im.runner.mu.Lock()
	defer im.runner.mu.Unlock()

	out := make([]Status, 0, len(im.runner.running))
	for k, st := range im.runner.running {
		// A finished import stays visible for a minute so the person who
		// started it sees how it went, then clears itself away.
		if st.Finished && time.Since(st.StartedAt) > time.Minute && st.Err == "" {
			delete(im.runner.running, k)
			continue
		}
		out = append(out, *st)
	}
	return out
}

// Busy reports whether anything is downloading, so a page can decide whether it
// is worth refreshing itself.
func (im *Importer) Busy() bool {
	im.runner.mu.Lock()
	defer im.runner.mu.Unlock()
	for _, st := range im.runner.running {
		if !st.Finished {
			return true
		}
	}
	return false
}

// importWithProgress is Import with the chapter counter wired up.
func (im *Importer) importWithProgress(ctx context.Context, rawURL string, opts ImportOptions) (Result, error) {
	k := key(rawURL, opts.EditionID)

	opts.onProgress = func(e job.Event) {
		im.runner.mu.Lock()
		defer im.runner.mu.Unlock()
		if st := im.runner.running[k]; st != nil {
			st.Done, st.Total, st.ETA = e.Progress.Done, e.Progress.Total, e.ETA
		}
	}
	return im.Import(ctx, rawURL, opts)
}

// RefreshAll re-runs every import, picking up newly published chapters. It is
// what the periodic check calls.
func (im *Importer) RefreshAll(ctx context.Context) {
	imports, err := im.List(ctx)
	if err != nil {
		slog.Error("listing imported books", "err", err)
		return
	}

	for _, it := range imports {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, err := im.Import(ctx, it.URL, ImportOptions{EditionID: editionOf(ctx, im, it)}); err != nil {
			slog.Warn("checking for new chapters", "url", it.URL, "err", err)
		}
	}
}

func editionOf(ctx context.Context, im *Importer, it Imported) string {
	var edition string
	im.store.Reader().QueryRowContext(ctx,
		`SELECT edition_id FROM web_imports WHERE source_book_id = ?`, it.SourceBookID).Scan(&edition)
	return edition
}

var _ = store.Now
