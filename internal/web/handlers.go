package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// Login

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if _, err := store.GetSession(r.Context(), s.store.Reader(), cookie.Value); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.render(w, r, "login.gohtml", page{Title: T(langOf(r), "login.title"), Nav: "login"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// The login form has no session yet, so it carries no CSRF token; it is the
	// one POST that cannot be gated on one.
	user, err := s.authenticate(r.Context(), r.FormValue("name"), r.FormValue("password"))
	if err != nil {
		s.render(w, r, "login.gohtml", page{
			Title: T(langOf(r), "login.title"), Nav: "login",
			Error: T(langOf(r), "login.failed"),
		})
		return
	}

	session, err := store.CreateSession(r.Context(), s.store.Writer(), user.ID)
	if err != nil {
		s.render(w, r, "login.gohtml", page{Title: T(langOf(r), "login.title"), Nav: "login",
			Error: T(langOf(r), "login.nosession")})
		return
	}
	s.setSession(w, r, session)

	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLanguage records a language choice. It deliberately needs no session:
// a person has to be able to read the login page before they can sign in.
func (s *Server) handleLanguage(w http.ResponseWriter, r *http.Request) {
	if l, ok := validLang(r.FormValue("lang")); ok {
		setLangCookie(w, l)
	}
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFrom(r.Context()); sess != nil {
		store.DeleteSession(r.Context(), s.store.Writer(), sess.ID)
	}
	s.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Dashboard

type dashboardData struct {
	Stats    *store.Stats
	Sources  []*store.Source
	Devices  []store.DeviceRow
	Warnings []warning
	Recent   []store.LibraryRow
}

// warning is something the operator should act on, surfaced above the numbers.
type warning struct {
	Level  string // warn | critical
	Text   string
	Action string
	Href   string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	stats, err := store.GetStats(ctx, s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sources, err := store.ListSources(ctx, s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	devices, err := store.ListAllDevices(ctx, s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	recent, _, err := store.ListLibrary(ctx, s.store.Reader(),
		store.LibraryQuery{Sort: store.SortNewest, Limit: 12})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data := dashboardData{Stats: stats, Sources: sources, Devices: devices, Recent: recent}

	// Warnings are built here rather than in the template because each one names
	// a library or a count, so the phrase and its value have to be translated
	// together.
	lang := langOf(r)
	warn := func(level, key, href string, arg string) {
		text := T(lang, key)
		if arg != "" {
			text = T(lang, Msg(key, arg))
		}
		data.Warnings = append(data.Warnings, warning{
			Level: level, Text: text, Action: T(lang, key+".action"), Href: href,
		})
	}

	for _, src := range sources {
		switch src.LastStatus {
		case store.SourceStatusUnreachable:
			warn("critical", "warn.unreachable", "/sources", src.Name)
		case store.SourceStatusSuspicious:
			warn("critical", "warn.suspicious", "/sources", src.Name)
		case store.SourceStatusError:
			warn("warn", "warn.scanFailed", "/sources", src.Name)
		}
	}
	if len(sources) == 0 {
		warn("warn", "warn.noSources", "/sources", "")
	}
	if stats.KepubFailed > 0 {
		warn("warn", "warn.unconverted", "/library?only=unconverted",
			strconv.Itoa(stats.KepubFailed))
	}

	s.render(w, r, "dashboard.gohtml", page{Title: T(langOf(r), "dash.title"), Nav: "dashboard", Data: data})
}

// Sources

type sourcesData struct {
	Sources         []sourceView
	CollectionsMode string
}

type sourceView struct {
	*store.Source
	Runs []store.ScanRun
	// Columns are the library's own custom columns, and which of them were
	// chosen to become shelves.
	Columns []columnChoice
}

type columnChoice struct {
	Label    string
	Name     string
	Selected bool
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	sources, err := store.ListSources(r.Context(), s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data := sourcesData{CollectionsMode: ingest.CollectionsMode(r.Context(), s.store.Reader())}
	for _, src := range sources {
		runs, err := store.RecentScanRuns(r.Context(), s.store.Reader(), src.ID, 5)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		view := sourceView{Source: src, Runs: runs}
		chosen := map[string]bool{}
		for _, label := range ingest.ShelfColumns(r.Context(), s.store.Reader(), src.ID) {
			chosen[label] = true
		}
		for _, col := range ingest.KnownColumns(r.Context(), s.store.Reader(), src.ID) {
			if !col.UsableForShelves() {
				continue
			}
			view.Columns = append(view.Columns, columnChoice{
				Label: col.Label, Name: col.Name, Selected: chosen[col.Label]})
		}
		data.Sources = append(data.Sources, view)
	}

	s.render(w, r, "sources.gohtml", page{Title: T(langOf(r), "sources.title"), Nav: "sources", Data: data})
}

// handleSetCollections changes how the library's own organisation is mirrored
// onto the readers' shelves, and applies it at once — waiting for the next scan
// would make the setting look broken.
func (s *Server) handleSetCollections(w http.ResponseWriter, r *http.Request) {
	if err := ingest.SetCollectionsMode(r.Context(), s.store.Writer(), r.FormValue("mode")); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	if err := s.scanner.RebuildCollections(r.Context()); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	redirect(w, r, "/sources", "flash.collectionsSaved", "")
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	path := strings.TrimSpace(r.FormValue("path"))
	if name == "" || path == "" {
		redirect(w, r, "/sources", "", "flash.sourceNeedsNamePath")
		return
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		redirect(w, r, "/sources", "", "flash.badPath")
		return
	}
	if _, err := calibre.Stat(abs); err != nil {
		redirect(w, r, "/sources", "", Msg("flash.noMetadataDb", abs))
		return
	}

	priority, _ := strconv.Atoi(r.FormValue("priority"))
	interval, _ := strconv.Atoi(r.FormValue("interval"))
	src := &store.Source{
		Name: name, LibraryPath: abs, Priority: priority, Enabled: true,
		ShareAll: true, ScanIntervalSec: interval,
	}
	id, err := store.CreateSource(r.Context(), s.store.Writer(), src)
	if err != nil {
		redirect(w, r, "/sources", "", Msg("flash.sourceAddFailed", err.Error()))
		return
	}

	s.scanInBackground(id)
	redirect(w, r, "/sources", Msg("flash.sourceAdded", name), "")
}

func (s *Server) handleEditSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	src, err := store.GetSource(r.Context(), s.store.Reader(), id)
	if err != nil {
		redirect(w, r, "/sources", "", "flash.sourceGone")
		return
	}

	if v := strings.TrimSpace(r.FormValue("name")); v != "" {
		src.Name = v
	}
	if v, err := strconv.Atoi(r.FormValue("priority")); err == nil {
		src.Priority = v
	}
	if v, err := strconv.Atoi(r.FormValue("interval")); err == nil && v > 0 {
		src.ScanIntervalSec = v
	}

	if err := store.UpdateSource(r.Context(), s.store.Writer(), src); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	// Priority decides which library wins for a shared book, so the merged
	// records have to be recomputed before the change means anything.
	if err := s.scanner.ResolveSource(r.Context(), id); err != nil {
		slog.Error("re-resolving after a source edit", "source", id, "err", err)
	}
	redirect(w, r, "/sources", Msg("flash.sourceSaved", src.Name), "")
}

// handleSetColumns records which of a library's own columns become shelves, and
// rebuilds at once — a setting that only takes effect after the next scan looks
// broken.
func (s *Server) handleSetColumns(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	if err := ingest.SetShelfColumns(r.Context(), s.store.Writer(), id, r.Form["columns"]); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	// The values themselves come from the library, and a scan reads only what
	// changed there — so this one has to re-read the lot. It runs in the
	// background, and the shelves appear when it finishes.
	s.scanInBackground(id)
	redirect(w, r, "/sources", "flash.columnsSaved", "")
}

func (s *Server) handleScanSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	confirm := r.FormValue("confirm") == "1"

	res, err := s.scanner.Scan(r.Context(), id, ingest.ScanOptions{Force: true, ConfirmVanish: confirm})
	switch {
	case errors.Is(err, ingest.ErrSuspicious):
		redirect(w, r, "/sources", "", "flash.suspicious")
	case errors.Is(err, calibre.ErrUnreachable):
		redirect(w, r, "/sources", "",
			"flash.unreachable")
	case err != nil:
		redirect(w, r, "/sources", "", err.Error())
	default:
		s.prewarm()
		redirect(w, r, "/sources", fmt.Sprintf("Scanned: %d book(s), %d new, %d updated, %d gone.",
			res.Seen, res.Added, res.Updated, res.Vanished), "")
	}
}

func (s *Server) handleToggleSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	enabled := r.FormValue("enabled") == "1"

	if err := s.scanner.SetSourceEnabled(r.Context(), id, enabled); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	if enabled {
		s.scanInBackground(id)
		redirect(w, r, "/sources", "flash.sourceOn", "")
		return
	}
	redirect(w, r, "/sources",
		"flash.sourceOff", "")
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))

	if err := s.scanner.SetSourceEnabled(r.Context(), id, false); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	if err := store.DeleteSource(r.Context(), s.store.Writer(), id); err != nil {
		redirect(w, r, "/sources", "", err.Error())
		return
	}
	redirect(w, r, "/sources",
		"flash.sourceRemoved", "")
}

// scanInBackground kicks off a scan without making the browser wait for a large
// library to be read.
func (s *Server) scanInBackground(sourceID int64) {
	if s.scheduler != nil {
		s.scheduler.Trigger(sourceID)
	}
}

func (s *Server) prewarm() {
	if s.prewarmer != nil {
		s.prewarmer.Trigger()
	}
}

// Library

type libraryData struct {
	Rows     []store.LibraryRow
	Total    int
	Query    store.LibraryQuery
	Sources  []*store.Source
	Page     int
	Pages    int
	PrevHref string
	NextHref string
}

const libraryPageSize = 48

func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	q := store.LibraryQuery{
		Search: r.URL.Query().Get("q"),
		Only:   r.URL.Query().Get("only"),
		// Newest first by default: what someone came to look at is nearly always
		// what just arrived, and alphabetical order buries it.
		Sort:  r.URL.Query().Get("sort"),
		Limit: libraryPageSize,
	}
	if q.Sort == "" {
		q.Sort = store.SortNewest
	}
	q.SourceID = atoi64(r.URL.Query().Get("source"))
	// Progress is per person: an administrator sees every book, but only their
	// own place in one.
	if user := userFrom(r.Context()); user != nil {
		q.ProgressFor = user.ID
	}

	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pageNum < 1 {
		pageNum = 1
	}
	q.Offset = (pageNum - 1) * libraryPageSize

	rows, total, err := store.ListLibrary(r.Context(), s.store.Reader(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sources, err := store.ListSources(r.Context(), s.store.Reader())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	pages := (total + libraryPageSize - 1) / libraryPageSize
	data := libraryData{Rows: rows, Total: total, Query: q, Sources: sources,
		Page: pageNum, Pages: pages}

	base := r.URL.Query()
	if pageNum > 1 {
		base.Set("page", strconv.Itoa(pageNum-1))
		data.PrevHref = "/library?" + base.Encode()
	}
	if pageNum < pages {
		base.Set("page", strconv.Itoa(pageNum+1))
		data.NextHref = "/library?" + base.Encode()
	}

	s.render(w, r, "library.gohtml", page{Title: T(langOf(r), "library.title"), Nav: "library", Data: data})
}

type bookData struct {
	Book         *store.Book
	Contributors []store.Contributor
	Devices      []store.DeviceBookState
	Formats      []downloadOption
	Converted    bool
	ConvertError string
}

// downloadOption is one row in the download list. Converted says where the file
// comes from, because the same format can mean either the library's own file or
// one this server made, and the two must not read alike.
type downloadOption struct {
	Format string
	Label  string
	Why    string // already translated: where this file comes from
	Href   string
	Size   int64
}

func (s *Server) handleBook(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}

	contributors, err := store.Contributors(r.Context(), s.store.Reader(), book)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	devices, err := store.BookDeviceStates(r.Context(), s.store.Reader(), book.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	data := bookData{Book: book, Contributors: contributors, Devices: devices}

	// Downloads: the files as they sit in Calibre, plus the KEPUB this server
	// makes. Only a KEPUB is ever synced to a Kobo.
	//
	// A library that already holds a KEPUB is the case worth being careful
	// about: nothing is converted then, so listing a converted row as well would
	// be two identical-looking links to the same file.
	lang := langOf(r)
	servesLibraryKepub := book.ConvertFrom == store.FormatKEPUB
	for _, c := range contributors {
		if c.Missing {
			continue
		}
		for _, f := range c.Formats {
			if !f.Present {
				continue
			}
			why := T(lang, Msg("book.asItIsIn", c.SourceName))
			if f.Format == store.FormatKEPUB && servesLibraryKepub {
				why = T(lang, Msg("book.alreadyKepub", c.SourceName))
			}
			data.Formats = append(data.Formats, downloadOption{
				Format: f.Format,
				Label:  f.Format,
				Why:    why,
				Href:   "/books/" + book.ID + "/download/" + f.Format,
				Size:   f.Size,
			})
		}
		break // only the winning source's files
	}

	if book.DownloadFormat == store.FormatKEPUB && !servesLibraryKepub {
		var size int64
		err := s.store.Reader().QueryRowContext(r.Context(),
			`SELECT size FROM kepub_cache WHERE book_id = ? LIMIT 1`, book.ID).Scan(&size)
		data.Converted = err == nil
		data.Formats = append(data.Formats, downloadOption{
			Format: store.FormatKEPUB,
			Label:  "KEPUB",
			Why:    T(lang, "book.convertedFor"),
			Href:   "/books/" + book.ID + "/download/KEPUB",
			Size:   size,
		})
	}

	s.store.Reader().QueryRowContext(r.Context(),
		`SELECT err FROM kepub_failures WHERE book_id = ? LIMIT 1`, book.ID).Scan(&data.ConvertError)

	s.render(w, r, "book.gohtml", page{Title: book.Title, Nav: "library", Data: data})
}

func (s *Server) lookupBook(w http.ResponseWriter, r *http.Request) (*store.Book, bool) {
	id, err := store.ResolveBookID(r.Context(), s.store.Reader(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	book, err := store.GetBook(r.Context(), s.store.Reader(), id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return book, true
}

func (s *Server) handleToggleHidden(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	hidden := r.FormValue("hidden") == "1"

	if err := store.SetBookHidden(r.Context(), s.store.Writer(), book.ID, hidden); err != nil {
		redirect(w, r, "/books/"+book.ID, "", err.Error())
		return
	}
	if hidden {
		redirect(w, r, "/books/"+book.ID,
			"flash.hidden", "")
		return
	}
	redirect(w, r, "/books/"+book.ID, "flash.shown", "")
}

func (s *Server) handleRebuildKepub(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}

	// Drop what we have so the next request converts afresh, then let the
	// background queue do the work.
	s.store.Writer().ExecContext(r.Context(), `DELETE FROM kepub_cache WHERE book_id = ?`, book.ID)
	s.store.Writer().ExecContext(r.Context(), `DELETE FROM kepub_failures WHERE book_id = ?`, book.ID)
	s.prewarm()

	redirect(w, r, "/books/"+book.ID, "flash.converting", "")
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}
	format := strings.ToUpper(r.PathValue("format"))

	if format == store.FormatKEPUB && s.kepub != nil {
		s.serveKepub(w, r, book)
		return
	}

	// EPUB may mean the library's own file or one converted from another format.
	if format == "EPUB" {
		if path, err := s.epubPath(r, book); err == nil {
			s.serveFile(w, r, path, downloadName(book, ".epub"))
			return
		}
	}

	path, err := store.BookFilePath(r.Context(), s.store.Reader(), book, format)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, path, downloadName(book, "."+strings.ToLower(format)))
}

func (s *Server) serveKepub(w http.ResponseWriter, r *http.Request, book *store.Book) {
	src, err := s.epubPath(r, book)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// The library's own KEPUB is already what a Kobo wants.
	if book.ConvertFrom == store.FormatKEPUB {
		s.serveFile(w, r, src, downloadName(book, kepubconv.KepubSuffix))
		return
	}

	path, _, err := s.kepub.Path(r.Context(), book.ID, src)
	if err != nil {
		if errors.Is(err, kepubconv.ErrTooLarge) {
			http.Error(w, T(langOf(r), "err.tooLargeToConvert"),
				http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, T(langOf(r), "err.couldNotConvert"),
			http.StatusUnprocessableEntity)
		return
	}
	s.serveFile(w, r, path, downloadName(book, kepubconv.KepubSuffix))
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path, filename string) {
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	http.ServeContent(w, r, filename, fi.ModTime(), f)
}

// epubPath finds an EPUB for a book, converting from another format when that
// is what the library holds. Without a converter it falls back to whatever EPUB
// is actually on disk, so a plain library still works.
func (s *Server) epubPath(r *http.Request, book *store.Book) (string, error) {
	if path, err := s.ebook.EPUBFor(r.Context(), book); err == nil {
		return path, nil
	}
	return store.BookFilePath(r.Context(), s.store.Reader(), book, "EPUB")
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}

	bucket := covers.BucketFor(360)
	if r.URL.Query().Get("size") == "large" {
		bucket = covers.BucketLarge
	}

	src, _ := store.BookCoverPath(r.Context(), s.store.Reader(), book.ID)
	imageID := book.CoverImageID
	if imageID == "" {
		imageID = book.ID
	}

	if s.covers != nil {
		if path, err := s.covers.Get(imageID, bucket, src); err == nil {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			http.ServeFile(w, r, path)
			return
		}
	}

	// A missing cover is a passing state — the book may be mid-import, or its
	// cover may be recovered a minute from now — and the URL does not change when
	// it arrives. Letting a browser keep the placeholder is how a book stays a
	// grey rectangle long after it has a cover.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(covers.Placeholder())
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("serving a page", "path", r.URL.Path, "err", err)
	http.Error(w, T(langOf(r), "err.pageFailed"), http.StatusInternalServerError)
}
