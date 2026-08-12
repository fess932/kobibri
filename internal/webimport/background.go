package webimport

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fess932/novelkit/job"

	"github.com/fess932/kobibri/internal/store"
)

// Status is an import that is running, or has just finished.
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

// runner keeps one import per book at a time.
//
// Without it, a periodic check and someone pressing the button could download
// the same book twice at once, into the same cache directory and over the same
// assembled file.
type runner struct {
	mu      sync.Mutex
	running map[string]*Status
}

func newRunner() *runner { return &runner{running: map[string]*Status{}} }

func key(url, editionID string) string { return url + "#" + editionID }

// acquire claims a book. It returns false when that book is already being
// downloaded.
func (r *runner) acquire(url, editionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := key(url, editionID)
	if st, ok := r.running[k]; ok && !st.Finished {
		return false
	}
	r.running[k] = &Status{URL: url, EditionID: editionID, StartedAt: time.Now()}
	return true
}

func (r *runner) progress(url, editionID string, e job.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.running[key(url, editionID)]; st != nil {
		st.Done, st.Total, st.ETA = e.Progress.Done, e.Progress.Total, e.ETA
	}
}

func (r *runner) finish(url, editionID string, res Result, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.running[key(url, editionID)]
	if st == nil {
		return
	}
	st.Finished = true
	if err != nil {
		st.Err = err.Error()
		return
	}
	st.Title = res.Title
	st.Done, st.Total = res.Chapters, res.Chapters
}

// snapshot returns what to show, forgetting quiet successes after a while.
func (r *runner) snapshot() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Status, 0, len(r.running))
	for k, st := range r.running {
		// A finished import stays visible for a minute so whoever started it
		// sees how it went; a failed one stays until it is tried again.
		if st.Finished && st.Err == "" && time.Since(st.StartedAt) > time.Minute {
			delete(r.running, k)
			continue
		}
		out = append(out, *st)
	}
	return out
}

func (r *runner) busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.running {
		if !st.Finished {
			return true
		}
	}
	return false
}

// run performs one import under the guard, reporting progress as it goes.
func (im *Importer) run(ctx context.Context, rawURL string, opts ImportOptions) (Result, error) {
	if !im.runner.acquire(rawURL, opts.EditionID) {
		return Result{}, errAlreadyRunning
	}

	opts.onProgress = func(e job.Event) { im.runner.progress(rawURL, opts.EditionID, e) }

	res, err := im.Import(ctx, rawURL, opts)
	im.runner.finish(rawURL, opts.EditionID, res, err)
	return res, err
}

var errAlreadyRunning = errors.New("that book is already downloading")

// Start begins an import in the background and says whether it started one.
//
// The context is deliberately not a request's: a browser navigating away must
// not abandon a download half done.
func (im *Importer) Start(ctx context.Context, rawURL string, opts ImportOptions) bool {
	if !im.runner.acquire(rawURL, opts.EditionID) {
		return false
	}

	go func() {
		opts.onProgress = func(e job.Event) { im.runner.progress(rawURL, opts.EditionID, e) }

		res, err := im.Import(ctx, rawURL, opts)
		im.runner.finish(rawURL, opts.EditionID, res, err)
		if err != nil {
			slog.Error("import failed", "url", rawURL, "err", err)
			return
		}
		slog.Info("imported a book", "url", rawURL, "title", res.Title, "chapters", res.Chapters)
	}()
	return true
}

// StartRefresh checks one imported book for new chapters, in the background.
func (im *Importer) StartRefresh(ctx context.Context, bookID string) error {
	url, edition, err := im.linkOf(ctx, bookID)
	if err != nil {
		return err
	}
	if !im.Start(ctx, url, ImportOptions{EditionID: edition}) {
		return errAlreadyRunning
	}
	return nil
}

// Running returns what to show on the page.
func (im *Importer) Running() []Status { return im.runner.snapshot() }

// Busy reports whether anything is downloading, so a page can decide whether it
// is worth refreshing itself.
func (im *Importer) Busy() bool { return im.runner.busy() }

// RefreshAll checks every imported book for newly published chapters.
//
// One at a time, on purpose: these are other people's sites, and a serial that
// is a few hours out of date is not worth hammering them for.
func (im *Importer) RefreshAll(ctx context.Context) {
	imports, err := im.List(ctx)
	if err != nil {
		slog.Error("listing imported books", "err", err)
		return
	}

	var checked, updated int
	for _, it := range imports {
		select {
		case <-ctx.Done():
			return
		default:
		}

		before := it.ChaptersTotal
		res, err := im.run(ctx, it.URL, ImportOptions{EditionID: it.EditionID})
		switch {
		case errors.Is(err, errAlreadyRunning):
			continue
		case err != nil:
			slog.Warn("checking for new chapters", "url", it.URL, "err", err)
			continue
		}
		checked++
		if res.Chapters > before {
			updated++
			slog.Info("new chapters", "title", res.Title,
				"was", before, "now", res.Chapters)
		}
	}

	if checked > 0 {
		slog.Info("checked imported books for new chapters",
			"checked", checked, "updated", updated)
	}
}

// lastRefreshKey remembers when the last sweep ran, so restarting the server does
// not start one.
const lastRefreshKey = "webimport:last_refresh"

// RunPeriodicRefresh checks for new chapters on a timer until the context ends.
//
// The last sweep is remembered in the database rather than only in memory. A
// server that is restarted often would otherwise hammer every site it knows on
// every start, which is both rude and a good way to get a token refused.
func (im *Importer) RunPeriodicRefresh(ctx context.Context, every time.Duration) {
	if every <= 0 {
		slog.Info("periodic checking for new chapters is switched off")
		return
	}

	// Not immediately on start even when one is due: a restart should not set
	// every site going at once, and nothing here is urgent.
	wait := time.Minute
	if due := im.nextRefreshIn(ctx, every); due > wait {
		wait = due
		slog.Info("next check for new chapters", "in", wait.Round(time.Minute))
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		im.RefreshAll(ctx)
		im.recordRefresh(ctx)
		timer.Reset(every)
	}
}

// nextRefreshIn is how long is left of the interval, or zero when one is due.
func (im *Importer) nextRefreshIn(ctx context.Context, every time.Duration) time.Duration {
	raw, err := store.GetKV(ctx, im.store.Reader(), lastRefreshKey)
	if err != nil || raw == "" {
		return 0
	}
	last := store.ParseTime(raw)
	if last.IsZero() {
		return 0
	}
	if left := every - time.Since(last); left > 0 {
		return left
	}
	return 0
}

func (im *Importer) recordRefresh(ctx context.Context) {
	if err := store.SetKV(ctx, im.store.Writer(), lastRefreshKey, store.Now()); err != nil {
		slog.Warn("recording the last check for new chapters", "err", err)
	}
}

func (im *Importer) linkOf(ctx context.Context, bookID string) (url, editionID string, err error) {
	err = im.store.Reader().QueryRowContext(ctx, `
		SELECT w.url, w.edition_id FROM web_imports w
		JOIN source_books sb ON sb.id = w.source_book_id
		WHERE sb.book_id = ? LIMIT 1`, bookID).Scan(&url, &editionID)
	return url, editionID, err
}
