package web

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fess932/kobibri/internal/store"
)

// OPDS is a catalogue feed: the standard way for a reading app to browse a
// library and download from it. It exists because a Kobo is not the only thing
// anyone reads on — KOReader, Foliate, Moon+ and the rest all speak it, and
// without it the books here are reachable only from a browser.
//
// Version 1.2 (Atom) rather than 2.0 (JSON): every reader supports it, and the
// newer one is still not universal.
const (
	opdsNav         = "application/atom+xml;profile=opds-catalog;kind=navigation"
	opdsAcquisition = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	opdsPageSize    = 50
)

type feed struct {
	XMLName      xml.Name `xml:"feed"`
	Xmlns        string   `xml:"xmlns,attr"`
	XmlnsDC      string   `xml:"xmlns:dc,attr"`
	XmlnsOPDS    string   `xml:"xmlns:opds,attr"`
	XmlnsSearch  string   `xml:"xmlns:opensearch,attr"`
	ID           string   `xml:"id"`
	Title        string   `xml:"title"`
	Updated      string   `xml:"updated"`
	Author       author   `xml:"author"`
	TotalResults int      `xml:"opensearch:totalResults,omitempty"`
	ItemsPerPage int      `xml:"opensearch:itemsPerPage,omitempty"`
	StartIndex   int      `xml:"opensearch:startIndex,omitempty"`
	Links        []link   `xml:"link"`
	Entries      []entry  `xml:"entry"`
}

type author struct {
	Name string `xml:"name"`
	URI  string `xml:"uri,omitempty"`
}

type link struct {
	Rel   string `xml:"rel,attr,omitempty"`
	Href  string `xml:"href,attr"`
	Type  string `xml:"type,attr,omitempty"`
	Title string `xml:"title,attr,omitempty"`
	Count int    `xml:"opds:count,attr,omitempty"`
}

type entry struct {
	Title    string   `xml:"title"`
	ID       string   `xml:"id"`
	Updated  string   `xml:"updated"`
	Authors  []author `xml:"author,omitempty"`
	Language string   `xml:"dc:language,omitempty"`
	Series   string   `xml:"dc:source,omitempty"`
	Content  *text    `xml:"content,omitempty"`
	Links    []link   `xml:"link"`
}

type text struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

// mountOPDS registers the catalogue. It has its own authentication: a reading
// app has no browser session and no way to fill in a login form, so this is the
// one part of the interface that answers to HTTP Basic.
func (s *Server) mountOPDS(mux *http.ServeMux) {
	mux.HandleFunc("GET /opds", s.requireBasicAuth(s.handleOPDSRoot))
	mux.HandleFunc("GET /opds/{$}", s.requireBasicAuth(s.handleOPDSRoot))
	mux.HandleFunc("GET /opds/all", s.requireBasicAuth(s.handleOPDSAll))
	mux.HandleFunc("GET /opds/new", s.requireBasicAuth(s.handleOPDSNew))
	mux.HandleFunc("GET /opds/search", s.requireBasicAuth(s.handleOPDSSearch))
	mux.HandleFunc("GET /opds/opensearch.xml", s.requireBasicAuth(s.handleOPDSDescription))
	mux.HandleFunc("GET /opds/cover/{id}", s.requireBasicAuth(s.handleCover))
	mux.HandleFunc("GET /opds/download/{id}/{format}", s.requireBasicAuth(s.handleDownload))
}

func (s *Server) handleOPDSRoot(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	total := s.visibleCount(r)

	f := newFeed("kobibri:catalogue", "kobibri", base)
	f.Links = []link{
		{Rel: "self", Href: base + "/opds", Type: opdsNav},
		{Rel: "start", Href: base + "/opds", Type: opdsNav},
		{Rel: "search", Href: base + "/opds/opensearch.xml", Type: "application/opensearchdescription+xml"},
	}
	f.Entries = []entry{
		navEntry("kobibri:all", "All books", "Everything you can read, by title.",
			base+"/opds/all", total),
		navEntry("kobibri:new", "Recently added", "Newest first.", base+"/opds/new", 0),
	}
	writeFeed(w, f)
}

func (s *Server) handleOPDSAll(w http.ResponseWriter, r *http.Request) {
	s.opdsPage(w, r, "kobibri:all", "All books", "/opds/all", store.LibraryQuery{})
}

