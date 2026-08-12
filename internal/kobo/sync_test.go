package kobo_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/kobo"
	"github.com/fess932/kobibri/internal/store"
)

// fakeKobo replays a device's side of the sync conversation and keeps the
// library the device would end up holding, so tests can assert on the outcome
// rather than on individual responses.
type fakeKobo struct {
	t     *testing.T
	env   *env
	token string

	// library is what the device holds: book id -> title.
	library map[string]string
	// archived records books the server told it to remove.
	archived map[string]bool
}

func newFakeKobo(t *testing.T, e *env) *fakeKobo {
	return &fakeKobo{t: t, env: e, library: map[string]string{}, archived: map[string]bool{}}
}

// syncOnce performs a single sync request and applies the result.
func (k *fakeKobo) syncOnce() (items []map[string]json.RawMessage, more bool) {
	k.t.Helper()

	req, err := http.NewRequest("GET", k.env.server.URL+k.env.kobo("/v1/library/sync"), nil)
	if err != nil {
		k.t.Fatal(err)
	}
	req.Header.Set("x-kobo-deviceid", "device-abc")
	if k.token != "" {
		req.Header.Set("x-kobo-synctoken", k.token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		k.t.Fatalf("sync status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		k.t.Errorf("sync Content-Type = %q", ct)
	}

	next := resp.Header.Get("x-kobo-synctoken")
	if next == "" {
		k.t.Error("sync response carried no x-kobo-synctoken")
	}
	k.token = next
	more = resp.Header.Get("x-kobo-sync") == "continue"

	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		k.t.Fatalf("decoding sync array: %v", err)
	}
	k.apply(items)
	return items, more
}

// sync drains until the server stops asking for more.
func (k *fakeKobo) sync() []map[string]json.RawMessage {
	k.t.Helper()
	var all []map[string]json.RawMessage
	for range 50 {
		items, more := k.syncOnce()
		all = append(all, items...)
		if !more {
			return all
		}
	}
	k.t.Fatal("sync did not finish within 50 requests")
	return nil
}

// apply mutates the device library the way real firmware would.
func (k *fakeKobo) apply(items []map[string]json.RawMessage) {
	k.t.Helper()
	for _, item := range items {
		for kind, payload := range item {
			switch kind {
			case "NewEntitlement", "ChangedEntitlement":
				var c struct {
					BookEntitlement kobo.BookEntitlement `json:"BookEntitlement"`
					BookMetadata    kobo.BookMetadata    `json:"BookMetadata"`
				}
				if err := json.Unmarshal(payload, &c); err != nil {
					k.t.Fatalf("decoding %s: %v", kind, err)
				}
				id := c.BookEntitlement.ID
				if c.BookEntitlement.IsRemoved {
					delete(k.library, id)
					k.archived[id] = true
					continue
				}
				k.library[id] = c.BookMetadata.Title
				delete(k.archived, id)
			}
		}
	}
}

func (k *fakeKobo) titles() map[string]bool {
	out := map[string]bool{}
	for _, title := range k.library {
		out[title] = true
	}
	return out
}

// kinds counts the item kinds in a response.
func kinds(items []map[string]json.RawMessage) map[string]int {
	out := map[string]int{}
	for _, item := range items {
		for kind := range item {
			out[kind]++
		}
	}
	return out
}

// syncEnv is an env with a Calibre source wired up behind it.
type syncEnv struct {
	*env
	lib      *calibretest.Library
	sourceID int64
	scanner  *ingest.Scanner
}

func newSyncEnv(t *testing.T, books ...calibretest.BookSpec) *syncEnv {
	t.Helper()
	e := newEnv(t, "")

	lib := calibretest.New(t, books...)
	scanner := ingest.NewScanner(e.store, filepath.Join(t.TempDir(), "tmp"))

	sourceID, err := store.CreateSource(e.ctx, e.store.Writer(), &store.Source{
		Name: "main", LibraryPath: lib.Path, Priority: 100, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	se := &syncEnv{env: e, lib: lib, sourceID: sourceID, scanner: scanner}
	se.ingest()
	return se
}

func (s *syncEnv) ingest() {
	s.t.Helper()
	if _, err := s.scanner.Scan(s.ctx, s.sourceID, ingest.ScanOptions{Force: true}); err != nil {
		s.t.Fatalf("scan: %v", err)
	}
}

func (s *syncEnv) bookID(title string) string {
	s.t.Helper()
	var id string
	err := s.store.Reader().QueryRowContext(s.ctx,
		`SELECT id FROM books WHERE title = ? AND merged_into IS NULL`, title).Scan(&id)
	if err != nil {
		s.t.Fatalf("no canonical book titled %q: %v", title, err)
	}
	return id
}

func TestFirstSyncDeliversTheLibrary(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "One", Authors: []string{"Jane Author"}, Cover: true},
		calibretest.BookSpec{Title: "Two", Authors: []string{"Kim Second"},
			Series: "A Series", SeriesIndex: 2, Publisher: "Some Press"},
	)
	k := newFakeKobo(t, s.env)

	items := k.sync()
	if got := kinds(items); got["NewEntitlement"] != 2 {
		t.Fatalf("item kinds = %v, want 2 NewEntitlement", got)
	}
	if len(k.library) != 2 {
		t.Fatalf("device holds %d books, want 2", len(k.library))
	}
	for _, want := range []string{"One", "Two"} {
		if !k.titles()[want] {
			t.Errorf("device is missing %q; it holds %v", want, k.titles())
		}
	}
}

// A second sync with nothing changed must say nothing at all.
func TestSecondSyncIsEmpty(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})
	k := newFakeKobo(t, s.env)

	k.sync()
	items := k.sync()
	if len(items) != 0 {
		t.Errorf("second sync returned %d items, want none: %v", len(items), kinds(items))
	}
}

