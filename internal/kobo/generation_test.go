package kobo_test

import (
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/store"
)

// A sync that finds nothing changed answers without building a snapshot. That
// makes the quiet case cheap, and it makes a new kind of fault possible: a change
// the fingerprint does not notice would leave a device silently stale forever,
// which is worse than any amount of slowness.
//
// So every kind of change gets its own case here. If someone adds a way for the
// library to move, this is where it has to be taught about — and the test that
// will fail if it is not.
func TestEveryChangeIsNoticed(t *testing.T) {
	type change struct {
		name string
		// apply makes the change and returns what the device must end up seeing.
		apply func(t *testing.T, h *genHarness) func(t *testing.T, k *fakeKobo)
	}

	changes := []change{
		{"a new book", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			h.lib.Add(calibretest.BookSpec{Title: "Newcomer"})
			h.scan()
			return func(t *testing.T, k *fakeKobo) {
				if !k.titles()["Newcomer"] {
					t.Error("a book added to the library never reached the device")
				}
			}
		}},

		{"an edited title", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			h.lib.Exec(`UPDATE books SET title = 'Edited',
				last_modified = '2032-01-01 00:00:00.000000+00:00' WHERE id = 1`)
			h.scan()
			return func(t *testing.T, k *fakeKobo) {
				if !k.titles()["Edited"] {
					t.Error("an edited title never reached the device")
				}
			}
		}},

		{"a book hidden", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			id := h.bookID("Book 00")
			if err := store.SetBookHidden(h.env.ctx, h.env.store.Writer(), id, true); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T, k *fakeKobo) {
				if k.library[id] != "" {
					t.Error("a hidden book was not retracted from the device")
				}
			}
		}},

		{"a collection created", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			if _, err := store.CreateTag(h.env.ctx, h.env.store.Writer(),
				h.env.userID, "Bedtime", store.TagOriginServer); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T, k *fakeKobo) {
				if !k.tags["Bedtime"] {
					t.Error("a new collection never reached the device")
				}
			}
		}},

		{"progress from another device", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			id := h.bookID("Book 01")
			other := newFakeKoboAs(t, h.env, "the-other-one")
			other.sync()
			body := `{"ReadingStates":[{"EntitlementId":"` + id + `",
				"LastModified":"2032-02-02T10:00:00Z",
				"StatusInfo":{"Status":"Reading","LastModified":"2032-02-02T10:00:00Z"},
				"CurrentBookmark":{"ProgressPercent":50,"ContentSourceProgressPercent":50,
					"LastModified":"2032-02-02T10:00:00Z"}}]}`
			if resp := h.env.doAsDevice(other.deviceID, "PUT",
				h.env.kobo("/v1/library/"+id+"/state"), body); resp.StatusCode != 200 {
				t.Fatalf("PUT state: %d", resp.StatusCode)
			}
			return func(t *testing.T, k *fakeKobo) {
				if !k.readingStates[id] {
					t.Error("progress reported on another device never reached this one")
				}
			}
		}},

		{"a library shared with this person", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			// A second library that this person cannot see, and then can. Carry
			// forward means the first library's books never leave, so the test
			// has to turn on books the device does not already hold — otherwise
			// it could not fail.
			other, err := store.CreateUser(h.env.ctx, h.env.store.Writer(), "someone-else", "x", false)
			if err != nil {
				t.Fatal(err)
			}

			private := calibretest.New(t, calibretest.BookSpec{Title: "Someone Else's Book"})
			privateID := addSource(t, h.env, "private", private.Path, 20)
			if err := store.SetSourceSharing(h.env.ctx, h.env.store.Writer(),
				privateID, false, []int64{other}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.scanner.Scan(h.env.ctx, privateID,
				ingest.ScanOptions{Force: true}); err != nil {
				t.Fatal(err)
			}

			return func(t *testing.T, k *fakeKobo) {
				if k.titles()["Someone Else's Book"] {
					t.Fatal("a restricted library reached someone it was not shared with")
				}

				if err := store.SetSourceSharing(h.env.ctx, h.env.store.Writer(),
					privateID, false, []int64{other, h.env.userID}); err != nil {
					t.Fatal(err)
				}
				k.sync()
				if !k.titles()["Someone Else's Book"] {
					t.Error("a library shared with this person never reached their device")
				}
			}
		}},

		{"a source switched off", func(t *testing.T, h *genHarness) func(*testing.T, *fakeKobo) {
			if err := h.scanner.SetSourceEnabled(h.env.ctx, h.sourceID, false); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T, k *fakeKobo) {
				// Nothing is retracted — that is the whole design — but the sync
				// must not be skipped on the assumption that nothing moved.
				if len(k.library) == 0 {
					t.Error("switching a source off emptied the device")
				}
			}
		}},
	}

	for _, c := range changes {
		t.Run(c.name, func(t *testing.T) {
			h := newGenHarness(t)

			device := newFakeKoboAs(t, h.env, "device-1")
			device.sync()
			before := len(device.library)
			if before == 0 {
				t.Fatal("the device got nothing on its first sync")
			}

			// A second sync must be the cheap one, or this test proves nothing.
			if items := device.sync(); len(items) != 0 {
				t.Fatalf("a settled library still had %d items to send", len(items))
			}

			expect := c.apply(t, h)
			device.sync()
			expect(t, device)
		})
	}
}

type genHarness struct {
	env     *env
	lib     *calibretest.Library
	scanner *ingest.Scanner
	// sourceID is the library being scanned.
	sourceID int64
}

func newGenHarness(t *testing.T) *genHarness {
	e := newEnvWith(t, envOptions{SyncBatch: 50})
	lib := calibretest.New(t, seriesOfBooks("Book", 6)...)
	scanner := ingest.NewScanner(e.store, filepath.Join(t.TempDir(), "tmp"))
	id := addSource(t, e, "main", lib.Path, 10)

	h := &genHarness{env: e, lib: lib, scanner: scanner, sourceID: id}
	h.scan()
	return h
}

func (h *genHarness) scan() {
	h.env.t.Helper()
	if _, err := h.scanner.Scan(h.env.ctx, h.sourceID,
		ingest.ScanOptions{Force: true, ConfirmVanish: true}); err != nil {
		h.env.t.Fatalf("scan: %v", err)
	}
}

func (h *genHarness) bookID(title string) string {
	h.env.t.Helper()
	var id string
	if err := h.env.store.Reader().QueryRowContext(h.env.ctx,
		`SELECT id FROM books WHERE title = ?`, title).Scan(&id); err != nil {
		h.env.t.Fatalf("no book %q: %v", title, err)
	}
	return id
}