func (s *Server) handleOPDSNew(w http.ResponseWriter, r *http.Request) {
	s.opdsPage(w, r, "kobibri:new", "Recently added", "/opds/new",
		store.LibraryQuery{Sort: "added"})
}

func (s *Server) handleOPDSSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	s.opdsPage(w, r, "kobibri:search", "Search: "+query, "/opds/search",
		store.LibraryQuery{Search: query})
}

// opdsPage renders one page of an acquisition feed.
func (s *Server) opdsPage(w http.ResponseWriter, r *http.Request, id, title, path string, q store.LibraryQuery) {
	user := userFrom(r.Context())
	base := s.publicBase(r)

	start, _ := strconv.Atoi(r.URL.Query().Get("start"))
	if start < 0 {
		start = 0
	}

	// Only books that can actually be handed over, and only the ones this person
	// is allowed to see: the catalogue answers to the same rules as a sync.
	q.Only = "syncable"
	q.UserID = user.ID
	q.Limit = opdsPageSize
	q.Offset = start

	rows, total, err := store.ListLibrary(r.Context(), s.store.Reader(), q)
	if err != nil {
		http.Error(w, "catalogue unavailable", http.StatusInternalServerError)
		return
	}

	f := newFeed(id, title, base)
	f.TotalResults = total
	f.ItemsPerPage = opdsPageSize
	f.StartIndex = start

	self := base + path + carryQuery(r, start)
	f.Links = []link{
		{Rel: "self", Href: self, Type: opdsAcquisition},
		{Rel: "start", Href: base + "/opds", Type: opdsNav},
		{Rel: "search", Href: base + "/opds/opensearch.xml", Type: "application/opensearchdescription+xml"},
	}
	if start+opdsPageSize < total {
		f.Links = append(f.Links, link{Rel: "next",
			Href: base + path + carryQuery(r, start+opdsPageSize), Type: opdsAcquisition})
	}
	if start > 0 {
		f.Links = append(f.Links, link{Rel: "previous",
			Href: base + path + carryQuery(r, max(0, start-opdsPageSize)), Type: opdsAcquisition})
	}

	for _, row := range rows {
		f.Entries = append(f.Entries, s.bookEntry(base, row))
	}
	writeFeed(w, f)
}

func (s *Server) bookEntry(base string, row store.LibraryRow) entry {
	e := entry{
		// A urn:uuid id is what every reader keys on, and ours is already a uuid,
		// so a book keeps its place in a reading list across rebuilds.
		Title:   row.Title,
		ID:      "urn:uuid:" + row.ID,
		Updated: opdsTime(time.Now()),
	}
	for _, name := range authorList(row.Authors) {
		e.Authors = append(e.Authors, author{Name: name})
	}
	if row.SeriesName != "" {
		e.Series = row.SeriesName
		if row.SeriesIndex.Valid {
			e.Series += " #" + strconv.FormatFloat(row.SeriesIndex.Float64, 'g', -1, 64)
		}
	}

	if row.CoverImageID != "" {
		cover := base + "/opds/cover/" + row.ID
		e.Links = append(e.Links,
			link{Rel: "http://opds-spec.org/image", Href: cover + "?size=large", Type: "image/jpeg"},
			link{Rel: "http://opds-spec.org/image/thumbnail", Href: cover + "?size=small", Type: "image/jpeg"})
	}

	// EPUB first: a reading app that is not a Kobo wants the plain file, and
	// several of them take the first acquisition link they understand.
	e.Links = append(e.Links, link{
		Rel:  "http://opds-spec.org/acquisition",
		Href: base + "/opds/download/" + row.ID + "/EPUB",
		Type: "application/epub+zip",
	})
	if row.Format == store.FormatKEPUB {
		e.Links = append(e.Links, link{
			Rel:   "http://opds-spec.org/acquisition",
			Href:  base + "/opds/download/" + row.ID + "/KEPUB",
			Type:  "application/epub+zip",
			Title: "KEPUB (Kobo)",
		})
	}
	return e
}

