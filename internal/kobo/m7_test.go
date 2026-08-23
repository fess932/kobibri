package kobo_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/kobo"
	"github.com/fess932/kobibri/internal/store"
)

// A library larger than one batch must arrive across several requests, driven
// by the x-kobo-sync: continue header, and every book must arrive exactly once.
func TestPaginatedSyncDeliversEverythingExactlyOnce(t *testing.T) {
	specs := make([]calibretest.BookSpec, 0, 25)
	for i := range 25 {
		specs = append(specs, calibretest.BookSpec{Title: fmt.Sprintf("Book %02d", i)})
	}
	s := newSyncEnvBatched(t, 5, specs...)
	k := newFakeKobo(t, s.env)

	var requests int
	seen := map[string]int{}
	for range 50 {
		items, more := k.syncOnce()
		requests++
		for _, item := range items {
			for kind, payload := range item {
				if kind != "NewEntitlement" {
					continue
				}
				var c struct {
					BookEntitlement kobo.BookEntitlement `json:"BookEntitlement"`
				}
				if err := json.Unmarshal(payload, &c); err != nil {
					t.Fatal(err)
				}
				seen[c.BookEntitlement.ID]++
			}
		}
		if !more {
			break
		}
	}

	if requests < 5 {
		t.Errorf("25 books at a batch of 5 took %d requests; pagination did not engage", requests)
	}
	if len(seen) != 25 {
		t.Fatalf("received %d distinct books, want 25", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("book %s arrived %d times, want once", id, n)
		}
	}
	if len(k.library) != 25 {
		t.Errorf("device holds %d books, want 25", len(k.library))
	}
}

// A sync abandoned halfway must lose nothing: the next attempt starts over from
// the last completed snapshot, which is only deleted once its child completes.
func TestInterruptedSyncLosesNothing(t *testing.T) {
	specs := make([]calibretest.BookSpec, 0, 20)
	for i := range 20 {
		specs = append(specs, calibretest.BookSpec{Title: fmt.Sprintf("Book %02d", i)})
	}
	s := newSyncEnvBatched(t, 3, specs...)

	// The device gets partway through, then stops talking to us.
	interrupted := newFakeKobo(t, s.env)
	for range 2 {
		if _, more := interrupted.syncOnce(); !more {
			t.Fatal("the library was delivered in fewer requests than expected")
		}
	}
	partial := len(interrupted.library)
	if partial == 0 || partial >= 20 {
		t.Fatalf("interrupted device holds %d books; expected a partial library", partial)
	}

	// It comes back with no token at all, as a device that restarted would.
	resumed := newFakeKobo(t, s.env)
	resumed.sync()

	if len(resumed.library) != 20 {
		t.Errorf("after restarting mid-sync the device holds %d books, want all 20",
			len(resumed.library))
	}
}

// Resuming with the token we handed out must continue rather than start over.
func TestResumeContinuesFromTheCursor(t *testing.T) {
	specs := make([]calibretest.BookSpec, 0, 12)
	for i := range 12 {
		specs = append(specs, calibretest.BookSpec{Title: fmt.Sprintf("Book %02d", i)})
	}
	s := newSyncEnvBatched(t, 4, specs...)
	k := newFakeKobo(t, s.env)

	first, more := k.syncOnce()
	if !more {
		t.Fatal("12 books at a batch of 4 finished in one request")
	}
	if len(first) != 4 {
		t.Errorf("first response carried %d items, want 4", len(first))
	}

	tok := kobo.ParseSyncToken(k.token)
	if tok.Ongoing == "" {
		t.Error("a continuing sync did not name an ongoing snapshot in its token")
	}

	k.sync()
	if len(k.library) != 12 {
		t.Errorf("device holds %d books after resuming, want 12", len(k.library))
	}
}

