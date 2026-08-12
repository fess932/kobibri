package web

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fess932/kobibri/internal/reader"
	"github.com/fess932/kobibri/internal/store"
)

type readData struct {
	Book     *store.Book
	Chapters []readChapter
	At       int
	Prev     string
	Next     string
	Frame    string
	Failed   string
}

type readChapter struct {
	Title string
	Href  string
}

// handleRead shows a book in the browser.
//
// It reads the KEPUB — the file that actually syncs — rather than the original,
// because the question this answers is whether the conversion produced something
// readable. Without a Kobo to hand there is no other way to find out.
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}

	data := readData{Book: book}
	path, err := s.readableFile(r, book)
	if err != nil {
		data.Failed = T(langOf(r), "read.noFile")
		s.render(w, r, "read.gohtml", page{Title: book.Title, Nav: "library", Data: data})
		return
	}

	b, err := reader.Open(path)
	if err != nil {
		data.Failed = T(langOf(r), Msg("read.unreadable", err.Error()))
		s.render(w, r, "read.gohtml", page{Title: book.Title, Nav: "library", Data: data})
		return
	}
	defer b.Close()

	at, _ := strconv.Atoi(r.URL.Query().Get("at"))
	if at < 0 || at >= len(b.Spine) {
		at = 0
	}
	data.At = at

	base := "/books/" + book.ID + "/read"
	for i, c := range b.Spine {
		data.Chapters = append(data.Chapters, readChapter{
			Title: c.Title,
			Href:  base + "?at=" + strconv.Itoa(i),
		})
	}
	data.Frame = base + "/" + pathEscape(b.Spine[at].Path)
	if at > 0 {
		data.Prev = data.Chapters[at-1].Href
	}
	if at < len(b.Spine)-1 {
		data.Next = data.Chapters[at+1].Href
	}

	s.render(w, r, "read.gohtml", page{Title: book.Title, Nav: "library", Data: data})
}

// handleReadAsset serves one file from inside the book — a chapter, a
// stylesheet, an image.
//
// Everything is served under the same prefix as it is named inside the zip, so
// the relative links a book uses between its own files resolve without being
// rewritten.
func (s *Server) handleReadAsset(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lookupBook(w, r)
	if !ok {
		return
	}

	path, err := s.readableFile(r, book)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, err := reader.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer b.Close()

	rc, size, contentType, err := b.Open(r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()

	// A book may carry scripts and remote references. The frame sandbox is the
	// real defence against the first; this stops the page reaching out on its own.
	//
	// The allowed source is written out as this server's own address rather than
	// as 'self'. The frame is sandboxed without allow-same-origin, so its origin
	// is opaque, and relying on 'self' there is how a book ends up rendering with
	// none of its pictures and none of its styling.
	base := s.publicBase(r)
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"img-src " + base + " data:",
		"style-src " + base + " 'unsafe-inline'",
		"font-src " + base + " data:",
		"media-src " + base,
		"script-src 'none'",
		"object-src 'none'",
		"frame-ancestors 'self'",
	}, "; "))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", contentType)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	io.Copy(w, rc)
}

// readableFile picks what to open: the KEPUB if there is one, since that is what
// a device gets, and the plain EPUB otherwise.
func (s *Server) readableFile(r *http.Request, book *store.Book) (string, error) {
	src, err := s.epubPath(r, book)
	if err != nil {
		return "", err
	}
	if book.DownloadFormat != store.FormatKEPUB || book.ConvertFrom == store.FormatKEPUB ||
		s.kepub == nil {
		return src, nil
	}
	if s.kepub.Failed(r.Context(), book.ID, src) {
		return src, nil
	}
	path, _, err := s.kepub.Path(r.Context(), book.ID, src)
	if err != nil {
		return src, nil
	}
	return path, nil
}

// pathEscape escapes each segment but keeps the separators, so a chapter inside
// a folder stays inside it.
func pathEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
