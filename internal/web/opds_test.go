package web_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

// atomFeed is a reader's view of the catalogue: only what a reading app parses.
type atomFeed struct {
	XMLName xml.Name `xml:"feed"`
	Title   string   `xml:"title"`
	ID      string   `xml:"id"`
	Total   int      `xml:"totalResults"`
	Links   []struct {
		Rel  string `xml:"rel,attr"`
		Href string `xml:"href,attr"`
		Type string `xml:"type,attr"`
	} `xml:"link"`
	Entries []struct {
		Title   string `xml:"title"`
		ID      string `xml:"id"`
		Authors []struct {
			Name string `xml:"name"`
		} `xml:"author"`
		Links []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
			Type string `xml:"type,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// opds fetches a catalogue URL with a username and password, the way a reading
// app does — it has no browser session and no way to fill in a login form.
func (e *env) opds(t *testing.T, path string) (int, atomFeed, string) {
	t.Helper()

	req, err := http.NewRequest("GET", e.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("admin", testPassword)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}

	// Only the catalogue feeds are parsed; the OpenSearch document is a
	// different root element and is checked as text.
	var feed atomFeed
	if resp.StatusCode == 200 && strings.Contains(body.String(), "<feed") {
		if err := xml.Unmarshal([]byte(body.String()), &feed); err != nil {
			t.Fatalf("%s did not parse as a feed: %v\n%s", path, err, body.String())
		}
	}
	return resp.StatusCode, feed, body.String()
}

// A reading app has no session, so the catalogue must not be reachable without
// credentials — and must ask for them rather than redirecting to a login page a
// reader cannot use.
func TestTheCatalogueAsksForCredentials(t *testing.T) {
	e := newEnv(t)

	resp, err := (&http.Client{}).Get(e.server.URL + "/opds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") {
		t.Errorf("WWW-Authenticate = %q, want a Basic challenge",
			resp.Header.Get("WWW-Authenticate"))
	}
}

// The root is a navigation feed pointing at the rest.
func TestTheCatalogueRootLeadsSomewhere(t *testing.T) {
	e := newEnv(t)

	status, feed, body := e.opds(t, "/opds")
	if status != 200 {
		t.Fatalf("status = %d:\n%s", status, body)
	}
	if len(feed.Entries) < 2 {
		t.Fatalf("%d entries, want at least all-books and recently-added", len(feed.Entries))
	}

	var hasSearch bool
	for _, l := range feed.Links {
		if l.Rel == "search" {
			hasSearch = true
		}
	}
	if !hasSearch {
		t.Error("the root does not advertise search, so no reader can find it")
	}
}

// Every book in the feed must be downloadable, and the link must actually work:
// a catalogue entry with a dead acquisition link is worse than no entry.
func TestEveryCatalogueEntryCanBeDownloaded(t *testing.T) {
	e := newEnv(t)

	status, feed, body := e.opds(t, "/opds/all")
	if status != 200 {
		t.Fatalf("status = %d:\n%s", status, body)
	}
	if len(feed.Entries) == 0 {
		t.Fatalf("the catalogue is empty:\n%s", body)
	}

	for _, entry := range feed.Entries {
		if !strings.HasPrefix(entry.ID, "urn:uuid:") {
			t.Errorf("%s has id %q, want a urn:uuid", entry.Title, entry.ID)
		}

		var acquisition string
		for _, l := range entry.Links {
			if l.Rel == "http://opds-spec.org/acquisition" && acquisition == "" {
				acquisition = l.Href
			}
		}
		if acquisition == "" {
			t.Errorf("%s has no acquisition link", entry.Title)
			continue
		}

		req, _ := http.NewRequest("GET", acquisition, nil)
		req.SetBasicAuth("admin", testPassword)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: downloading %s gave %d", entry.Title, acquisition, resp.StatusCode)
		}
	}
}

// Search has to narrow the feed, or the OpenSearch document is a lie.
func TestSearchingTheCatalogue(t *testing.T) {
	e := newEnv(t)

	_, all, _ := e.opds(t, "/opds/all")
	_, found, body := e.opds(t, "/opds/search?q=Readable")

	if len(found.Entries) == 0 {
		t.Fatalf("searching found nothing:\n%s", body)
	}
	if len(found.Entries) >= len(all.Entries) {
		t.Errorf("search returned %d of %d entries; it did not narrow anything",
			len(found.Entries), len(all.Entries))
	}
	for _, entry := range found.Entries {
		if !strings.Contains(entry.Title, "Readable") {
			t.Errorf("%q does not match the search", entry.Title)
		}
	}
}

// The OpenSearch document is what a reader fetches to learn how to search, and
// its template has to carry the placeholder.
func TestTheOpenSearchDocument(t *testing.T) {
	e := newEnv(t)

	status, _, body := e.opds(t, "/opds/opensearch.xml")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, "{searchTerms}") {
		t.Errorf("the search template has no placeholder:\n%s", body)
	}
}

// A book that cannot be handed over must not be listed. An entry whose download
// fails is what makes a reading app show an error instead of a library.
func TestTheCatalogueOnlyListsBooksItCanServe(t *testing.T) {
	e := newEnv(t)

	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET hidden = 1, syncable = 0 WHERE title = 'Readable One'`); err != nil {
		t.Fatal(err)
	}

	_, feed, body := e.opds(t, "/opds/all")
	for _, entry := range feed.Entries {
		if entry.Title == "Readable One" {
			t.Errorf("a hidden book is in the catalogue:\n%s", body)
		}
	}
}
