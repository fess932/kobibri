package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/kobo"
	"github.com/fess932/kobibri/internal/store"
	"github.com/fess932/kobibri/internal/upload"
	"github.com/fess932/kobibri/internal/webimport"
)

//go:embed templates/*.gohtml static/*
var assets embed.FS

// Server renders the browser interface.
type Server struct {
	store     *store.Store
	scanner   *ingest.Scanner
	scheduler *ingest.Scheduler
	kepub     *kepubconv.Cache
	covers    *covers.Cache
	prewarmer *kepubconv.Prewarmer
	imports   *webimport.Importer
	ebook     *ebookconv.Cache
	uploads   *upload.Store
	// cacheDir holds converted files and scaled covers, so purging a book can
	// take its derived files with it.
	cacheDir string
	// basicAuth serves the catalogue, which a reading app reaches with a
	// username and password rather than a browser session.
	basicAuth *basicAuthCache
	// background outlives a request: a download must not be abandoned because
	// the browser navigated away.
	background context.Context

	baseURL    string
	listenAddr string
	templates  *template.Template
	// apiDoc is the Kobo API reference, parsed once. A specification that will
	// not parse is a broken build; finding that out when someone opens the page
	// is finding out too late.
	apiDoc *kobo.APIDoc
}

type Options struct {
	Store     *store.Store
	Scanner   *ingest.Scanner
	Scheduler *ingest.Scheduler
	Kepub     *kepubconv.Cache
	Covers    *covers.Cache
	Prewarmer *kepubconv.Prewarmer
	Imports   *webimport.Importer
	Ebook     *ebookconv.Cache
	Uploads   *upload.Store
	// CacheDir is where converted files and scaled covers live.
	CacheDir   string
	BaseURL    string
	ListenAddr string
	// AdminPassword creates the first account on a fresh install.
	AdminPassword string
}

func New(ctx context.Context, opts Options) (*Server, error) {
	s := &Server{
		store: opts.Store, scanner: opts.Scanner, scheduler: opts.Scheduler,
		kepub: opts.Kepub, covers: opts.Covers, prewarmer: opts.Prewarmer,
		imports: opts.Imports, ebook: opts.Ebook, uploads: opts.Uploads,
		cacheDir: opts.CacheDir, background: ctx,
		basicAuth: newBasicAuthCache(60 * time.Second),
		baseURL:   strings.TrimSuffix(opts.BaseURL, "/"), listenAddr: opts.ListenAddr,
	}

	tmpl, err := template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.templates = tmpl

	if s.apiDoc, err = kobo.ParseOpenAPI(); err != nil {
		return nil, fmt.Errorf("parse the Kobo API document: %w", err)
	}

	if err := s.bootstrapAdmin(ctx, opts.AdminPassword); err != nil {
		return nil, err
	}
	return s, nil
}

