package web_test

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/calibre/calibretest"
	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
	"github.com/fess932/kobibri/internal/upload"
	"github.com/fess932/kobibri/internal/web"
)

const testPassword = "hunter2hunter2"

type env struct {
	t      *testing.T
	store  *store.Store
	server *httptest.Server
	client *http.Client
	ctx    context.Context
	bookID string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "kobibri.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	lib := calibretest.New(t,
		calibretest.BookSpec{Title: "Readable One", Authors: []string{"Jane Author"},
			Series: "A Series", SeriesIndex: 1, Publisher: "Some Press", Cover: true},
		calibretest.BookSpec{Title: "Fixed Art",
			Formats: []calibretest.FormatSpec{{Format: "EPUB", Kind: "pre-paginated"}}},
	)

	scanner := ingest.NewScanner(st, filepath.Join(dir, "tmp"))
	sourceID, err := store.CreateSource(ctx, st.Writer(), &store.Source{
		Name: "Main shelf", LibraryPath: lib.Path, Priority: 10, Enabled: true, ShareAll: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(ctx, sourceID, ingest.ScanOptions{Force: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	kepubCache, err := kepubconv.NewCache(kepubconv.Options{Dir: filepath.Join(dir, "kepub"), Store: st})
	if err != nil {
		t.Fatal(err)
	}
	coverCache, err := covers.NewCache(filepath.Join(dir, "covers"), st)
	if err != nil {
		t.Fatal(err)
	}
	ebookCache, err := ebookconv.New(ebookconv.Options{
		Dir: filepath.Join(dir, "epub"), Store: st,
	})
	if err != nil {
		t.Fatal(err)
	}

	uploads, err := upload.New(st, filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}

	srv, err := web.New(ctx, web.Options{
		Store: st, Scanner: scanner, Kepub: kepubCache, Covers: coverCache, Ebook: ebookCache,
		Uploads:       uploads,
		Prewarmer:     kepubconv.NewPrewarmer(kepubCache, st, ebookCache),
		AdminPassword: testPassword, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("new web server: %v", err)
	}

	ts := httptest.NewServer(srv.Mount())
	t.Cleanup(ts.Close)

	jar, _ := newJar()
	e := &env{t: t, store: st, server: ts, ctx: ctx,
		client: &http.Client{Jar: jar}}

	if err := st.Reader().QueryRowContext(ctx,
		`SELECT id FROM books WHERE title = 'Readable One'`).Scan(&e.bookID); err != nil {
		t.Fatal(err)
	}
	return e
}

func newJar() (http.CookieJar, error) {
	return cookieJar{store: map[string][]*http.Cookie{}}, nil
}

// cookieJar is a minimal jar: the test only ever talks to one host.
type cookieJar struct{ store map[string][]*http.Cookie }

func (j cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		existing := j.store[u.Host]
		replaced := false
		for i, old := range existing {
			if old.Name == c.Name {
				existing[i] = c
				replaced = true
			}
		}
		if !replaced {
			existing = append(existing, c)
		}
		j.store[u.Host] = existing
	}
}

func (j cookieJar) Cookies(u *url.URL) []*http.Cookie { return j.store[u.Host] }

func (e *env) login() {
	e.t.Helper()
	resp, err := e.client.PostForm(e.server.URL+"/login",
		url.Values{"name": {"admin"}, "password": {testPassword}})
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 303 {
		e.t.Fatalf("login status = %d", resp.StatusCode)
	}
}

func (e *env) get(path string, headers ...[2]string) (int, string) {
	e.t.Helper()
	req, err := http.NewRequest("GET", e.server.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()

	var sb strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func (e *env) csrf() string {
	e.t.Helper()
	_, body := e.get("/")
	const marker = `name="csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		e.t.Fatal("no CSRF token on the page")
	}
	rest := body[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	return rest[:end]
}

// Every page must render. A template that references a function or a field that
// does not exist only fails when it is executed, so without this the server
// starts fine and breaks in front of a person.
func TestEveryPageRenders(t *testing.T) {
	e := newEnv(t)
	e.login()

	for _, path := range []string{
		"/", "/library", "/library?q=Readable&only=syncable", "/devices",
		"/sources", "/users", "/imports", "/uploads", "/duplicates", "/books/" + e.bookID,
		"/series", "/series?q=Series", "/series/" + ingest.SeriesUUID("A Series"),
		"/library?sort=activity", "/api", "/stats",
	} {
		t.Run(path, func(t *testing.T) {
			status, body := e.get(path)
			if status != 200 {
				t.Fatalf("status = %d", status)
			}
			if !strings.Contains(body, "</html>") {
				t.Error("page is truncated; a template failed part-way through")
			}
			// An unresolved key is rendered verbatim, which is the tell-tale of
			// a phrase missing from the catalogue.
			for _, prefix := range []string{"nav.", "dash.", "th.", "pill.", "book.", "library.", "devices.", "users.", "sources.", "collections.", "uploads.", "upload.", "read.", "dupes.", "opds.", "warn.", "err.", "flash.", "series.", "api.", "stats."} {
				if strings.Contains(body, ">"+prefix) {
					t.Errorf("an untranslated catalogue key leaked into the page: %s…", prefix)
				}
			}
			// A phrase that names something carries the value after a
			// separator; seeing one on the page means the phrase itself was
			// never found in the catalogue.
			if strings.Contains(body, "\x1f") {
				t.Error("a phrase reached the page with its argument still attached")
			}
		})
	}
}

// Signed-out visitors must not reach anything but the login page.
func TestPagesRequireLogin(t *testing.T) {
	e := newEnv(t)

	for _, path := range []string{"/", "/library", "/devices", "/sources", "/users"} {
		req, _ := http.NewRequest("GET", e.server.URL+path, nil)
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want a redirect to the login page", path, resp.StatusCode)
		}
	}
}

// English is the default, the browser's preference is honoured, and an explicit
// choice beats the browser.
func TestLanguageSelection(t *testing.T) {
	e := newEnv(t)
	e.login()

	_, body := e.get("/")
	if !strings.Contains(body, "Overview") {
		t.Error("the default language is not English")
	}

	_, body = e.get("/", [2]string{"Accept-Language", "ru-RU,ru;q=0.9"})
	if !strings.Contains(body, "Обзор") {
		t.Error("Accept-Language: ru was not honoured")
	}

	// An explicit choice is remembered and outranks the browser.
	resp, err := e.client.PostForm(e.server.URL+"/language",
		url.Values{"lang": {"ru"}, "next": {"/"}, "csrf": {e.csrf()}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_, body = e.get("/", [2]string{"Accept-Language", "en-US,en;q=0.9"})
	if !strings.Contains(body, "Обзор") {
		t.Error("an explicit Russian choice did not beat Accept-Language: en")
	}

	// And it can be switched back.
	resp, err = e.client.PostForm(e.server.URL+"/language",
		url.Values{"lang": {"en"}, "next": {"/"}, "csrf": {e.csrf()}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if _, body = e.get("/"); !strings.Contains(body, "Overview") {
		t.Error("could not switch back to English")
	}
}

// The login page has to be readable before anyone can sign in, so it localises
// without a session.
func TestLoginPageLocalises(t *testing.T) {
	e := newEnv(t)

	_, body := e.get("/login", [2]string{"Accept-Language", "ru"})
	if !strings.Contains(body, "Войти") {
		t.Error("the login page did not localise for a signed-out visitor")
	}
}

// Every phrase must exist in both languages, or the interface is half English
// for a Russian reader.
func TestCatalogueIsComplete(t *testing.T) {
	for _, key := range web.CatalogueKeys() {
		en := web.T(web.LangEN, key)
		ru := web.T(web.LangRU, key)
		if en == key {
			t.Errorf("%s has no English phrase", key)
		}
		if ru == en {
			t.Errorf("%s has no Russian phrase (it fell back to English)", key)
		}
	}
}

// Downloads are offered as the original and as the converted file; only the
// converted one goes to a Kobo.
func TestDownloadsFromTheBrowser(t *testing.T) {
	e := newEnv(t)
	e.login()

	for _, format := range []string{"EPUB", "KEPUB"} {
		status, _ := e.get("/books/" + e.bookID + "/download/" + format)
		if status != 200 {
			t.Errorf("%s download status = %d", format, status)
		}
	}
	if status, _ := e.get("/books/" + e.bookID + "/cover"); status != 200 {
		t.Errorf("cover status = %d", status)
	}
}

// A state-changing request without the session's token must be refused.
func TestCSRFIsEnforced(t *testing.T) {
	e := newEnv(t)
	e.login()

	resp, err := e.client.PostForm(e.server.URL+"/devices/tokens", url.Values{"label": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 without a CSRF token", resp.StatusCode)
	}
}

// Reading a book in the browser has to actually open the file that syncs, not
// just render a frame around nothing.
func TestReadingABookInTheBrowser(t *testing.T) {
	e := newEnv(t)
	e.login()

	status, body := e.get("/books/" + e.bookID + "/read")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if strings.Contains(body, "note-bad") {
		t.Fatalf("the reader refused to open the book:\n%s", body)
	}
	if !strings.Contains(body, `sandbox=""`) {
		t.Error("the book is framed without a sandbox; it is untrusted content")
	}

	// The frame's source must be a real file inside the book.
	const marker = `class="reader-page" src="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("no page frame on the reader")
	}
	rest := body[i+len(marker):]
	src := rest[:strings.IndexByte(rest, '"')]

	status, chapter := e.get(src)
	if status != 200 {
		t.Fatalf("chapter %s: status = %d", src, status)
	}
	if !strings.Contains(strings.ToLower(chapter), "<html") {
		t.Errorf("chapter %s did not come back as a document:\n%s", src, chapter)
	}
}

// A path in the URL must not reach outside the book.
func TestTheReaderRefusesToLeaveTheBook(t *testing.T) {
	e := newEnv(t)
	e.login()

	for _, path := range []string{
		"/books/" + e.bookID + "/read/../../../etc/passwd",
		"/books/" + e.bookID + "/read/nothing-here.xhtml",
	} {
		if status, _ := e.get(path); status == 200 {
			t.Errorf("%s was served", path)
		}
	}
}

// Uploading through the browser has to file the book, not just accept the bytes.
func TestUploadingAFileThroughTheBrowser(t *testing.T) {
	e := newEnv(t)
	e.login()

	if status := e.upload(t, "Hand Picked.epub", minimalEPUB(t, "Hand Picked")); status >= 400 {
		t.Fatalf("upload status = %d", status)
	}

	_, page := e.get("/uploads")
	if !strings.Contains(page, "Hand Picked") {
		t.Fatalf("the uploaded book is not listed:\n%s", page)
	}

	// And it is a real book, ready for a device.
	var syncable bool
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT syncable FROM books WHERE title = 'Hand Picked'`).Scan(&syncable); err != nil {
		t.Fatalf("the upload did not become a book: %v", err)
	}
	if !syncable {
		t.Error("the uploaded book is not offered to devices")
	}
}

// A file a Kobo cannot read must be turned away with a reason, not stored.
func TestUploadingSomethingUnreadable(t *testing.T) {
	e := newEnv(t)
	e.login()

	if status := e.upload(t, "scan.pdf", []byte("%PDF-1.4")); status >= 400 {
		t.Fatalf("upload status = %d — a refused file should still answer politely", status)
	}

	_, page := e.get("/uploads")
	if strings.Contains(page, "scan.pdf</strong>") {
		t.Error("a PDF was filed as a book")
	}
}

// upload posts one file the way the form does: the CSRF token in the query,
// because reading it from the body would consume the upload stream.
func (e *env) upload(t *testing.T, name string, content []byte) int {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	mw.Close()

	req, _ := http.NewRequest("POST", e.server.URL+"/uploads?csrf="+urlQuery(e.csrf()), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func urlQuery(s string) string { return url.QueryEscape(s) }

// minimalEPUB is the smallest thing the ingest path will accept as a book.
func minimalEPUB(t *testing.T, title string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"content.opf": `<package xmlns:dc="http://purl.org/dc/elements/1.1/" version="2.0">
			  <metadata><dc:title>` + title + `</dc:title>
			    <dc:creator>Jane Author</dc:creator></metadata>
			  <manifest><item id="c1" href="one.xhtml" media-type="application/xhtml+xml"/></manifest>
			  <spine><itemref idref="c1"/></spine></package>`,
		"one.xhtml": `<html><body><p>Words.</p></body></html>`,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, content)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The library shows what just arrived first. Alphabetical order buries it, and
// what someone came to look at is nearly always the newest thing.
func TestTheLibraryShowsNewestFirst(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Two books whose alphabetical order is the opposite of their arrival order.
	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET title = 'Aaa Oldest', sort_title = 'Aaa Oldest',
		        created_at = '2020-01-01T00:00:00Z'
		 WHERE title = 'Readable One'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET title = 'Zzz Newest', sort_title = 'Zzz Newest',
		        created_at = '2026-01-01T00:00:00Z'
		 WHERE title = 'Fixed Art'`); err != nil {
		t.Fatal(err)
	}

	_, body := e.get("/library")
	newest := strings.Index(body, "Zzz Newest")
	oldest := strings.Index(body, "Aaa Oldest")
	if newest < 0 || oldest < 0 {
		t.Fatalf("both books should be listed (newest=%d oldest=%d)", newest, oldest)
	}
	if newest > oldest {
		t.Error("the older book came first; the default order is not newest first")
	}

	// And the other order is still available.
	_, body = e.get("/library?sort=title")
	newest = strings.Index(body, "Zzz Newest")
	oldest = strings.Index(body, "Aaa Oldest")
	if oldest > newest {
		t.Error("sorting by title did not put Aaa before Zzz")
	}
}

// A book's own pictures have to reach the page. They are subresources of a
// sandboxed frame, so a policy written in terms of 'self' would block every one
// of them: the frame has no origin to be the same as.
func TestPicturesInsideABookAreServed(t *testing.T) {
	e := newEnv(t)
	e.login()

	e.upload(t, "Illustrated.epub", illustratedEPUB(t))

	var bookID string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM books WHERE title = 'Illustrated'`).Scan(&bookID); err != nil {
		t.Fatal(err)
	}

	status, image := e.get("/books/" + bookID + "/read/images/plate.png")
	if status != 200 {
		t.Fatalf("the picture was not served: status %d", status)
	}
	if len(image) == 0 {
		t.Error("the picture came back empty")
	}

	// And the policy must name a real source for it, not 'self'.
	resp, err := e.client.Get(e.server.URL + "/books/" + bookID + "/read/images/plate.png")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	policy := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src http") {
		t.Errorf("img-src does not name this server, so a sandboxed frame cannot load it: %q", policy)
	}
	if strings.Contains(policy, "img-src 'self'") {
		t.Errorf("img-src is 'self', which an opaque origin never matches: %q", policy)
	}
	if !strings.Contains(policy, "script-src 'none'") {
		t.Errorf("scripts are not blocked: %q", policy)
	}
}

// illustratedEPUB is a book with a picture in it.
func illustratedEPUB(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf"/></rootfiles></container>`,
		"content.opf": `<package xmlns:dc="http://purl.org/dc/elements/1.1/" version="3.0">
			  <metadata><dc:title>Illustrated</dc:title></metadata>
			  <manifest>
			    <item id="c1" href="one.xhtml" media-type="application/xhtml+xml"/>
			    <item id="img" href="images/plate.png" media-type="image/png"/>
			  </manifest>
			  <spine><itemref idref="c1"/></spine></package>`,
		"one.xhtml":        `<html><body><img src="images/plate.png" alt=""/></body></html>`,
		"images/plate.png": string(testPNG()),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testPNG() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
		0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
		0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
		0x0d, 0x0a, 0x2d, 0xb4,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
	}
}

// The library grid and the book page ask for the same cover at different sizes.
// One of them working and the other not is the shape of the bug reported here,
// so both are checked against the same book.
func TestACoverIsServedAtEverySize(t *testing.T) {
	e := newEnv(t)
	e.login()

	e.upload(t, "Pretty.epub", coveredEPUB(t))

	var bookID string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM books WHERE title = 'Pretty'`).Scan(&bookID); err != nil {
		t.Fatal(err)
	}

	placeholder := len(covers.Placeholder())
	for _, path := range []string{
		"/books/" + bookID + "/cover",            // the library grid and the dashboard
		"/books/" + bookID + "/cover?size=large", // the book page
	} {
		status, body := e.get(path)
		if status != 200 {
			t.Errorf("%s: status %d", path, status)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s: empty", path)
		}
		if len(body) == placeholder {
			t.Errorf("%s: served the placeholder, not the book's own cover", path)
		}
	}
}

// coveredEPUB is a book carrying a real cover image.
func coveredEPUB(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile
			full-path="content.opf"/></rootfiles></container>`,
		"content.opf": `<package xmlns:dc="http://purl.org/dc/elements/1.1/" version="3.0">
			  <metadata><dc:title>Pretty</dc:title></metadata>
			  <manifest>
			    <item id="c1" href="one.xhtml" media-type="application/xhtml+xml"/>
			    <item id="cov" href="cover.png" media-type="image/png" properties="cover-image"/>
			  </manifest>
			  <spine><itemref idref="c1"/></spine></package>`,
		"one.xhtml": `<html><body><p>Words.</p></body></html>`,
		"cover.png": string(testPNG()),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A missing cover is a passing state: the book may be mid-import, or its cover
// may be recovered a minute later. The address does not change when it arrives,
// so letting a browser keep the placeholder is how a book stays a grey rectangle
// long after it has a cover.
func TestAMissingCoverIsNeverCached(t *testing.T) {
	e := newEnv(t)
	e.login()

	var bookID string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM books WHERE title = 'Fixed Art'`).Scan(&bookID); err != nil {
		t.Fatal(err)
	}
	// Take its cover away, as a book imported before covers were read out of the
	// file would have arrived.
	if _, err := e.store.Writer().ExecContext(e.ctx,
		`UPDATE books SET cover_image_id = '', cover_source_book_id = NULL WHERE id = ?`,
		bookID); err != nil {
		t.Fatal(err)
	}

	resp, err := e.client.Get(e.server.URL + "/books/" + bookID + "/cover")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if cache := resp.Header.Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control = %q for a placeholder, want no-store", cache)
	}
}

// A cover that is there must be cacheable, and its address must change when the
// cover does — otherwise a browser holding yesterday's picture never asks again.
func TestACoverThatExistsIsCacheableAndVersioned(t *testing.T) {
	e := newEnv(t)
	e.login()

	e.upload(t, "Pretty.epub", coveredEPUB(t))

	var bookID, imageID string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id, cover_image_id FROM books WHERE title = 'Pretty'`).Scan(&bookID, &imageID); err != nil {
		t.Fatal(err)
	}
	if imageID == "" {
		t.Fatal("the uploaded book has no cover image id")
	}

	resp, err := e.client.Get(e.server.URL + "/books/" + bookID + "/cover")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cache := resp.Header.Get("Cache-Control"); !strings.Contains(cache, "max-age") {
		t.Errorf("Cache-Control = %q for a real cover, want it cacheable", cache)
	}

	// And the page hands out an address carrying that id.
	_, body := e.get("/library")
	if !strings.Contains(body, "/cover?v="+imageID) {
		t.Errorf("the library does not version the cover address with %q", imageID)
	}
}

// A book with no series could not be given one at all: the only editor was on a
// series page, and a book in no series appears on none of them.
func TestABookWithNoSeriesCanBeGivenOneFromItsOwnPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	var strayID string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM books WHERE title = 'Fixed Art'`).Scan(&strayID); err != nil {
		t.Fatal(err)
	}

	status, body := e.get("/books/" + strayID)
	if status != 200 {
		t.Fatalf("book page status = %d", status)
	}
	if !strings.Contains(body, `action="/books/`+strayID+`/series"`) {
		t.Fatal("the book page offers no way to set a series")
	}
	// The names already in use are what makes joining an existing series a
	// matter of picking one rather than retyping it exactly right.
	if !strings.Contains(body, `<option value="A Series">`) {
		t.Error("the existing series was not offered as a suggestion")
	}

	resp, err := e.client.PostForm(e.server.URL+"/books/"+strayID+"/series",
		url.Values{"csrf": {e.csrf()}, "series": {"A Series"}, "index": {"2"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var name string
	var index sql.NullFloat64
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT series_name, series_index FROM books WHERE id = ?`, strayID).Scan(&name, &index); err != nil {
		t.Fatal(err)
	}
	if name != "A Series" || !index.Valid || index.Float64 != 2 {
		t.Fatalf("book is in series %q #%v, want \"A Series\" #2", name, index)
	}

	// It has to show up where a series is read, not just in the row it wrote.
	_, page := e.get("/series/" + ingest.SeriesUUID("A Series"))
	if !strings.Contains(page, "Fixed Art") {
		t.Error("the book did not appear on the series page it was moved into")
	}

	// And the book's own page must now offer the way back to the library's
	// answer, which is a different thing from having no series.
	_, body = e.get("/books/" + strayID)
	if !strings.Contains(body, `name="reset"`) {
		t.Error("an edited book offers no way back to what the library says")
	}
}

