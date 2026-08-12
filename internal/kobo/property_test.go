package kobo_test

import (
	"flag"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// A scripted test only finds what its author already suspected. This one throws
// random sequences of everything that can happen — scans, edits, a library
// going away and coming back, books hidden, syncs cut off partway, a book
// deleted on a device — at two devices at once, and checks after every step that
// the promise this whole design exists to keep still holds:
//
//	a device never loses a book it was given.
//
// Two things make a failure usable rather than a curiosity: the seed is printed
// and can be replayed with -seed, and the operations are logged as they run, so
// a failing sequence reads as a story rather than as a state dump.

var seedFlag = flag.Int64("seed", 0, "seed for the property test; 0 picks one per run")

func TestPropertyDevicesNeverLoseBooks(t *testing.T) {
	if testing.Short() {
		t.Skip("property test")
	}

	rounds := 12
	if testing.Short() {
		rounds = 3
	}

	for round := range rounds {
		seed := *seedFlag
		if seed == 0 {
			// Derived from the round rather than the clock: a run is reproducible
			// from its own output, and a clock makes a failure someone else's
			// problem.
			seed = int64(round)*7919 + 104729
		}

		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			runProperty(t, seed, 30)
		})

		if *seedFlag != 0 {
			break // an explicit seed means one run of that seed
		}
	}
}

// world is the server, its libraries, its devices, and what each device is owed.
type world struct {
	t       *testing.T
	env     *env
	scanner *ingest.Scanner
	rng     *rand.Rand
	log     []string

	libs    []*calibretest.Library
	sources []int64
	devices []*fakeKobo

	// everSeen is what each device was given at least once. Only the device
	// itself may take something off that list.
	everSeen []map[string]bool
	// selfDeleted is what each device chose to delete on itself.
	selfDeleted []map[string]bool
	// hidden is what the operator told the server to stop offering, which is the
	// one way the server may take a book off a device.
	hidden map[string]bool
	// seen counts what actually ran, so a run that quietly exercises three of
	// nine operations can be told from one that exercises all of them.
	seen map[string]int
}

func runProperty(t *testing.T, seed int64, steps int) {
	w := newWorld(t, seed)

	for step := range steps {
		op := w.pick()
		w.note("%2d %s", step, op.name)
		op.run()
		w.check(op.name)
	}

	// Everything settles: a last clean sync must leave both devices whole.
	w.note("-- final sync")
	for _, d := range w.devices {
		d.sync()
	}
	w.check("after a final sync")
}

func newWorld(t *testing.T, seed int64) *world {
	e := newEnvWith(t, envOptions{SyncBatch: 5})

	w := &world{
		t: t, env: e, rng: rand.New(rand.NewSource(seed)),
		scanner: ingest.NewScanner(e.store, filepath.Join(t.TempDir(), "tmp")),
		hidden:  map[string]bool{},
		seen:    map[string]int{},
	}

	for i := range 2 {
		lib := calibretest.New(t, seriesOfBooks(fmt.Sprintf("Lib%d", i), 8)...)
		w.libs = append(w.libs, lib)
		w.sources = append(w.sources,
			addSource(t, e, fmt.Sprintf("source-%d", i), lib.Path, 10+i*10))
	}
	for _, id := range w.sources {
		w.scan(id)
	}

	for i := range 2 {
		d := newFakeKoboAs(t, e, fmt.Sprintf("device-%d", i))
		d.sync()
		w.devices = append(w.devices, d)

		seen := map[string]bool{}
		for id := range d.library {
			seen[id] = true
		}
		w.everSeen = append(w.everSeen, seen)
		w.selfDeleted = append(w.selfDeleted, map[string]bool{})
	}
	return w
}

type operation struct {
	name string
	run  func()
}

// pick chooses what happens next. The weights are not uniform on purpose: syncs
// and scans are what a server actually spends its life doing, and the rare
// events are the ones worth interleaving with them.
func (w *world) pick() operation {
	op := w.choose()
	w.seen[op.name]++
	return op
}

