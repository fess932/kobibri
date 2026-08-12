package kobo_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// TestSoak runs the whole thing against a moving library: two sources, two
// devices, books edited, a source going away and coming back, a book deleted on
// one device, syncs interrupted partway.
//
// The property under test is the one the design exists to provide: a device
// never loses a book it was given. Everything else here is noise designed to
// break that.
func TestSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test")
	}

	const booksPerSource = 60

	e := newEnvWith(t, envOptions{SyncBatch: 7})

	mainLib := calibretest.New(t, seriesOfBooks("Main", booksPerSource)...)
	backupLib := calibretest.New(t, seriesOfBooks("Backup", booksPerSource)...)

	scanner := ingest.NewScanner(e.store, filepath.Join(t.TempDir(), "tmp"))

	mainID := addSource(t, e, "main", mainLib.Path, 10)
	backupID := addSource(t, e, "backup", backupLib.Path, 50)

	scan := func(id int64) {
		t.Helper()
		if _, err := scanner.Scan(e.ctx, id, ingest.ScanOptions{Force: true, ConfirmVanish: true}); err != nil {
			t.Fatalf("scan %d: %v", id, err)
		}
	}
	scan(mainID)
	scan(backupID)

	one := newFakeKoboAs(t, e, "kobo-one")
	two := newFakeKoboAs(t, e, "kobo-two")

	one.sync()
	two.sync()

	total := 2 * booksPerSource
	if len(one.library) != total || len(two.library) != total {
		t.Fatalf("initial sync gave %d and %d books, want %d each",
			len(one.library), len(two.library), total)
	}

	// everSeen is what each device was given. Nothing in what follows may remove
	// an entry from a device's library. selfDeleted records what a device chose
	// to delete on itself, which is the only legitimate way to lose a book.
	everSeen := map[string]bool{}
	for id := range one.library {
		everSeen[id] = true
	}
	selfDeleted := map[string]bool{}

	// A book is edited: the device should learn the new title and keep the book.
	mainLib.Exec(`UPDATE books SET title = 'Main 00 renamed',
		last_modified = '2030-01-01 00:00:00.000000+00:00' WHERE id = 1`)
	scan(mainID)
	one.sync()
	if !one.titles()["Main 00 renamed"] {
		t.Error("an edited title did not reach the device")
	}

	// A book is deleted from Calibre entirely. It must stay on both devices.
	mainLib.Remove(2)
	scan(mainID)
	assertNoLoss(t, "after a book was deleted from Calibre", one, two, everSeen, selfDeleted)

	// A whole source goes away, as an unmounted share eventually looks.
	if err := scanner.SetSourceEnabled(e.ctx, backupID, false); err != nil {
		t.Fatal(err)
	}
	assertNoLoss(t, "after a source was switched off", one, two, everSeen, selfDeleted)

	// And comes back.
	if err := scanner.SetSourceEnabled(e.ctx, backupID, true); err != nil {
		t.Fatal(err)
	}
	scan(backupID)
	assertNoLoss(t, "after the source came back", one, two, everSeen, selfDeleted)

	// One device deletes a book. The other must keep it.
	devices, err := store.ListDevices(e.ctx, e.store.Reader(), e.userID)
	if err != nil {
		t.Fatal(err)
	}
	var deviceOne int64
	for _, d := range devices {
		if d.KoboDeviceID == "kobo-one" {
			deviceOne = d.ID
		}
	}
	if deviceOne == 0 {
		t.Fatal("kobo-one was never registered")
	}

	var deleted string
	for id := range one.library {
		deleted = id
		break
	}
	if err := store.AddTombstone(e.ctx, e.store.Writer(), deviceOne, deleted); err != nil {
		t.Fatal(err)
	}
	one.sync()
	delete(everSeen, deleted)
	selfDeleted[deleted] = true

	if one.library[deleted] != "" {
		t.Error("the deleted book is still on the device that deleted it")
	}
	if two.library[deleted] == "" {
		t.Error("deleting on one device removed the book from the other")
	}

	// A sync interrupted at every cursor position must still converge.
	//
	// The device stops partway and then restarts: it loses its sync token, but
	// not the books already on its storage. That asymmetry is the whole point —
	// the server has to work out what it still owes a device that cannot tell
	// it where it left off.
	for stopAfter := 1; stopAfter <= 4; stopAfter++ {
		mainLib.Exec(fmt.Sprintf(
			`UPDATE books SET title = 'Main %02d edit %d',
			 last_modified = '2031-01-0%d 00:00:00.000000+00:00' WHERE id = %d`,
			stopAfter+2, stopAfter, stopAfter, stopAfter+3))
		scan(mainID)

		for range stopAfter {
			if _, more := two.syncOnce(); !more {
				break
			}
		}

		two.token = "" // restarted
		two.sync()

		for id := range everSeen {
			if two.library[id] == "" {
				t.Fatalf("a sync interrupted after %d request(s) lost book %s", stopAfter, id)
			}
		}
	}

	// A final quiet sync must have nothing left to say.
	if items := two.sync(); len(items) != 0 {
		t.Errorf("the library never settled: %v", kinds(items))
	}

	assertNoLoss(t, "at the end", one, two, everSeen, selfDeleted)
}

// assertNoLoss is the invariant: a device never loses a book it was given, and
// is only ever told to archive one it deleted on itself.
func assertNoLoss(t *testing.T, when string, one, two *fakeKobo,
	everSeen, selfDeleted map[string]bool) {
	t.Helper()
	one.sync()
	two.sync()

	for _, dev := range []struct {
		name string
		k    *fakeKobo
	}{{"kobo-one", one}, {"kobo-two", two}} {
		for id := range everSeen {
			if dev.k.library[id] == "" {
				t.Fatalf("%s: %s lost book %s", when, dev.name, id)
			}
		}
		for id := range dev.k.archived {
			if !selfDeleted[id] {
				t.Fatalf("%s: %s was told to archive book %s, which it never deleted",
					when, dev.name, id)
			}
		}
	}
}

func seriesOfBooks(prefix string, n int) []calibretest.BookSpec {
	out := make([]calibretest.BookSpec, 0, n)
	for i := range n {
		out = append(out, calibretest.BookSpec{
			Title:   fmt.Sprintf("%s %02d", prefix, i),
			Authors: []string{fmt.Sprintf("Author %c", 'A'+rune(i%5))},
			Cover:   i%3 == 0,
		})
	}
	return out
}

func addSource(t *testing.T, e *env, name, path string, priority int) int64 {
	t.Helper()
	id, err := store.CreateSource(e.ctx, e.store.Writer(), &store.Source{
		Name: name, LibraryPath: path, Priority: priority, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
