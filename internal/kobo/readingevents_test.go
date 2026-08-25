package kobo_test

import (
	"encoding/json"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/kobo"
)

type updateResults struct {
	RequestResult string `json:"RequestResult"`
	UpdateResults []struct {
		CurrentBookmarkResult map[string]string `json:"CurrentBookmarkResult"`
		StatisticsResult      map[string]string `json:"StatisticsResult"`
		StatusInfoResult      map[string]string `json:"StatusInfoResult"`
	} `json:"UpdateResults"`
}

func fullState(id string) string {
	return `{"ReadingStates":[{
		"EntitlementId":"` + id + `",
		"LastModified":"2026-08-25T18:33:17Z",
		"StatusInfo":{"Status":"Reading","TimesStartedReading":2,"LastModified":"2026-08-25T18:33:17Z"},
		"Statistics":{"SpentReadingMinutes":204,"RemainingTimeMinutes":4144,"LastModified":"2026-08-25T18:33:17Z"},
		"CurrentBookmark":{"ProgressPercent":3,"ContentSourceProgressPercent":0,
			"LastModified":"2026-08-25T18:33:17Z",
			"Location":{"Value":"kobo.1.1","Type":"KoboSpan","Source":"OEBPS/text/ch0023.xhtml"}}
	}]}`
}

// The three sections of a reading state are independently optional — that is
// why the response reports on each one separately. Writing null over a bookmark
// because a status-only update arrived loses the reader's place in the book.
func TestAStatusOnlyUpdateKeepsThePosition(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Read Me"})
	id := s.bookID("Read Me")

	if r := s.do("PUT", s.kobo("/v1/library/"+id+"/state"), fullState(id)); r.StatusCode != 200 {
		t.Fatalf("PUT status = %d", r.StatusCode)
	}

	resp := s.do("PUT", s.kobo("/v1/library/"+id+"/state"),
		`{"ReadingStates":[{"EntitlementId":"`+id+`","LastModified":"2026-08-25T19:00:00Z",
		  "StatusInfo":{"Status":"Reading","LastModified":"2026-08-25T19:00:00Z"}}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("partial PUT status = %d", resp.StatusCode)
	}

	var got updateResults
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.UpdateResults) != 1 {
		t.Fatalf("got %d update results, want 1", len(got.UpdateResults))
	}
	// A section that never arrived cannot have succeeded.
	if r := got.UpdateResults[0].CurrentBookmarkResult["Result"]; r != "Ignored" {
		t.Errorf("CurrentBookmarkResult = %q, want Ignored", r)
	}
	if r := got.UpdateResults[0].StatusInfoResult["Result"]; r != "Success" {
		t.Errorf("StatusInfoResult = %q, want Success", r)
	}

	states := decode[[]kobo.ReadingState](t, s.do("GET", s.kobo("/v1/library/"+id+"/state"), ""))
	if len(states) != 1 {
		t.Fatalf("got %d reading states, want 1", len(states))
	}
	if states[0].CurrentBookmark == nil {
		t.Fatal("a status-only update wiped the stored position")
	}
	if states[0].CurrentBookmark.Location == nil ||
		states[0].CurrentBookmark.Location.Value != "kobo.1.1" {
		t.Errorf("location = %+v, want the span kept", states[0].CurrentBookmark.Location)
	}
	if states[0].Statistics == nil || states[0].Statistics.SpentReadingMinutes != 204 {
		t.Errorf("statistics = %+v, want the minutes kept", states[0].Statistics)
	}
}

// The device sends these two on some firmwares and not others. Parsing them and
// dropping them made every answer say the book had been started zero times.
func TestTimesStartedReadingSurvives(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Read Me"})
	id := s.bookID("Read Me")

	if r := s.do("PUT", s.kobo("/v1/library/"+id+"/state"), fullState(id)); r.StatusCode != 200 {
		t.Fatalf("PUT status = %d", r.StatusCode)
	}

	states := decode[[]kobo.ReadingState](t, s.do("GET", s.kobo("/v1/library/"+id+"/state"), ""))
	if len(states) != 1 || states[0].StatusInfo.TimesStartedReading != 2 {
		t.Errorf("TimesStartedReading = %d, want 2", states[0].StatusInfo.TimesStartedReading)
	}
}

// Progress reports are the raw material of every reading statistic, and each
// one used to be thrown away as the next arrived.
func TestProgressReportsAreKept(t *testing.T) {
	s := newSyncEnv(t, calibretest.BookSpec{Title: "Read Me"})
	id := s.bookID("Read Me")

	for _, body := range []string{
		fullState(id),
		`{"ReadingStates":[{"EntitlementId":"` + id + `","LastModified":"2026-08-25T19:00:54Z",
		  "StatusInfo":{"Status":"Reading","LastModified":"2026-08-25T19:00:54Z"},
		  "Statistics":{"SpentReadingMinutes":217,"RemainingTimeMinutes":4135,"LastModified":"2026-08-25T19:00:54Z"},
		  "CurrentBookmark":{"ProgressPercent":3,"ContentSourceProgressPercent":32,
		    "LastModified":"2026-08-25T19:00:54Z",
		    "Location":{"Value":"kobo.51.1","Type":"KoboSpan","Source":"OEBPS/text/ch0024.xhtml"}}}]}`,
	} {
		if r := s.do("PUT", s.kobo("/v1/library/"+id+"/state"), body); r.StatusCode != 200 {
			t.Fatalf("PUT status = %d", r.StatusCode)
		}
	}

	rows, err := s.env.store.Reader().QueryContext(t.Context(),
		`SELECT source, block, spent_minutes, spent_delta FROM reading_events
		  WHERE book_id = ? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type event struct {
		source string
		block  int
		spent  int
		delta  int
	}
	var events []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.source, &e.block, &e.spent, &e.delta); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 2 {
		t.Fatalf("stored %d events, want 2", len(events))
	}
	if events[0].block != 1 || events[1].block != 51 {
		t.Errorf("blocks = %d, %d — want the koboSpan block kept", events[0].block, events[1].block)
	}
	if events[1].source != "OEBPS/text/ch0024.xhtml" {
		t.Errorf("source = %q", events[1].source)
	}
	// 217 minus 204: what the device counted between the two reports, which is
	// not the time between them.
	if events[1].delta != 13 {
		t.Errorf("spent delta = %d, want 13", events[1].delta)
	}
}