func (w *world) choose() operation {
	switch w.rng.Intn(100) {
	case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19,
		20, 21, 22, 23, 24:
		d := w.someDevice()
		return operation{fmt.Sprintf("sync %s", d.deviceID), func() { d.sync() }}

	case 25, 26, 27, 28, 29, 30, 31, 32, 33, 34:
		d := w.someDevice()
		return operation{
			fmt.Sprintf("sync %s, interrupted", d.deviceID),
			func() { d.syncOnce() },
		}

	case 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49:
		i := w.rng.Intn(len(w.sources))
		return operation{fmt.Sprintf("scan source-%d", i), func() { w.scan(w.sources[i]) }}

	case 50, 51, 52, 53, 54, 55, 56, 57, 58, 59:
		i := w.rng.Intn(len(w.libs))
		id := int64(w.rng.Intn(8) + 1)
		return operation{
			fmt.Sprintf("edit book %d in lib%d", id, i),
			func() {
				w.libs[i].Exec(`UPDATE books SET title = title || ' (ed)',
					last_modified = '2031-01-01 00:00:00.000000+00:00' WHERE id = ?`, id)
				w.scan(w.sources[i])
			},
		}

	case 60, 61, 62, 63, 64:
		i := w.rng.Intn(len(w.libs))
		id := int64(w.rng.Intn(8) + 1)
		return operation{
			fmt.Sprintf("remove book %d from lib%d", id, i),
			func() {
				w.libs[i].Remove(id)
				w.scan(w.sources[i])
			},
		}

	case 65, 66, 67, 68, 69, 70, 71, 72, 73, 74:
		i := w.rng.Intn(len(w.sources))
		enabled := w.rng.Intn(2) == 0
		return operation{
			fmt.Sprintf("source-%d enabled=%v", i, enabled),
			func() {
				if err := w.scanner.SetSourceEnabled(w.env.ctx, w.sources[i], enabled); err != nil {
					w.fatal("switching a source: %v", err)
				}
			},
		}

	case 75, 76, 77, 78, 79, 80, 81, 82, 83, 84:
		d := w.someDevice()
		book := w.someBookOn(d)
		if book == "" {
			return operation{"nothing to delete", func() {}}
		}
		i := w.deviceIndex(d)
		return operation{
			fmt.Sprintf("%s deletes a book", d.deviceID),
			func() {
				w.deleteOnDevice(d, book)
				w.selfDeleted[i][book] = true
			},
		}

	case 85, 86, 87:
		// The very thing this design was built for: a library goes away
		// entirely and comes back. Nothing may be lost, and nothing may arrive
		// twice under a new identity.
		i := w.rng.Intn(len(w.sources))
		return operation{
			fmt.Sprintf("source-%d removed and added back", i),
			func() {
				lib := w.libs[i]
				if err := store.DeleteSource(w.env.ctx, w.env.store.Writer(), w.sources[i]); err != nil {
					w.fatal("removing a source: %v", err)
				}
				w.sources[i] = addSource(w.t, w.env, fmt.Sprintf("source-%d", i), lib.Path, 10+i*10)
				w.scan(w.sources[i])
			},
		}

	case 88, 89:
		// The operator's emergency button. It throws away what the server
		// remembers of a device, and must not throw away the device's books.
		d := w.someDevice()
		return operation{
			fmt.Sprintf("reset sync state for %s", d.deviceID),
			func() {
				devices, err := store.ListAllDevices(w.env.ctx, w.env.store.Reader())
				if err != nil {
					w.fatal("listing devices: %v", err)
				}
				for _, row := range devices {
					if row.KoboDeviceID != d.deviceID {
						continue
					}
					if err := store.ResetDeviceSyncState(w.env.ctx, w.env.store.Writer(), row.ID); err != nil {
						w.fatal("resetting sync state: %v", err)
					}
				}
				d.token = "" // the device starts over from nothing
			},
		}

	case 90, 91, 92:
		book := w.someBook()
		if book == "" {
			return operation{"nothing to hide", func() {}}
		}
		hide := !w.hidden[book]
		return operation{
			fmt.Sprintf("hidden=%v for a book", hide),
			func() {
				if err := store.SetBookHidden(w.env.ctx, w.env.store.Writer(), book, hide); err != nil {
					w.fatal("hiding a book: %v", err)
				}
				w.hidden[book] = hide
			},
		}

	default:
		d := w.someDevice()
		book := w.someBookOn(d)
		if book == "" {
			return operation{"nothing to read", func() {}}
		}
		return operation{
			fmt.Sprintf("%s reports progress", d.deviceID),
			func() { w.reportProgress(d, book) },
		}
	}
}

// check is the invariant, tested after every single operation.
func (w *world) check(after string) {
	w.t.Helper()

	for i, d := range w.devices {
		for id := range w.everSeen[i] {
			switch {
			case d.library[id] != "":
				// still there, which is the point
			case w.selfDeleted[i][id]:
				// gone because the device said so
			case w.hidden[id]:
				// gone because the operator retracted it, which is the only
				// other way a book may leave a device
			default:
				w.dump()
				w.t.Fatalf("%s: %s lost book %s, which nobody deleted and nothing hid",
					after, d.deviceID, id)
			}
		}

		for id := range d.library {
			if w.selfDeleted[i][id] {
				w.dump()
				w.t.Fatalf("%s: %s got back book %s after deleting it", after, d.deviceID, id)
			}
		}

		// A book must never arrive twice under two identities. This is what
		// makes removing a library and adding it back a non-event: the canonical
		// id is derived from the book, so the device sees nothing at all.
		byTitle := map[string]string{}
		for id, title := range d.library {
			if other, clash := byTitle[title]; clash {
				w.dump()
				w.t.Fatalf("%s: %s holds %q twice, as %s and %s",
					after, d.deviceID, title, other, id)
			}
			byTitle[title] = id
		}

		// Everything the device now holds counts as seen from here on.
		for id := range d.library {
			w.everSeen[i][id] = true
		}
	}
}