// Reading progress must round-trip through the device-facing endpoints.
func TestReadingStateRoundTrip(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Read Me"})
	id := s.bookID("Read Me")

	body := `{"ReadingStates":[{
		"EntitlementId":"` + id + `",
		"LastModified":"2026-08-12T10:00:00Z",
		"StatusInfo":{"Status":"Reading","TimesStartedReading":1,"LastModified":"2026-08-12T10:00:00Z"},
		"Statistics":{"SpentReadingMinutes":42,"RemainingTimeMinutes":180,"LastModified":"2026-08-12T10:00:00Z"},
		"CurrentBookmark":{"ProgressPercent":37,"ContentSourceProgressPercent":61,
			"LastModified":"2026-08-12T10:00:00Z",
			"Location":{"Value":"kobo.12.3","Type":"KoboSpan","Source":"OEBPS/chapter05.xhtml"}}
	}]}`

	resp := s.do("PUT", s.kobo("/v1/library/"+id+"/state"), body)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
	}

	var put struct {
		RequestResult string `json:"RequestResult"`
		UpdateResults []struct {
			EntitlementID         string            `json:"EntitlementId"`
			CurrentBookmarkResult map[string]string `json:"CurrentBookmarkResult"`
			StatisticsResult      map[string]string `json:"StatisticsResult"`
			StatusInfoResult      map[string]string `json:"StatusInfoResult"`
		} `json:"UpdateResults"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&put); err != nil {
		t.Fatal(err)
	}
	if put.RequestResult != "Success" {
		t.Errorf("RequestResult = %q", put.RequestResult)
	}
	if len(put.UpdateResults) != 1 {
		t.Fatalf("got %d update results, want 1", len(put.UpdateResults))
	}
	r0 := put.UpdateResults[0]
	if r0.EntitlementID != id {
		t.Errorf("EntitlementId = %q, want %q", r0.EntitlementID, id)
	}
	for name, got := range map[string]map[string]string{
		"CurrentBookmarkResult": r0.CurrentBookmarkResult,
		"StatisticsResult":      r0.StatisticsResult,
		"StatusInfoResult":      r0.StatusInfoResult,
	} {
		if got["Result"] != "Success" {
			t.Errorf("%s.Result = %q, want Success", name, got["Result"])
		}
	}

	// Reading it back must return an array of exactly one state.
	getResp := s.do("GET", s.kobo("/v1/library/"+id+"/state"), "")
	if getResp.StatusCode != 200 {
		t.Fatalf("GET status = %d", getResp.StatusCode)
	}
	states := decode[[]kobo.ReadingState](t, getResp)
	if len(states) != 1 {
		t.Fatalf("got %d reading states, want exactly 1", len(states))
	}

	got := states[0]
	if got.EntitlementID != id {
		t.Errorf("EntitlementId = %q", got.EntitlementID)
	}
	if got.StatusInfo.Status != kobo.StatusReading {
		t.Errorf("Status = %q, want Reading", got.StatusInfo.Status)
	}
	if got.CurrentBookmark == nil {
		t.Fatal("the bookmark was not stored")
	}
	if got.CurrentBookmark.ProgressPercent != 37 {
		t.Errorf("ProgressPercent = %v, want 37", got.CurrentBookmark.ProgressPercent)
	}
	if got.CurrentBookmark.Location == nil || got.CurrentBookmark.Location.Value != "kobo.12.3" {
		t.Errorf("Location = %+v, want the kobo span preserved", got.CurrentBookmark.Location)
	}
	if got.CurrentBookmark.Location.Type != "KoboSpan" {
		t.Errorf("Location.Type = %q, want KoboSpan", got.CurrentBookmark.Location.Type)
	}
	if got.Statistics == nil || got.Statistics.SpentReadingMinutes != 42 {
		t.Errorf("Statistics = %+v", got.Statistics)
	}
}

// When a book is finished the device sends the *first* resource as the current
// position, not the last. Storing that verbatim would send the reader back to
// page one on their next device.
func TestFinishedBookDoesNotStoreTheBogusFirstLocation(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Finished"})
	id := s.bookID("Finished")

	body := `{"ReadingStates":[{
		"EntitlementId":"` + id + `",
		"LastModified":"2026-08-12T10:00:00Z",
		"StatusInfo":{"Status":"Finished","LastModified":"2026-08-12T10:00:00Z"},
		"CurrentBookmark":{"ProgressPercent":2,"ContentSourceProgressPercent":1,
			"LastModified":"2026-08-12T10:00:00Z",
			"Location":{"Value":"kobo.1.1","Type":"KoboSpan","Source":"OEBPS/chapter01.xhtml"}}
	}]}`

	if resp := s.do("PUT", s.kobo("/v1/library/"+id+"/state"), body); resp.StatusCode != 200 {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}

	states := decode[[]kobo.ReadingState](t, s.do("GET", s.kobo("/v1/library/"+id+"/state"), ""))
	if len(states) != 1 {
		t.Fatalf("got %d states", len(states))
	}
	got := states[0]

	if got.StatusInfo.Status != kobo.StatusFinished {
		t.Errorf("Status = %q, want Finished", got.StatusInfo.Status)
	}
	if got.CurrentBookmark == nil {
		t.Fatal("no bookmark stored")
	}
	if got.CurrentBookmark.ProgressPercent != 100 {
		t.Errorf("ProgressPercent = %v for a finished book, want 100",
			got.CurrentBookmark.ProgressPercent)
	}
	if got.CurrentBookmark.Location != nil && got.CurrentBookmark.Location.Value == "kobo.1.1" {
		t.Error("the device's bogus first-resource location was stored verbatim")
	}
}

// A reading state for a book we do not know must still answer in the shape the
// device expects.
func TestPutStateForUnknownBookIsWellFormed(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Known"})

	resp := s.do("PUT", s.kobo("/v1/library/00000000-0000-4000-8000-000000000000/state"),
		`{"ReadingStates":[{"StatusInfo":{"Status":"Reading"}}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decode[map[string]any](t, resp)
	if body["RequestResult"] != "Success" {
		t.Errorf("RequestResult = %v", body["RequestResult"])
	}
	if _, ok := body["UpdateResults"]; !ok {
		t.Error("response has no UpdateResults")
	}
}

// Deleting a book on the device must answer 204 and record a permanent,
// device-scoped tombstone.
func TestDeleteBookRecordsATombstone(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "Keep"},
		calibretest.BookSpec{Title: "Delete Me"},
	)
	k := newFakeKobo(t, s.env)
	k.sync()

	id := s.bookID("Delete Me")

	req, err := http.NewRequest("DELETE", s.server.URL+s.kobo("/v1/library/"+id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-kobo-deviceid", "device-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}

	devices, err := store.ListDevices(s.ctx, s.store.Reader(), s.userID)
	if err != nil || len(devices) == 0 {
		t.Fatalf("devices = %v, err = %v", devices, err)
	}
	tombstoned, err := store.HasTombstone(s.ctx, s.store.Reader(), devices[0].ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if !tombstoned {
		t.Fatal("no tombstone was recorded")
	}

	// The book stays in the library: deletion is a device-side fact.
	if _, err := store.GetBook(s.ctx, s.store.Reader(), id); err != nil {
		t.Errorf("the canonical book was removed from the library: %v", err)
	}

	// And it is never offered to this device again.
	k.sync()
	for range 3 {
		if items := k.sync(); len(items) != 0 {
			t.Fatalf("a deleted book produced %d further items: %v", len(items), kinds(items))
		}
	}
	if k.titles()["Delete Me"] {
		t.Error("the deleted book is still in the device library")
	}
	if !k.titles()["Keep"] {
		t.Error("deleting one book disturbed another")
	}
}

// Deleting on one device must leave the book on another.
func TestDeleteIsScopedToOneDevice(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Shared"})
	id := s.bookID("Shared")

	first := newFakeKoboAs(t, s.env, "device-one")
	second := newFakeKoboAs(t, s.env, "device-two")
	first.sync()
	second.sync()

	if len(first.library) != 1 || len(second.library) != 1 {
		t.Fatalf("devices hold %d and %d books, want 1 each", len(first.library), len(second.library))
	}

	req, _ := http.NewRequest("DELETE", s.server.URL+s.kobo("/v1/library/"+id), nil)
	req.Header.Set("x-kobo-deviceid", "device-one")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	first.sync()
	if first.titles()["Shared"] {
		t.Error("the book survived on the device that deleted it")
	}

	if items := second.sync(); len(items) != 0 {
		t.Errorf("the other device received %d items about a deletion it did not make: %v",
			len(items), kinds(items))
	}
	if !second.titles()["Shared"] {
		t.Error("deleting on one device removed the book from another")
	}
}

// The collections endpoint must not be swallowed by the book deletion route.
func TestDeleteTagsIsNotReadAsABookDeletion(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "One"})

	req, _ := http.NewRequest("DELETE", s.server.URL+s.kobo("/v1/library/tags"), nil)
	req.Header.Set("x-kobo-deviceid", "device-abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 so it cannot be mistaken for deleting a book named \"tags\"",
			resp.StatusCode)
	}

	// Nothing may have been tombstoned by that request.
	devices, err := store.ListDevices(s.ctx, s.store.Reader(), s.userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		ids, err := store.ListTombstones(s.ctx, s.store.Reader(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Errorf("DELETE /v1/library/tags recorded tombstones: %v", ids)
		}
	}
}

