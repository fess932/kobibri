package kobo

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// downloadWriteTimeout bounds one transfer. The server has no global write
// timeout precisely so a large book over slow Wi-Fi is not cut off mid-file;
// the deadline is set per request instead.
const downloadWriteTimeout = 30 * time.Minute

// handleDownload serves GET /download/{uuid}/{format}.
//
// Kobo devices resume interrupted downloads, so this goes through
// http.ServeContent to get range request support for free.
func (h *Handler) handleDownload(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	id := r.PathValue("uuid")

	resolved, err := store.ResolveBookID(r.Context(), h.store.Reader(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// A book this device deleted should not have a live URL on it at all.
	if device != nil {
		if tombstoned, err := store.HasTombstone(r.Context(), h.store.Reader(),
			device.ID, resolved); err == nil && tombstoned {
			http.NotFound(w, r)
			return
		}
	}

	book, err := store.GetBook(r.Context(), h.store.Reader(), resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	srcPath, err := h.epubPath(r, book)
	if err != nil {
		slog.Warn("no servable file for book", "book", book.ID, "title", book.Title, "err", err)
		http.NotFound(w, r)
		return
	}

	servePath, filename := srcPath, downloadFilename(book, filepath.Ext(srcPath))

	// A fixed-layout book is served untouched: it already has one page per
	// chapter, and converting it would break full-screen rendering.
	if book.DownloadFormat == store.FormatKEPUB {
		// The extension is load-bearing however we got here — a conversion that
		// failed, or a library that was already KEPUB. Kobo picks its renderer
		// by filename, and word-level progress is lost without it.
		filename = downloadFilename(book, kepubconv.KepubSuffix)

		// A book the library already holds as a KEPUB is served as it is:
		// converting it again would nest koboSpan ids inside each other.
		if h.kepub != nil && book.ConvertFrom != store.FormatKEPUB {
			if converted, ok := h.convertedPath(r, book, srcPath); ok {
				servePath = converted
			}
		}
	}

	f, err := os.Open(servePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Give the transfer its own deadline rather than relying on a server-wide
	// one, which would have to be either uselessly long or fatally short.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(downloadWriteTimeout))
	}

	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	http.ServeContent(w, r, filename, fi.ModTime(), f)
}

// convertedPath returns the KEPUB for a book, falling back to the original EPUB
// when conversion is impossible.
//
// Serving the original is much better than failing: a 500 on a download makes
// the device retry the whole sync, whereas a plain EPUB reads fine and only
// costs mid-chapter progress tracking.
func (h *Handler) convertedPath(r *http.Request, book *store.Book, srcPath string) (string, bool) {
	if h.kepub.Failed(r.Context(), book.ID, srcPath) {
		return "", false
	}
	path, _, err := h.kepub.Path(r.Context(), book.ID, srcPath)
	if err != nil {
		if !errors.Is(err, kepubconv.ErrTooLarge) {
			slog.Warn("serving the original epub after a failed conversion",
				"book", book.ID, "err", err)
		}
		return "", false
	}
	return path, true
}

// epubPath locates an EPUB for a book, converting from another format if that
// is what the library holds.
func (h *Handler) epubPath(r *http.Request, book *store.Book) (string, error) {
	if path, err := h.ebook.EPUBFor(r.Context(), book); err == nil {
		return path, nil
	}
	// Without a converter, a library that is already EPUB still works.
	return store.BookFilePath(r.Context(), h.store.Reader(), book, "EPUB")
}

// downloadFilename builds a name the device is happy to store.
func downloadFilename(book *store.Book, ext string) string {
	name := book.Title
	if authors := decodeAuthors(book.AuthorsJSON); len(authors) > 0 {
		name += " - " + authors[0]
	}
	name = sanitiseFilename(name)
	if name == "" {
		name = book.ID
	}
	return name + ext
}

// sanitiseFilename strips what a filesystem or an HTTP header would object to.
func sanitiseFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case strings.ContainsRune(`/\:*?"<>|`, r):
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 120 {
		out = strings.TrimSpace(out[:120])
	}
	return out
}

// contentDisposition sends both an ASCII-safe name and the real UTF-8 one.
func contentDisposition(filename string) string {
	return httpx.ContentDisposition(filename)
}