// The headline requirement: a book that disappears from every source stays on
// the device, and the sync that follows is silent about it.
func TestVanishedBookStaysOnDevice(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "Stays"},
		calibretest.BookSpec{Title: "Vanishes"},
	)
	k := newFakeKobo(t, s.env)
	k.sync()

	if len(k.library) != 2 {
		t.Fatalf("device holds %d books before the removal, want 2", len(k.library))
	}
	before := len(k.library)

	// The book is deleted from Calibre entirely.
	s.lib.Remove(2)
	s.ingest()

	items := k.sync()
	if len(items) != 0 {
		t.Errorf("sync sent %d items about a book that vanished server-side: %v",
			len(items), kinds(items))
	}
	if len(k.library) != before {
		t.Errorf("device library changed from %d to %d books", before, len(k.library))
	}
	if !k.titles()["Vanishes"] {
		t.Error("the vanished book was removed from the device; it must be left alone")
	}
	if len(k.archived) != 0 {
		t.Errorf("device archived %d books, want none", len(k.archived))
	}
}

// The same must hold when an entire source goes away, which is what an
// unmounted share looks like once an operator disables it.
func TestDisablingASourceLeavesTheDeviceUntouched(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)
	k := newFakeKobo(t, s.env)
	k.sync()
	before := k.titles()

	if err := s.scanner.SetSourceEnabled(s.ctx, s.sourceID, false); err != nil {
		t.Fatal(err)
	}

	items := k.sync()
	if len(items) != 0 {
		t.Errorf("sync sent %d items after the source was disabled: %v", len(items), kinds(items))
	}
	if len(k.titles()) != len(before) {
		t.Errorf("device library changed: %v -> %v", before, k.titles())
	}
}

