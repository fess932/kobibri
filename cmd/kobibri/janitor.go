package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/fess932/kobibri/internal/config"
	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
	"github.com/fess932/kobibri/internal/textindex"
)

// janitorInterval is how often the housekeeping pass runs. Nothing it does is
// urgent; the point is that none of it accumulates without bound.
const janitorInterval = time.Hour

// abandonedSyncPointAge is how long a half-finished snapshot is kept. A device
// that walked away mid-sync usually comes back within minutes, but keeping a
// week of them costs almost nothing and covers a reader left in a drawer.
const abandonedSyncPointAge = 7 * 24 * time.Hour

// runJanitor trims everything that grows on its own until the context ends.
//
// The caches are rebuildable, so their budgets are ceilings rather than
// promises. Snapshots and sessions are cheap but unbounded, and a device's
// single completed snapshot is never touched — it is the baseline its next sync
// diffs against.
func runJanitor(ctx context.Context, st *store.Store, kepub *kepubconv.Cache,
	cover *covers.Cache, ebook *ebookconv.Cache, cfg *config.Config) {

	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep(ctx, st, kepub, cover, ebook, cfg)
		}
	}
}

// syncHistoryPerDevice is how many syncs are kept per reader.
const syncHistoryPerDevice = 50

func sweep(ctx context.Context, st *store.Store, kepub *kepubconv.Cache,
	cover *covers.Cache, ebook *ebookconv.Cache, cfg *config.Config) {

	if err := kepub.Evict(ctx, cfg.KepubCacheBytes); err != nil {
		slog.Debug("evicting converted books", "err", err)
	}
	if err := cover.Evict(cfg.CoverCacheBytes); err != nil {
		slog.Debug("evicting covers", "err", err)
	}
	if err := ebook.Evict(ctx, cfg.EpubCacheBytes); err != nil {
		slog.Debug("evicting converted epubs", "err", err)
	}

	cutoff := store.FormatTime(time.Now().Add(-abandonedSyncPointAge))
	if n, err := store.GCSyncPoints(ctx, st.Writer(), cutoff); err != nil {
		slog.Debug("removing stale sync points", "err", err)
	} else if n > 0 {
		slog.Info("removed stale sync points", "count", n)
	}

	if err := store.DeleteExpiredSessions(ctx, st.Writer()); err != nil {
		slog.Debug("removing expired sessions", "err", err)
	}

	// The sync history is for answering "what did my reader get, and when", which
	// nobody asks about last spring.
	if err := store.TrimSyncRuns(ctx, st.Writer(), syncHistoryPerDevice); err != nil {
		slog.Debug("trimming the sync history", "err", err)
	}
}

// textIndexSweep is how often books that have been read but never measured are
// picked up. A reader's own progress reports measure the book they are in, so
// this is only for a library that was being read before any of it existed.
const textIndexSweep = 30 * time.Minute

// textIndexBatch bounds one pass. Measuring opens and parses the whole book.
const textIndexBatch = 20

func sweepTextIndex(ctx context.Context, indexer *textindex.Builder) {
	ticker := time.NewTicker(textIndexSweep)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := indexer.Sweep(ctx, textIndexBatch); n > 0 {
				slog.Debug("measured books for reading statistics", "books", n)
			}
		}
	}
}