// handleOPDSDescription is the OpenSearch document a reader fetches to learn how
// to search this catalogue.
func (s *Server) handleOPDSDescription(w http.ResponseWriter, r *http.Request) {
	base := s.publicBase(r)
	doc := struct {
		XMLName     xml.Name `xml:"OpenSearchDescription"`
		Xmlns       string   `xml:"xmlns,attr"`
		ShortName   string   `xml:"ShortName"`
		Description string   `xml:"Description"`
		InputEnc    string   `xml:"InputEncoding"`
		URL         struct {
			Type     string `xml:"type,attr"`
			Template string `xml:"template,attr"`
		} `xml:"Url"`
	}{
		Xmlns:       "http://a9.com/-/spec/opensearch/1.1/",
		ShortName:   "kobibri",
		Description: "Search this library",
		InputEnc:    "UTF-8",
	}
	doc.URL.Type = opdsAcquisition
	doc.URL.Template = base + "/opds/search?q={searchTerms}"

	w.Header().Set("Content-Type", "application/opensearchdescription+xml; charset=utf-8")
	io := xml.NewEncoder(w)
	io.Indent("", "  ")
	w.Write([]byte(xml.Header))
	io.Encode(doc)
	io.Flush()
}

func (s *Server) visibleCount(r *http.Request) int {
	user := userFrom(r.Context())
	_, total, err := store.ListLibrary(r.Context(), s.store.Reader(),
		store.LibraryQuery{Only: "syncable", UserID: user.ID, Limit: 1})
	if err != nil {
		return 0
	}
	return total
}

func newFeed(id, title, base string) *feed {
	return &feed{
		Xmlns:       "http://www.w3.org/2005/Atom",
		XmlnsDC:     "http://purl.org/dc/terms/",
		XmlnsOPDS:   "http://opds-spec.org/2010/catalog",
		XmlnsSearch: "http://a9.com/-/spec/opensearch/1.1/",
		ID:          id,
		Title:       title,
		Updated:     opdsTime(time.Now()),
		Author:      author{Name: "kobibri", URI: base},
	}
}

func navEntry(id, title, summary, href string, count int) entry {
	return entry{
		Title:   title,
		ID:      id,
		Updated: opdsTime(time.Now()),
		Content: &text{Type: "text", Body: summary},
		Links:   []link{{Rel: "subsection", Href: href, Type: opdsAcquisition, Count: count}},
	}
}

func writeFeed(w http.ResponseWriter, f *feed) {
	w.Header().Set("Content-Type", opdsAcquisition+"; charset=utf-8")
	w.Write([]byte(xml.Header))

	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(f); err == nil {
		enc.Flush()
	}
}

// carryQuery keeps the search term across pages while replacing the offset.
func carryQuery(r *http.Request, start int) string {
	q := url.Values{}
	if term := r.URL.Query().Get("q"); term != "" {
		q.Set("q", term)
	}
	if start > 0 {
		q.Set("start", strconv.Itoa(start))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

func opdsTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// requireBasicAuth authenticates a reading app.
//
// Comparing a bcrypt hash takes long enough to notice, and a reader fetches a
// feed, a cover and a book in quick succession, so a successful pair is
// remembered briefly. The cache holds a hash of the password, never the password.
func (s *Server) requireBasicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, password, ok := r.BasicAuth()
		if !ok {
			basicAuthChallenge(w)
			return
		}

		user, err := s.basicAuth.check(r, s, name, password)
		if err != nil {
			basicAuthChallenge(w)
			return
		}
		next(w, r.WithContext(withUser(r.Context(), user)))
	}
}

func basicAuthChallenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="kobibri", charset="UTF-8"`)
	http.Error(w, "sign in with your kobibri account", http.StatusUnauthorized)
}

// basicAuthCache remembers recent successful logins for a short while.
type basicAuthCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	users map[string]basicAuthEntry
}

type basicAuthEntry struct {
	user    *store.User
	expires time.Time
}

func newBasicAuthCache(ttl time.Duration) *basicAuthCache {
	return &basicAuthCache{ttl: ttl, users: map[string]basicAuthEntry{}}
}

func (c *basicAuthCache) check(r *http.Request, s *Server, name, password string) (*store.User, error) {
	key := name + "\x00" + password

	c.mu.Lock()
	if e, ok := c.users[key]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.user, nil
	}
	c.mu.Unlock()

	user, err := s.authenticate(r.Context(), name, password)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.users[key] = basicAuthEntry{user: user, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return user, nil
}