// Filling a series has to be possible from the series itself. Doing it a book
// at a time from each book's own page means finding ten books by hand.
func TestBooksCanBeAddedToASeriesFromTheSeriesPage(t *testing.T) {
	e := newEnv(t)
	e.login()

	seriesURL := "/series/" + ingest.SeriesUUID("A Series")

	status, body := e.get(seriesURL + "?add=Fixed")
	if status != 200 {
		t.Fatalf("series page status = %d", status)
	}
	if !strings.Contains(body, "Fixed Art") {
		t.Fatal("the search found nothing to add")
	}

	var strayID string
	if err := e.store.Reader().QueryRowContext(e.ctx,
		`SELECT id FROM books WHERE title = 'Fixed Art'`).Scan(&strayID); err != nil {
		t.Fatal(err)
	}

	resp, err := e.client.PostForm(e.server.URL+"/books/"+strayID+"/series",
		url.Values{"csrf": {e.csrf()}, "series": {"A Series"}, "index": {"2"},
			"back": {seriesURL}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_, page := e.get(seriesURL)
	if !strings.Contains(page, "Fixed Art") {
		t.Error("the added book is not on the series page")
	}
	// A book already in the series is not worth offering to add again.
	_, again := e.get(seriesURL + "?add=Fixed")
	if strings.Contains(again, `name="series" value="A Series"`) {
		t.Error("a book already in the series was still offered")
	}
}