func (w *world) scan(id int64) {
	w.t.Helper()
	_, err := w.scanner.Scan(w.env.ctx, id, ingest.ScanOptions{Force: true, ConfirmVanish: true})
	if err != nil {
		w.dump()
		w.t.Fatalf("scan %d: %v", id, err)
	}
}

func (w *world) deleteOnDevice(d *fakeKobo, bookID string) {
	w.t.Helper()
	resp := w.env.doAsDevice(d.deviceID, "DELETE", w.env.kobo("/v1/library/"+bookID), "")
	if resp.StatusCode != 204 {
		w.dump()
		w.t.Fatalf("deleting %s on %s: status %d", bookID, d.deviceID, resp.StatusCode)
	}
	delete(d.library, bookID)
}

func (w *world) reportProgress(d *fakeKobo, bookID string) {
	w.t.Helper()
	body := `{"ReadingStates":[{"EntitlementId":"` + bookID + `",
		"LastModified":"2031-02-02T10:00:00Z",
		"StatusInfo":{"Status":"Reading","LastModified":"2031-02-02T10:00:00Z"},
		"CurrentBookmark":{"ProgressPercent":10,"ContentSourceProgressPercent":20,
			"LastModified":"2031-02-02T10:00:00Z"}}]}`
	resp := w.env.doAsDevice(d.deviceID, "PUT", w.env.kobo("/v1/library/"+bookID+"/state"), body)
	if resp.StatusCode != 200 {
		w.dump()
		w.t.Fatalf("reporting progress: status %d", resp.StatusCode)
	}
}

func (w *world) someDevice() *fakeKobo { return w.devices[w.rng.Intn(len(w.devices))] }

func (w *world) deviceIndex(d *fakeKobo) int {
	for i, other := range w.devices {
		if other == d {
			return i
		}
	}
	return -1
}

// someBookOn picks a book the device holds, deterministically for a given seed:
// ranging over a map would make a failing run unreproducible.
func (w *world) someBookOn(d *fakeKobo) string {
	ids := sortedKeys(d.library)
	if len(ids) == 0 {
		return ""
	}
	return ids[w.rng.Intn(len(ids))]
}

func (w *world) someBook() string {
	for _, d := range w.devices {
		if id := w.someBookOn(d); id != "" {
			return id
		}
	}
	return ""
}

func (w *world) note(format string, args ...any) {
	w.log = append(w.log, fmt.Sprintf(format, args...))
}

func (w *world) fatal(format string, args ...any) {
	w.dump()
	w.t.Fatalf(format, args...)
}

// dump prints the sequence that led here, which is the only thing that makes a
// random failure worth having.
func (w *world) dump() {
	w.t.Helper()
	w.t.Log("what happened:")
	for _, line := range w.log {
		w.t.Log("   " + line)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// A stable order is what keeps a seed reproducible.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// A random test that never reaches half its operations is a slower version of a
// smaller one. This reports what the seeds actually exercise.
func TestPropertyRunsEveryKindOfOperation(t *testing.T) {
	if testing.Short() {
		t.Skip("property test")
	}

	kinds := map[string]bool{}
	for round := range 12 {
		w := newWorld(t, int64(round)*7919+104729)
		for step := range 30 {
			op := w.pick()
			w.note("%2d %s", step, op.name)
			op.run()
			w.check(op.name)
		}
		for name := range w.seen {
			kinds[kindOf(name)] = true
		}
	}

	want := []string{"sync", "sync-interrupted", "scan", "edit", "remove-book",
		"enable", "delete-on-device", "readd-source", "reset-sync", "hide", "progress"}
	for _, k := range want {
		if !kinds[k] {
			t.Errorf("no seed ever performed %q", k)
		}
	}
	t.Logf("operations exercised: %d of %d", len(kinds), len(want))
}

// kindOf strips the varying parts of an operation's name.
func kindOf(name string) string {
	switch {
	case strings.Contains(name, "interrupted"):
		return "sync-interrupted"
	case strings.HasPrefix(name, "sync "):
		return "sync"
	case strings.HasPrefix(name, "scan "):
		return "scan"
	case strings.HasPrefix(name, "edit "):
		return "edit"
	case strings.HasPrefix(name, "remove book"):
		return "remove-book"
	case strings.Contains(name, "removed and added back"):
		return "readd-source"
	case strings.Contains(name, "reset sync state"):
		return "reset-sync"
	case strings.Contains(name, "enabled="):
		return "enable"
	case strings.Contains(name, "deletes a book"):
		return "delete-on-device"
	case strings.Contains(name, "hidden="):
		return "hide"
	case strings.Contains(name, "reports progress"):
		return "progress"
	}
	return name
}
