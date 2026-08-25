package kobo

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// handleMetadata serves GET /v1/library/{uuid}/metadata.
//
// The response is an array holding exactly one object, which is what the device
// expects even though it asked about a single book.
func (h *Handler) handleMetadata(w http.ResponseWriter, r *http.Request) {
	book, ok := h.resolveBook(w, r)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, []BookMetadata{h.buildMetadata(r, book)})
}

// resolveBook loads the book a request names, following merge aliases so an id
// a device has held since before a merge still works.
//
// A miss answers with an empty array rather than 404: an error here would make
// the device give up on the whole sync.
func (h *Handler) resolveBook(w http.ResponseWriter, r *http.Request) (*store.Book, bool) {
	id := r.PathValue("uuid")
	if id == "" {
		writeEmptyArray(w)
		return nil, false
	}

	resolved, err := store.ResolveBookID(r.Context(), h.store.Reader(), id)
	if err != nil {
		if !errors.Is(err, store.ErrBookNotFound) {
			slog.Warn("resolving book id", "book", id, "err", err)
		}
		writeEmptyArray(w)
		return nil, false
	}

	book, err := store.GetBook(r.Context(), h.store.Reader(), resolved)
	if err != nil {
		writeEmptyArray(w)
		return nil, false
	}
	return book, true
}

func writeEmptyArray(w http.ResponseWriter) {
	w.Header().Set("Content-Type", httpx.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("[]"))
}

// readingState renders a book's stored progress for the device. A book nobody
// has opened yet reports ReadyToRead rather than nothing, so the device has a
// state to attach its own progress to.
func (h *Handler) readingState(ctx context.Context, userID int64, book *store.Book) (*ReadingState, error) {
	var (
		status                   string
		bookmarkJSON, statsJSON  string
		lastModified, priorityTS string
		timesStarted             int
		lastStarted              string
	)
	err := h.store.Reader().QueryRowContext(ctx, `
		SELECT status, bookmark_json, statistics_json, last_modified, priority_ts,
		       times_started, last_started
		FROM reading_states WHERE user_id = ? AND book_id = ?`, userID, book.ID).
		Scan(&status, &bookmarkJSON, &statsJSON, &lastModified, &priorityTS,
			&timesStarted, &lastStarted)

	created := ParseStored(book.CreatedAt)
	if err != nil {
		return &ReadingState{
			EntitlementID:     book.ID,
			Created:           created,
			LastModified:      created,
			PriorityTimestamp: created,
			StatusInfo:        StatusInfo{LastModified: created, Status: StatusReadyToRead},
		}, nil
	}

	modified := ParseStored(lastModified)
	rs := &ReadingState{
		EntitlementID:     book.ID,
		Created:           created,
		LastModified:      modified,
		PriorityTimestamp: ParseStored(priorityTS),
		StatusInfo: StatusInfo{
			LastModified:           modified,
			Status:                 status,
			TimesStartedReading:    timesStarted,
			LastTimeStartedReading: ParseStored(lastStarted),
		},
	}
	if rs.PriorityTimestamp.IsZero() {
		rs.PriorityTimestamp = modified
	}

	if bookmarkJSON != "" && bookmarkJSON != "null" {
		var bm Bookmark
		if err := json.Unmarshal([]byte(bookmarkJSON), &bm); err == nil {
			rs.CurrentBookmark = &bm
		}
	}
	if statsJSON != "" && statsJSON != "null" {
		var st Statistics
		if err := json.Unmarshal([]byte(statsJSON), &st); err == nil {
			rs.Statistics = &st
		}
	}
	return rs, nil
}

// buildTag renders a collection. A deleted tag needs only its id and timestamp.
func (h *Handler) buildTag(ctx context.Context, tagID string, deleted bool) (*Tag, error) {
	var (
		name         string
		created      string
		lastModified string
	)
	err := h.store.Reader().QueryRowContext(ctx,
		`SELECT name, created_at, last_modified FROM tags WHERE id = ?`, tagID).
		Scan(&name, &created, &lastModified)
	if err != nil {
		return nil, nil
	}

	tag := &Tag{
		Created:      ParseStored(created),
		ID:           tagID,
		LastModified: ParseStored(lastModified),
		Name:         name,
		Type:         tagTypeUser,
	}
	if deleted {
		return tag, nil
	}

	rows, err := h.store.Reader().QueryContext(ctx,
		`SELECT book_id FROM tag_books WHERE tag_id = ? ORDER BY book_id`, tagID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var bookID string
		if err := rows.Scan(&bookID); err != nil {
			return nil, err
		}
		tag.Items = append(tag.Items, TagItem{RevisionID: bookID, Type: tagItemTypeProduct})
	}
	return tag, rows.Err()
}
