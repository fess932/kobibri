package kobo_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
)

// Everything here has been tested on libraries of a few dozen books. The design
// was written for tens of thousands, and that claim has never been checked.
//
// This builds a library of a real size and measures what an operator would
// actually wait for: the first scan, the first sync of a device, and a sync when
// nothing has changed — which is the one that happens every few minutes forever
// and therefore the one that must be cheap.
//
//	go test ./internal/kobo/ -run Scale -v -books 20000 -timeout 30m
var bookCount = flag.Int("books", 5000, "how many books the scale test builds")

func TestScaleOfALargeLibrary(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}

	n := *bookCount
	e := newEnvWith(t, envOptions{SyncBatch: 100})

	build := time.Now()
	lib := calibretest.New(t, seriesOfBooks("Book", n)...)
	t.Logf("built a library of %d books in %s", n, since(build))

	scanner := ingest.NewScanner(e.store, filepath.Join(t.TempDir(), "tmp"))
	sourceID := addSource(t, e, "big", lib.Path, 10)

	start := time.Now()
	if _, err := scanner.Scan(e.ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	scanTook := time.Since(start)
	t.Logf("first scan:      %s (%s per book)", since(start), per(scanTook, n))

	// A second scan with nothing changed is the common case, and it must not
	// cost what the first one did.
	start = time.Now()
	if _, err := scanner.Scan(e.ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	t.Logf("unchanged rescan: %s", since(start))

	device := newFakeKoboAs(t, e, "big-device")

	start = time.Now()
	var requests int
	for {
		_, more := device.syncOnce()
		requests++
		if !more {
			break
		}
	}
	firstSync := time.Since(start)
	t.Logf("first sync:      %s over %d requests (%s per book)",
		since(start), requests, per(firstSync, n))

	if len(device.library) != n {
		t.Fatalf("the device got %d books, want %d", len(device.library), n)
	}

	// The sync that happens forever afterwards. This is the number that matters:
	// a device checks in every few minutes, and it must cost almost nothing.
	start = time.Now()
	items := device.sync()
	quiet := time.Since(start)
	t.Logf("quiet sync:      %s, %d items", since(start), len(items))

	if len(items) != 0 {
		t.Errorf("a sync with nothing changed returned %d items", len(items))
	}
	if quiet > 2*time.Second {
		t.Errorf("a sync with nothing to say took %s; it happens every few minutes forever", quiet)
	}

	t.Logf("database:        %s", size(t, e.dbPath))
}

func since(start time.Time) string { return time.Since(start).Round(time.Millisecond).String() }

func per(total time.Duration, n int) string {
	if n == 0 {
		return "-"
	}
	return (total / time.Duration(n)).Round(time.Microsecond).String()
}

func size(t *testing.T, path string) string {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil {
			total += fi.Size()
		}
	}
	return fmt.Sprintf("%.1f MB", float64(total)/(1<<20))
}