// Mount returns the handler for the browser interface.
func (s *Server) Mount() http.Handler {
	mux := http.NewServeMux()

	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheForever(http.FileServer(http.FS(static)))))

	mux.HandleFunc("POST /language", s.handleLanguage)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.requireLogin(s.handleLogout))

	mux.HandleFunc("GET /{$}", s.requireLogin(s.handleDashboard))

	mux.HandleFunc("GET /sources", s.requireAdmin(s.handleSources))
	mux.HandleFunc("POST /sources", s.requireAdmin(s.handleCreateSource))
	// A literal beats a wildcard in ServeMux, so this does not collide with
	// POST /sources/{id}/... — but it does have to be a distinct path shape.
	mux.HandleFunc("POST /sources/collections", s.requireAdmin(s.handleSetCollections))
	mux.HandleFunc("POST /sources/{id}/scan", s.requireAdmin(s.handleScanSource))
	mux.HandleFunc("POST /sources/{id}/enabled", s.requireAdmin(s.handleToggleSource))
	mux.HandleFunc("POST /sources/{id}/delete", s.requireAdmin(s.handleDeleteSource))
	mux.HandleFunc("POST /sources/{id}/edit", s.requireAdmin(s.handleEditSource))
	mux.HandleFunc("POST /sources/{id}/columns", s.requireAdmin(s.handleSetColumns))
	mux.HandleFunc("POST /sources/{id}/sharing", s.requireAdmin(s.handleSetSharing))

	mux.HandleFunc("GET /api", s.requireLogin(s.handleAPIDocs))
	mux.HandleFunc("GET /api/kobo.json", s.requireLogin(s.handleAPISpec))

	mux.HandleFunc("GET /library", s.requireLogin(s.handleLibrary))
	mux.HandleFunc("GET /series", s.requireLogin(s.handleSeries))
	mux.HandleFunc("GET /series/{uuid}", s.requireLogin(s.handleSeriesOne))
	mux.HandleFunc("POST /books/{id}/series", s.requireAdmin(s.handleSetSeries))
	mux.HandleFunc("GET /duplicates", s.requireAdmin(s.handleDuplicates))
	mux.HandleFunc("GET /books/{id}", s.requireLogin(s.handleBook))
	mux.HandleFunc("POST /books/{id}/hidden", s.requireAdmin(s.handleToggleHidden))
	mux.HandleFunc("POST /books/{id}/convert", s.requireAdmin(s.handleRebuildKepub))
	mux.HandleFunc("POST /books/{id}/delete", s.requireAdmin(s.handleDeleteBook))
	mux.HandleFunc("POST /books/{id}/split/{sb}", s.requireAdmin(s.handleSplit))
	mux.HandleFunc("POST /books/{id}/rejoin/{sb}", s.requireAdmin(s.handleRejoin))
	mux.HandleFunc("GET /books/{id}/download/{format}", s.requireLogin(s.handleDownload))
	mux.HandleFunc("GET /books/{id}/cover", s.requireLogin(s.handleCover))
	mux.HandleFunc("GET /books/{id}/read", s.requireLogin(s.handleRead))
	// Files inside the book keep the paths they have in the zip, so the relative
	// links between them resolve without being rewritten.
	mux.HandleFunc("GET /books/{id}/read/{path...}", s.requireLogin(s.handleReadAsset))

	mux.HandleFunc("GET /uploads", s.requireLogin(s.handleUploads))
	mux.HandleFunc("POST /uploads", s.requireLogin(s.handleUpload))
	mux.HandleFunc("POST /uploads/{id}/remove", s.requireLogin(s.handleUploadRemove))

	mux.HandleFunc("GET /imports", s.requireLogin(s.handleImports))
	mux.HandleFunc("POST /imports/lookup", s.requireLogin(s.handleImportLookup))
	mux.HandleFunc("POST /imports", s.requireLogin(s.handleImportStart))
	mux.HandleFunc("POST /imports/{id}/refresh", s.requireLogin(s.handleImportRefresh))
	mux.HandleFunc("POST /imports/token", s.requireAdmin(s.handleImportToken))

	mux.HandleFunc("GET /devices", s.requireLogin(s.handleDevices))
	mux.HandleFunc("POST /devices/tokens", s.requireLogin(s.handleIssueToken))
	mux.HandleFunc("POST /devices/tokens/{hash}/revoke", s.requireLogin(s.handleRevokeToken))
	mux.HandleFunc("POST /devices/{id}/resend", s.requireLogin(s.handleResendLibrary))
	mux.HandleFunc("POST /devices/{id}/tombstones/{book}/forget", s.requireLogin(s.handleForgetTombstone))

	s.mountOPDS(mux)

	mux.HandleFunc("GET /users", s.requireAdmin(s.handleUsers))
	mux.HandleFunc("POST /users", s.requireAdmin(s.handleCreateUser))
	mux.HandleFunc("POST /users/{id}/password", s.requireAdmin(s.handleSetPassword))
	mux.HandleFunc("POST /users/{id}/delete", s.requireAdmin(s.handleDeleteUser))

	return mux
}

