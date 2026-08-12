package web_test

import (
	"context"
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

	srv, err := web.New(ctx, web.Options{
		Store: st, Scanner: scanner, Kepub: kepubCache, Covers: coverCache, Ebook: ebookCache,
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
		"/sources", "/users", "/imports", "/books/" + e.bookID,
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
			for _, prefix := range []string{"nav.", "dash.", "th.", "pill.", "book.", "library.", "devices.", "users.", "sources.", "flash."} {
				if strings.Contains(body, ">"+prefix) {
					t.Errorf("an untranslated catalogue key leaked into the page: %s…", prefix)
				}
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
