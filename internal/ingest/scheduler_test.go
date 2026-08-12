package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// Trigger must reach a worker and produce a real scan.
func TestSchedulerTriggerScans(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Scheduled"})
	sourceID := h.addSource("main", lib.Path, 100)

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	sched := ingest.NewScheduler(h.scanner, h.store)
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sched.Trigger(sourceID)

	waitFor(t, 5*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM books WHERE syncable = 1`) == 1
	}, "the triggered scan to ingest the book")

	cancel()
	sched.Stop()
}

// Triggering a disabled source must not resurrect it.
func TestSchedulerSkipsDisabledSources(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "Off"})
	sourceID := h.addSource("main", lib.Path, 100)

	if err := h.scanner.SetSourceEnabled(h.ctx, sourceID, false); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	sched := ingest.NewScheduler(h.scanner, h.store)
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sched.Trigger(sourceID)

	time.Sleep(300 * time.Millisecond)
	if n := h.count(`SELECT count(*) FROM source_books`); n != 0 {
		t.Errorf("%d source rows ingested for a disabled source, want 0", n)
	}

	cancel()
	sched.Stop()
}

// Stop must return once the context is cancelled, even with work queued.
func TestSchedulerStopsCleanly(t *testing.T) {
	h := newHarness(t)
	lib := calibretest.New(t, calibretest.BookSpec{Title: "One"})
	sourceID := h.addSource("main", lib.Path, 100)

	ctx, cancel := context.WithCancel(h.ctx)
	sched := ingest.NewScheduler(h.scanner, h.store)
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sched.Trigger(sourceID)
	cancel()

	done := make(chan struct{})
	go func() { sched.Stop(); close(done) }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return within 10s after the context was cancelled")
	}
}

// An unreachable source must not stop the scheduler from serving others.
func TestSchedulerToleratesUnreachableSource(t *testing.T) {
	h := newHarness(t)
	good := calibretest.New(t, calibretest.BookSpec{Title: "Reachable"})

	badID := h.addSource("gone", t.TempDir()+"/unmounted", 100)
	goodID := h.addSource("good", good.Path, 200)

	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()

	sched := ingest.NewScheduler(h.scanner, h.store)
	if err := sched.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sched.Trigger(badID)
	sched.Trigger(goodID)

	waitFor(t, 5*time.Second, func() bool {
		return h.count(`SELECT count(*) FROM books WHERE syncable = 1`) == 1
	}, "the reachable source to be scanned despite the broken one")

	src, err := store.GetSource(h.ctx, h.store.Reader(), badID)
	if err != nil {
		t.Fatal(err)
	}
	if src.LastStatus != store.SourceStatusUnreachable {
		t.Errorf("unreachable source status = %q, want %q",
			src.LastStatus, store.SourceStatusUnreachable)
	}

	cancel()
	sched.Stop()
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}
