package kobo

import (
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/store"
)

// handleCover serves both cover URL shapes the device uses:
//
//	/covers/{ImageId}/{Width}/{Height}/{IsGreyscale}/image.jpg
//	/covers/{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}/image.jpg
//
// A miss returns a placeholder with 200 rather than 404: a device that gets an
// error for a cover retries it relentlessly.
func (h *Handler) handleCover(w http.ResponseWriter, r *http.Request) {
	imageID := r.PathValue("imageId")
	height, _ := strconv.Atoi(r.PathValue("height"))
	bucket := covers.BucketFor(height)

	bookID := normaliseImageID(imageID)
	if bookID == "" || h.covers == nil {
		h.servePlaceholder(w)
		return
	}

	resolved, err := store.ResolveBookID(r.Context(), h.store.Reader(), bookID)
	if err != nil {
		h.servePlaceholder(w)
		return
	}

	srcPath := h.coverSourcePath(r, resolved)
	path, err := h.covers.Get(imageID, bucket, srcPath)
	if err != nil {
		if srcPath != "" {
			slog.Debug("rendering cover", "image", imageID, "err", err)
		}
		h.servePlaceholder(w)
		return
	}

	// The image id embeds the cover's modification time, so a given id always
	// names the same bytes and may be cached forever.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+imageID+"-"+bucket+`"`)
	http.ServeFile(w, r, path)
}

func (h *Handler) servePlaceholder(w http.ResponseWriter) {
	buf := covers.Placeholder()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// coverSourcePath finds the cover file for a book, or "" when it has none.
func (h *Handler) coverSourcePath(r *http.Request, bookID string) string {
	var libraryPath, relPath sql.NullString
	err := h.store.Reader().QueryRowContext(r.Context(), `
		SELECT s.library_path, sb.cover_rel_path
		FROM books b
		JOIN source_books sb ON sb.id = b.cover_source_book_id
		JOIN sources s ON s.id = sb.source_id
		WHERE b.id = ? AND sb.cover_rel_path <> ''`, bookID).Scan(&libraryPath, &relPath)
	if err != nil || !libraryPath.Valid || !relPath.Valid {
		return ""
	}

	full := filepath.Join(libraryPath.String, filepath.FromSlash(relPath.String))
	clean := filepath.Clean(libraryPath.String)
	if full != clean && !strings.HasPrefix(full, clean+string(filepath.Separator)) {
		return ""
	}
	return full
}

// normaliseImageID strips the cache-busting suffix from "<uuid>-<mtime>".
//
// The suffix exists because the device caches covers by image id indefinitely:
// a replaced cover has to arrive under a new id or it is never refetched. Old
// ids keep resolving, which is why the stripping happens here rather than the
// id simply changing.
func normaliseImageID(imageID string) string {
	if imageID == "" {
		return ""
	}
	if _, err := uuid.Parse(imageID); err == nil {
		return imageID
	}
	if i := strings.LastIndexByte(imageID, '-'); i > 0 {
		candidate := imageID[:i]
		if _, err := uuid.Parse(candidate); err == nil {
			return candidate
		}
	}
	return imageID
}