// A changed book must not be announced as a ChangedEntitlement carrying a
// nested ReadingState: the device ignores that. It gets a NewEntitlement plus
// separate metadata and reading-state items.
func TestChangedBookUsesTheThreeItemShape(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Before"})
	k := newFakeKobo(t, s.env)
	k.sync()

	s.lib.Exec(`UPDATE books SET title = 'After', last_modified = '2030-01-01 00:00:00.000000+00:00' WHERE id = 1`)
	s.ingest()

	items := k.sync()
	got := kinds(items)
	if got["NewEntitlement"] != 1 || got["ChangedProductMetadata"] != 1 || got["ChangedReadingState"] != 1 {
		t.Errorf("item kinds = %v, want one each of NewEntitlement, ChangedProductMetadata, ChangedReadingState", got)
	}
	if got["ChangedEntitlement"] != 0 {
		t.Errorf("a changed book was sent as ChangedEntitlement, which the device mishandles")
	}
	if !k.titles()["After"] {
		t.Errorf("device did not pick up the new title; it holds %v", k.titles())
	}
}

// Hiding a book is how an operator pushes it to the device's Archive, and that
// is the one case where a removal is sent.
func TestHidingABookRetractsIt(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "Keep"},
		calibretest.BookSpec{Title: "Hide"},
	)
	k := newFakeKobo(t, s.env)
	k.sync()

	hidden := s.bookID("Hide")
	if err := store.SetBookHidden(s.ctx, s.store.Writer(), hidden, true); err != nil {
		t.Fatal(err)
	}

	items := k.sync()
	if got := kinds(items); got["ChangedEntitlement"] != 1 {
		t.Fatalf("item kinds = %v, want one ChangedEntitlement", got)
	}
	if !k.archived[hidden] {
		t.Error("the hidden book was not archived on the device")
	}
	if k.titles()["Hide"] {
		t.Error("the hidden book is still in the device library")
	}
	if !k.titles()["Keep"] {
		t.Error("hiding one book disturbed another")
	}
}

// A tombstoned book must never be offered again, not even by a full resync.
func TestTombstonedBookIsNeverOfferedAgain(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "Keep"},
		calibretest.BookSpec{Title: "Deleted"},
	)
	k := newFakeKobo(t, s.env)
	k.sync()

	deleted := s.bookID("Deleted")
	devices, err := store.ListDevices(s.ctx, s.store.Reader(), s.userID)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices = %v, err = %v", devices, err)
	}
	if err := store.AddTombstone(s.ctx, s.store.Writer(), devices[0].ID, deleted); err != nil {
		t.Fatal(err)
	}

	// The next sync retracts it, and later syncs stay silent.
	k.sync()
	for range 3 {
		if items := k.sync(); len(items) != 0 {
			t.Fatalf("a tombstoned book produced %d items: %v", len(items), kinds(items))
		}
	}

	// Even a full resync, with all sync state dropped, must not resurrect it.
	if err := store.ResetDeviceSyncState(s.ctx, s.store.Writer(), devices[0].ID); err != nil {
		t.Fatal(err)
	}
	k.token = ""
	k.library = map[string]string{}

	k.sync()
	if k.titles()["Deleted"] {
		t.Error("a full resync resurrected a book the device had deleted")
	}
	if !k.titles()["Keep"] {
		t.Error("the full resync did not deliver the remaining book")
	}
}

// Reading progress written by another device must reach this one; progress this
// device reported must not be echoed back at it.
func TestReadingStateEchoIsSuppressed(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Read Me"})
	k := newFakeKobo(t, s.env)
	k.sync()

	devices, err := store.ListDevices(s.ctx, s.store.Reader(), s.userID)
	if err != nil {
		t.Fatal(err)
	}
	bookID := s.bookID("Read Me")

	// Progress reported by this very device.
	mustExec(t, s.env, `INSERT INTO reading_states
		(user_id, book_id, status, bookmark_json, statistics_json, rev, last_writer_device_id, last_modified)
		VALUES (?,?,?,'null','null',1,?,?)`,
		s.userID, bookID, kobo.StatusReading, devices[0].ID, store.Now())

	if items := k.sync(); kinds(items)["ChangedReadingState"] != 0 {
		t.Errorf("our own reading progress was echoed back: %v", kinds(items))
	}

	// Progress from somewhere else must arrive.
	mustExec(t, s.env, `UPDATE reading_states SET rev = 2, last_writer_device_id = 9999,
		last_modified = ? WHERE book_id = ?`, store.Now(), bookID)

	if items := k.sync(); kinds(items)["ChangedReadingState"] != 1 {
		t.Errorf("reading progress from another device did not arrive: %v", kinds(items))
	}
}

