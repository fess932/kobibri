package ingest

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/store"
)

// Backoff bounds for a source we cannot reach. An unmounted share should be
// retried often enough to notice it coming back, but not so often that the logs
// become useless.
const (
	minBackoff = 1 * time.Minute
	maxBackoff = 6 * time.Hour
	jitterFrac = 0.10
)

// Scheduler runs periodic scans, one goroutine per source.
//
// Scans are serialised globally: the writer pool is a single connection anyway,
// and two libraries being merged at once would only contend for it.
type Scheduler struct {
	scanner *Scanner
	store   *store.Store

	mu       sync.Mutex
	triggers map[int64]chan struct{}
	running  bool

	scanSlot chan struct{}
	wg       sync.WaitGroup
}

func NewScheduler(scanner *Scanner, st *store.Store) *Scheduler {
	return &Scheduler{
		scanner:  scanner,
		store:    st,
		triggers: map[int64]chan struct{}{},
		scanSlot: make(chan struct{}, 1),
	}
}

// Start launches a worker per enabled source and returns. Call Stop, or cancel
// the context, to wind it down.
func (s *Scheduler) Start(ctx context.Context) error {
	sources, err := store.ListSources(ctx, s.store.Reader())
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	for _, src := range sources {
		if src.Enabled {
			s.startWorker(ctx, src.ID)
		}
	}
	return nil
}

// Stop waits for the workers to finish. The context passed to Start must
// already be cancelled.
func (s *Scheduler) Stop() {
	s.wg.Wait()
	s.mu.Lock()
	s.running = false
	s.triggers = map[int64]chan struct{}{}
	s.mu.Unlock()
}

// Trigger asks for a scan of one source as soon as a slot frees up. It never
// blocks: a request arriving while one is already pending is folded into it.
func (s *Scheduler) Trigger(sourceID int64) {
	s.mu.Lock()
	ch := s.triggers[sourceID]
	s.mu.Unlock()

	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Scheduler) startWorker(ctx context.Context, sourceID int64) {
	s.mu.Lock()
	if _, exists := s.triggers[sourceID]; exists {
		s.mu.Unlock()
		return
	}
	trigger := make(chan struct{}, 1)
	s.triggers[sourceID] = trigger
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(ctx, sourceID, trigger)
	}()
}

func (s *Scheduler) run(ctx context.Context, sourceID int64, trigger <-chan struct{}) {
	// Scan once shortly after start rather than immediately, so a server
	// restart does not hammer every library at once.
	timer := time.NewTimer(jitter(5 * time.Second))
	defer timer.Stop()

	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}

		src, err := store.GetSource(ctx, s.store.Reader(), sourceID)
		if err != nil {
			slog.Error("scheduler: source disappeared", "source", sourceID, "err", err)
			return
		}
		if !src.Enabled {
			// The source was turned off; the worker exits and Start will make a
			// new one if it is turned back on.
			s.mu.Lock()
			delete(s.triggers, sourceID)
			s.mu.Unlock()
			return
		}

		fails = s.scanOnce(ctx, src, fails)

		wait := jitter(time.Duration(src.ScanIntervalSec) * time.Second)
		if fails > 0 {
			wait = backoff(fails)
		}
		timer.Reset(wait)
	}
}

// scanOnce runs one scan under the global slot and returns the new failure
// count.
func (s *Scheduler) scanOnce(ctx context.Context, src *store.Source, fails int) int {
	select {
	case s.scanSlot <- struct{}{}:
	case <-ctx.Done():
		return fails
	}
	defer func() { <-s.scanSlot }()

	start := time.Now()
	res, err := s.scanner.Scan(ctx, src.ID, ScanOptions{})
	switch {
	case err == nil:
		if !res.Skipped {
			slog.Info("scanned source", "source", src.Name, "seen", res.Seen,
				"added", res.Added, "updated", res.Updated, "vanished", res.Vanished,
				"took", time.Since(start).Round(time.Millisecond))
		}
		return 0

	case errors.Is(err, context.Canceled):
		return fails

	case errors.Is(err, calibre.ErrUnreachable):
		// Nothing was written; the library is simply not there right now.
		slog.Warn("source unreachable, backing off", "source", src.Name,
			"fails", fails+1, "err", err)
		return fails + 1

	case errors.Is(err, ErrSuspicious):
		// Retrying will not help: this needs a human to confirm in the UI.
		// Keep polling at the normal interval so the moment the library comes
		// back whole, the scan simply succeeds.
		slog.Warn("scan refused: too many books vanished", "source", src.Name, "err", err)
		return 0

	default:
		slog.Error("scan failed", "source", src.Name, "fails", fails+1, "err", err)
		return fails + 1
	}
}

func backoff(fails int) time.Duration {
	d := minBackoff << min(fails-1, 16)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return jitter(d)
}

// jitter spreads timers so several sources do not line up on the same tick.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	delta := float64(d) * jitterFrac
	return time.Duration(float64(d) - delta + rand.Float64()*2*delta)
}