// Removing a tombstone must let the book be offered again, which is the escape
// hatch for an accidental deletion on the device.
func TestForgettingATombstoneRestoresTheBook(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Oops"})
	id := s.bookID("Oops")

	k := newFakeKobo(t, s.env)
	k.sync()

	devices, err := store.ListDevices(s.ctx, s.store.Reader(), s.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddTombstone(s.ctx, s.store.Writer(), devices[0].ID, id); err != nil {
		t.Fatal(err)
	}
	k.sync()
	if k.titles()["Oops"] {
		t.Fatal("the book was not removed from the device")
	}

	if err := store.RemoveTombstone(s.ctx, s.store.Writer(), devices[0].ID, id); err != nil {
		t.Fatal(err)
	}
	k.sync()
	if !k.titles()["Oops"] {
		t.Errorf("forgetting the tombstone did not bring the book back; device holds %v", k.titles())
	}
}

// newSyncEnvBatched builds a sync environment with a small batch size, so the
// continuation path is exercised without needing a huge fixture.
func newSyncEnvBatched(t *testing.T, batch int, books ...calibretest.BookSpec) *syncEnv {
	t.Helper()
	s := newSyncEnvWith(t, envOptions{SyncBatch: batch}, books...)
	return s
}

// newFakeKoboAs drives the conversation as a specific device id, so tests can
// run two Kobos against one token.
func newFakeKoboAs(t *testing.T, e *env, deviceID string) *fakeKobo {
	k := newFakeKobo(t, e)
	k.deviceID = deviceID
	return k
}

func init() {
	// Guard against a typo in the fixture titles used above, which would make
	// the pagination tests silently assert on an empty library.
	if !strings.HasPrefix(fmt.Sprintf("Book %02d", 1), "Book 01") {
		panic("unexpected fixture title format")
	}
}

// What a device reports has to reach the browser as a percentage. The number is
// buried in the bookmark the device sends, and the interface reads it from
// there — so this checks the whole way through rather than the endpoint alone.
func TestProgressReachesTheLibraryListing(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Read Me"})
	id := s.bookID("Read Me")

	body := `{"ReadingStates":[{
		"EntitlementId":"` + id + `",
		"LastModified":"2026-08-12T10:00:00Z",
		"StatusInfo":{"Status":"Reading","TimesStartedReading":1,"LastModified":"2026-08-12T10:00:00Z"},
		"CurrentBookmark":{"ProgressPercent":12,"ContentSourceProgressPercent":44,
			"LastModified":"2026-08-12T10:00:00Z",
			"Location":{"Value":"kobo.12.3","Type":"KoboSpan","Source":"OEBPS/ch05.xhtml"}}
	}]}`
	if resp := s.do("PUT", s.kobo("/v1/library/"+id+"/state"), body); resp.StatusCode != 200 {
		t.Fatalf("PUT status = %d", resp.StatusCode)
	}

	rows, _, err := store.ListLibrary(s.ctx, s.store.Reader(),
		store.LibraryQuery{ProgressFor: s.userID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, row := range rows {
		if row.ID != id {
			continue
		}
		found = true
		if !row.Progress.Started() {
			t.Error("the book shows as unstarted after the device reported progress")
		}
		// ProgressPercent is the whole book; ContentSourceProgressPercent is the
		// current spine file. This test asserted 44 until 2026-08-24, when a Kobo
		// Libra Colour 19 pages into a 760-page book sent ProgressPercent 2 and
		// ContentSourceProgressPercent 42 — 42% of one chapter file. The code and
		// the test held the same inverted belief, which is why neither caught it.
		if got := row.Progress.Rounded(); got != 12 {
			t.Errorf("progress = %d%%, want the whole-book 12%%", got)
		}
		if row.Progress.Status != store.ReadReading {
			t.Errorf("status = %q, want %q", row.Progress.Status, store.ReadReading)
		}
	}
	if !found {
		t.Fatal("the book is not in the listing at all")
	}
}

// A device's sync history is what answers "my book never arrived" — the only
// useful question then is what the server actually sent, and when.
func TestTheSyncHistoryRecordsWhatWasSent(t *testing.T) {
	s := newSyncEnv(t,
		calibretest.BookSpec{Title: "One"},
		calibretest.BookSpec{Title: "Two"},
	)

	k := newFakeKobo(t, s.env)
	k.sync()

	devices, err := store.ListAllDevices(s.ctx, s.store.Reader())
	if err != nil || len(devices) == 0 {
		t.Fatalf("no devices: %v", err)
	}

	runs, err := store.RecentSyncRuns(s.ctx, s.store.Reader(), devices[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d sync runs after one sync, want 1", len(runs))
	}
	if runs[0].NewBooks != 2 {
		t.Errorf("recorded %d new books, want 2", runs[0].NewBooks)
	}
	if runs[0].Status != "ok" || runs[0].FinishedAt == "" {
		t.Errorf("the run was not closed: %+v", runs[0])
	}

	// A sync that sent nothing leaves no trace: a reader checks in every few
	// minutes, and a history of empty entries is a history nobody can read.
	k.sync()
	k.sync()
	runs, err = store.RecentSyncRuns(s.ctx, s.store.Reader(), devices[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Errorf("%d entries after two quiet syncs, want the original 1", len(runs))
	}
}