// A book with no servable file must not reach the device at all.
func TestUnsyncableBooksAreNotSent(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "Epub Book"},
		calibretest.BookSpec{Title: "Pdf Only",
			Formats: []calibretest.FormatSpec{{Format: "PDF"}}},
	)
	k := newFakeKobo(t, s.env)
	k.sync()

	if k.titles()["Pdf Only"] {
		t.Error("a PDF-only book was sent; Kobo cannot sync those")
	}
	if !k.titles()["Epub Book"] {
		t.Error("the EPUB book was not sent")
	}
}

// The entitlement and metadata a device receives must carry the exact fields
// the protocol calls for.
func TestEntitlementShape(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{
		Title: "Shaped", Authors: []string{"Jane Author"},
		Series: "A Series", SeriesIndex: 3, Publisher: "Some Press",
		Languages: []string{"eng"}, Identifiers: map[string]string{"isbn": "9780306406157"},
		Cover: true,
	})
	k := newFakeKobo(t, s.env)

	items := k.sync()
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	payload, ok := items[0]["NewEntitlement"]
	if !ok {
		t.Fatalf("item is not a NewEntitlement: %v", items[0])
	}

	var c struct {
		BookEntitlement map[string]any `json:"BookEntitlement"`
		BookMetadata    map[string]any `json:"BookMetadata"`
		ReadingState    map[string]any `json:"ReadingState"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatal(err)
	}

	ent := c.BookEntitlement
	for k, want := range map[string]any{
		"Accessibility":       "Full",
		"Status":              "Active",
		"OriginCategory":      "Imported",
		"IsRemoved":           false,
		"IsHiddenFromArchive": false,
		"IsLocked":            false,
	} {
		if ent[k] != want {
			t.Errorf("BookEntitlement.%s = %v, want %v", k, ent[k], want)
		}
	}
	for _, field := range []string{"Id", "RevisionId", "CrossRevisionId", "Created", "LastModified", "ActivePeriod"} {
		if ent[field] == nil {
			t.Errorf("BookEntitlement.%s is missing", field)
		}
	}
	// Every id field carries the same canonical uuid.
	if ent["Id"] != ent["RevisionId"] || ent["Id"] != ent["CrossRevisionId"] {
		t.Errorf("id fields disagree: Id=%v RevisionId=%v CrossRevisionId=%v",
			ent["Id"], ent["RevisionId"], ent["CrossRevisionId"])
	}

	meta := c.BookMetadata
	if meta["Title"] != "Shaped" {
		t.Errorf("Title = %v", meta["Title"])
	}
	if meta["EntitlementId"] != ent["Id"] || meta["WorkId"] != ent["Id"] {
		t.Error("metadata ids do not match the entitlement id")
	}
	if meta["Language"] != "eng" {
		t.Errorf("Language = %v", meta["Language"])
	}
	if meta["ISBN"] != "9780306406157" {
		t.Errorf("ISBN = %v", meta["ISBN"])
	}

	series, ok := meta["Series"].(map[string]any)
	if !ok {
		t.Fatalf("Series is missing or malformed: %v", meta["Series"])
	}
	if series["Name"] != "A Series" || series["Number"] != 3.0 || series["NumberFloat"] != 3.0 {
		t.Errorf("Series = %v", series)
	}
	if series["Id"] != ingest.SeriesUUID("A Series") {
		t.Errorf("Series.Id = %v, want the uuid3 of the name", series["Id"])
	}

	// Exactly one download url, in the format the device should fetch.
	urls, ok := meta["DownloadUrls"].([]any)
	if !ok || len(urls) != 1 {
		t.Fatalf("DownloadUrls = %v, want exactly one entry", meta["DownloadUrls"])
	}
	u := urls[0].(map[string]any)
	if u["Format"] != "KEPUB" {
		t.Errorf("Format = %v, want KEPUB", u["Format"])
	}
	if u["Platform"] != "Generic" || u["DrmType"] != "None" {
		t.Errorf("Platform = %v, DrmType = %v", u["Platform"], u["DrmType"])
	}
	if u["Url"] == "" {
		t.Error("DownloadUrls entry has no Url")
	}

	if c.ReadingState == nil {
		t.Error("a new entitlement carried no ReadingState")
	}
}

// /v1/library/{uuid}/metadata answers with an array of exactly one object.
func TestMetadataEndpointReturnsAnArrayOfOne(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Single"})
	id := s.bookID("Single")

	resp := s.do("GET", s.kobo("/v1/library/"+id+"/metadata"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	items := decode[[]map[string]any](t, resp)
	if len(items) != 1 {
		t.Fatalf("got %d metadata objects, want exactly 1", len(items))
	}
	if items[0]["Title"] != "Single" {
		t.Errorf("Title = %v", items[0]["Title"])
	}
}

// An unknown book must not produce an error the device would choke on.
func TestMetadataForUnknownBookIsAnEmptyArray(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Known"})

	resp := s.do("GET", s.kobo("/v1/library/00000000-0000-4000-8000-000000000000/metadata"), "")
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if items := decode[[]map[string]any](t, resp); len(items) != 0 {
		t.Errorf("got %d items, want an empty array", len(items))
	}
}

// The sync response is always an array, never null.
func TestEmptySyncIsAnArrayNotNull(t *testing.T) {
	s := newSyncEnv(t)
	k := newFakeKobo(t, s.env)

	req, _ := http.NewRequest("GET", s.server.URL+s.kobo("/v1/library/sync"), nil)
	req.Header.Set("x-kobo-deviceid", "device-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Errorf("empty sync body = %s, want []", raw)
	}
	_ = k
}

// A device arriving with the real Kobo store's token must not be confused by
// it: we start from our own state and keep theirs for the proxy.
func TestForeignSyncTokenIsToleratedAndPreserved(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})
	k := newFakeKobo(t, s.env)
	k.token = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJrb2JvIn0"

	items := k.sync()
	if kinds(items)["NewEntitlement"] != 1 {
		t.Fatalf("a foreign token derailed the first sync: %v", kinds(items))
	}
	parsed := kobo.ParseSyncToken(k.token)
	if parsed.Last == "" {
		t.Error("our own token was not issued after a foreign one")
	}
}

func TestSyncTokenRoundTrip(t *testing.T) {
	original := kobo.SyncToken{Version: 1, Ongoing: "o-1", Last: "l-1", Raw: "store.token"}
	parsed := kobo.ParseSyncToken(original.String())

	if parsed != original {
		t.Errorf("round trip lost data: %+v -> %+v", original, parsed)
	}

	// The real store's token is of the form base64.base64 and carries no prefix
	// of ours; it must be preserved verbatim rather than parsed.
	foreign := kobo.ParseSyncToken("abc.def")
	if foreign.Raw != "abc.def" {
		t.Errorf("foreign token Raw = %q, want it kept verbatim", foreign.Raw)
	}
	if foreign.Ongoing != "" || foreign.Last != "" {
		t.Errorf("foreign token produced references to our snapshots: %+v", foreign)
	}

	// Garbage must degrade to a fresh token, not an error.
	if got := kobo.ParseSyncToken("KOBIBRI.!!!not-base64!!!"); got.Ongoing != "" || got.Last != "" {
		t.Errorf("garbage token = %+v, want empty", got)
	}
}

func mustExec(t *testing.T, e *env, query string, args ...any) {
	t.Helper()
	if _, err := e.store.Writer().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