func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// page is the data every template receives.
type page struct {
	Title   string
	Nav     string
	User    *store.User
	CSRF    string
	Flash   string
	Error   string
	BaseURL string
	Lang    Lang
	Data    any
}

// langed pairs a row with the language, so a sub-template still knows how to
// translate once it is out of the page's scope.
type langed struct {
	Lang Lang
	Row  any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	if u := userFrom(r.Context()); u != nil {
		p.User = u
	}
	if sess := sessionFrom(r.Context()); sess != nil {
		p.CSRF = sess.CSRF
	}
	p.Lang = langOf(r)
	// A flash carries a catalogue key when the message is fixed, and literal
	// text when it has to name a library or a count. Translating only what is a
	// known key handles both without the caller having to say which it is.
	if p.Flash == "" {
		p.Flash = T(p.Lang, r.URL.Query().Get("ok"))
	}
	if p.Error == "" {
		p.Error = T(p.Lang, r.URL.Query().Get("err"))
	}
	p.BaseURL = s.publicBase(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, p); err != nil {
		slog.Error("rendering page", "template", name, "err", err)
	}
}

// publicBase is the URL a device should be pointed at.
func (s *Server) publicBase(r *http.Request) string {
	if s.baseURL != "" {
		return s.baseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// redirect sends the browser back with a message, so a POST never leaves a
// resubmittable page in history.
func redirect(w http.ResponseWriter, r *http.Request, path, ok, errMsg string) {
	q := url.Values{}
	if ok != "" {
		q.Set("ok", ok)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func urlEscape(s string) string { return url.QueryEscape(s) }

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"t":    T,
		"dict": templateDict,
		"langs": func() []struct {
			Code Lang
			Name string
		} {
			return Languages
		},
		"authors": formatAuthors,
		"ago":     humanAgo,
		"bytes":   humanBytes,
		"num":     humanNumber,
		"shortID": func(s string) string { return firstN(s, 8) },
		"pct": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a * 100 / b
		},
		"add": func(a, b int) int { return a + b },
		// The overview, the books and the series are one section of the
		// interface split three ways. Both the spine and the strip along the
		// top ask this, so which pages belong together is decided once.
		"libraryNav": func(nav string) bool {
			return nav == "dashboard" || nav == "library" || nav == "series"
		},
		"seriesOf":  formatSeries,
		"hasPrefix": strings.HasPrefix,
		"markdown":  renderProse,
	}
}

// authorList decodes the stored JSON array.
func authorList(authorsJSON string) []string {
	var authors []string
	if err := json.Unmarshal([]byte(authorsJSON), &authors); err != nil {
		return nil
	}
	return authors
}

// formatAuthors turns the stored JSON array into something readable. It takes
// the language because both the fallback and the "and N others" tail are read
// by a person.
func formatAuthors(lang Lang, authorsJSON string) string {
	authors := authorList(authorsJSON)
	switch {
	case len(authors) == 0:
		return T(lang, "book.unknownAuthor")
	case len(authors) > 2:
		return authors[0] + " " + T(lang, Msg("book.andOthers", strconv.Itoa(len(authors)-1)))
	}
	return strings.Join(authors, ", ")
}

func formatSeries(name string, index any) string {
	if name == "" {
		return ""
	}
	switch v := index.(type) {
	case float64:
		return fmt.Sprintf("%s #%g", name, v)
	}
	return name
}

// humanAgo renders a stored timestamp as elapsed time, which is what an
// operator actually wants to know about a scan or a sync.
func humanAgo(ts string) string {
	if ts == "" {
		return "never"
	}
	t := store.ParseTime(ts)
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	default:
		return t.Format("2 Jan 2006")
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanNumber groups thousands with a thin space, which keeps columns readable
// without the noise of commas.
func humanNumber(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 4 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteString(" ")
		}
		b.WriteRune(r)
	}
	return b.String()
}

// templateDict builds a map inline, which is how a sub-template is handed more
// than one value.
func templateDict(pairs ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}
		out[key] = pairs[i+1]
	}
	return out
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
