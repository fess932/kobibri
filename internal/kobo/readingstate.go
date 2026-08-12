package kobo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// handleGetState serves GET /v1/library/{uuid}/state.
//
// Like the metadata endpoint, the answer is an array holding exactly one
// object, even though the device asked about a single book.
func (h *Handler) handleGetState(w http.ResponseWriter, r *http.Request) {
	book, ok := h.resolveBook(w, r)
	if !ok {
		return
	}
	device := deviceFrom(r.Context())
	if device == nil {
		writeEmptyArray(w)
		return
	}

	rs, err := h.readingState(r.Context(), device.UserID, book)
	if err != nil || rs == nil {
		writeEmptyArray(w)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, []ReadingState{*rs})
}

// putStateRequest is what the device sends when reading progress moves.
type putStateRequest struct {
	ReadingStates []struct {
		EntitlementID   string      `json:"EntitlementId"`
		LastModified    KoboTime    `json:"LastModified"`
		StatusInfo      *StatusInfo `json:"StatusInfo"`
		Statistics      *Statistics `json:"Statistics"`
		CurrentBookmark *Bookmark   `json:"CurrentBookmark"`
	} `json:"ReadingStates"`
}

type updateResult struct {
	EntitlementID         string       `json:"EntitlementId"`
	CurrentBookmarkResult resultStatus `json:"CurrentBookmarkResult"`
	StatisticsResult      resultStatus `json:"StatisticsResult"`
	StatusInfoResult      resultStatus `json:"StatusInfoResult"`
	LastModified          KoboTime     `json:"LastModified"`
	PriorityTimestamp     KoboTime     `json:"PriorityTimestamp"`
}

type resultStatus struct {
	Result string `json:"Result"`
}

const (
	resultSuccess = "Success"
	resultIgnored = "Ignored"
)

// handlePutState serves PUT /v1/library/{uuid}/state.
//
// The response shape is fixed and the device checks it, so even a book we
// cannot find gets a well-formed answer rather than an error.
func (h *Handler) handlePutState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("uuid")
	device := deviceFrom(r.Context())

	var req putStateRequest
	if err := httpx.DecodeJSON(r, 256<<10, &req); err != nil {
		slog.Debug("reading state body was not usable", "book", id, "err", err)
	}

	resolved, err := store.ResolveBookID(r.Context(), h.store.Reader(), id)
	if err != nil || device == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"RequestResult": resultSuccess,
			"UpdateResults": []updateResult{{
				EntitlementID:         id,
				CurrentBookmarkResult: resultStatus{resultIgnored},
				StatisticsResult:      resultStatus{resultIgnored},
				StatusInfoResult:      resultStatus{resultIgnored},
				LastModified:          Time(time.Now()),
				PriorityTimestamp:     Time(time.Now()),
			}},
		})
		return
	}

	now := time.Now().UTC()
	results := make([]updateResult, 0, max(1, len(req.ReadingStates)))

	for _, rs := range req.ReadingStates {
		status := StatusReadyToRead
		if rs.StatusInfo != nil && rs.StatusInfo.Status != "" {
			status = rs.StatusInfo.Status
		}

		bookmark := repairBookmark(status, rs.CurrentBookmark)

		if err := h.saveReadingState(r.Context(), device, resolved, status, bookmark, rs.Statistics, now); err != nil {
			slog.Error("saving reading state", "book", resolved, "err", err)
		}

		results = append(results, updateResult{
			EntitlementID:         id,
			CurrentBookmarkResult: resultStatus{resultSuccess},
			StatisticsResult:      resultStatus{resultSuccess},
			StatusInfoResult:      resultStatus{resultSuccess},
			LastModified:          Time(now),
			PriorityTimestamp:     Time(now),
		})
	}

	if len(results) == 0 {
		results = append(results, updateResult{
			EntitlementID:         id,
			CurrentBookmarkResult: resultStatus{resultIgnored},
			StatisticsResult:      resultStatus{resultIgnored},
			StatusInfoResult:      resultStatus{resultIgnored},
			LastModified:          Time(now),
			PriorityTimestamp:     Time(now),
		})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"RequestResult": resultSuccess,
		"UpdateResults": results,
	})
}

// repairBookmark works around a device quirk.
//
// When a book is finished, Kobo sends the *first* resource as the current
// position rather than the last, so storing it verbatim would send the reader
// back to page one on their next device. Komga hit the same thing. A finished
// book is recorded as complete instead, discarding the bogus location.
func repairBookmark(status string, bm *Bookmark) *Bookmark {
	if status != StatusFinished {
		return bm
	}
	repaired := Bookmark{ProgressPercent: 100, ContentSourceProgressPercent: 100}
	if bm != nil {
		repaired.LastModified = bm.LastModified
	}
	return &repaired
}

// saveReadingState records progress and bumps its revision, so other devices
// pick it up on their next sync.
//
// The writing device is recorded so the change is not echoed straight back at
// it, which would have the device fighting its own update.
func (h *Handler) saveReadingState(ctx context.Context, device *store.Device, bookID, status string,
	bookmark *Bookmark, stats *Statistics, now time.Time) error {

	bookmarkJSON := marshalOrNull(bookmark)
	statsJSON := marshalOrNull(stats)
	ts := store.FormatTime(now)

	return h.store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO reading_states
				(user_id, book_id, status, bookmark_json, statistics_json, rev,
				 last_writer_device_id, last_modified, priority_ts)
			VALUES (?,?,?,?,?,1,?,?,?)
			ON CONFLICT(user_id, book_id) DO UPDATE SET
				status = excluded.status,
				bookmark_json = excluded.bookmark_json,
				statistics_json = excluded.statistics_json,
				rev = reading_states.rev + 1,
				last_writer_device_id = excluded.last_writer_device_id,
				last_modified = excluded.last_modified,
				priority_ts = excluded.priority_ts`,
			device.UserID, bookID, status, bookmarkJSON, statsJSON, device.ID, ts, ts)
		return err
	})
}

func marshalOrNull(v any) string {
	if v == nil {
		return "null"
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(buf)
}

// handleDeleteBook serves DELETE /v1/library/{uuid}, which the device sends
// when the user deletes a book on it.
//
// The tombstone is permanent and scoped to this device: the book stays in the
// library, stays on every other device, and is never offered to this one again.
func (h *Handler) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	id := r.PathValue("uuid")

	if device == nil || id == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resolved, err := store.ResolveBookID(r.Context(), h.store.Reader(), id)
	if err != nil {
		if !errors.Is(err, store.ErrBookNotFound) {
			slog.Warn("resolving book for deletion", "book", id, "err", err)
		}
		// The device believes the book is gone either way; telling it otherwise
		// achieves nothing.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := store.AddTombstone(r.Context(), h.store.Writer(), device.ID, resolved); err != nil {
		slog.Error("recording on-device deletion", "device", device.ID, "book", resolved, "err", err)
	} else {
		slog.Info("device deleted a book", "device", device.ID, "book", resolved)
	}

	w.WriteHeader(http.StatusNoContent)
}
