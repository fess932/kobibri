package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/fess932/kobibri/internal/config"
	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
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
	cover *covers.Cache, cfg *config.Config) {

	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep(ctx, st, kepub, cover, cfg)
		}
	}
}

func sweep(ctx context.Context, st *store.Store, kepub *kepubconv.Cache,
	cover *covers.Cache, cfg *config.Config) {

	if err := kepub.Evict(ctx, cfg.KepubCacheBytes); err != nil {
		slog.Debug("evicting converted books", "err", err)
	}
	if err := cover.Evict(cfg.CoverCacheBytes); err != nil {
		slog.Debug("evicting covers", "err", err)
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
}
