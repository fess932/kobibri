package kobo

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// tagRequest is what the device posts when it creates or renames a collection.
type tagRequest struct {
	Name  string    `json:"Name"`
	Items []TagItem `json:"Items"`
}

// itemsRequest is what the device posts to add or remove members.
type itemsRequest struct {
	Items []TagItem `json:"Items"`
}

func (r itemsRequest) bookIDs() []string {
	out := make([]string, 0, len(r.Items))
	for _, item := range r.Items {
		if item.RevisionID != "" {
			out = append(out, item.RevisionID)
		}
	}
	return out
}

// handleCreateTag serves POST /v1/library/tags.
//
// The response is the new collection's id as a bare JSON string — not an
// object. The device parses it as a string and nothing else will do.
func (h *Handler) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	device := deviceFrom(r.Context())
	if device == nil {
		httpx.WriteEmptyJSON(w)
		return
	}

	var req tagRequest
	if err := httpx.DecodeJSON(r, 1<<20, &req); err != nil || req.Name == "" {
		slog.Debug("unusable create-collection request", "err", err)
		httpx.WriteEmptyJSON(w)
		return
	}

	var tagID string
	err := h.store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		tagID, err = store.CreateTag(r.Context(), tx, device.UserID, req.Name, store.TagOriginDevice)
		if err != nil {
			return err
		}
		return store.AddTagItems(r.Context(), tx, tagID, itemsRequest{Items: req.Items}.bookIDs())
	})
	if err != nil {
		slog.Error("creating collection", "name", req.Name, "err", err)
		httpx.WriteEmptyJSON(w)
		return
	}

	slog.Info("device created a collection", "device", device.ID, "name", req.Name, "id", tagID)
	writeBareString(w, http.StatusCreated, tagID)
}

// handleRenameTag serves PUT /v1/library/tags/{id}.
func (h *Handler) handleRenameTag(w http.ResponseWriter, r *http.Request) {
	tagID := r.PathValue("id")

	var req tagRequest
	if err := httpx.DecodeJSON(r, 1<<20, &req); err != nil || req.Name == "" {
		httpx.WriteEmptyJSON(w)
		return
	}
	if !h.ownsTag(r, tagID) {
		httpx.WriteEmptyJSON(w)
		return
	}

	if err := store.RenameTag(r.Context(), h.store.Writer(), tagID, req.Name); err != nil {
		slog.Debug("renaming collection", "tag", tagID, "err", err)
	}
	httpx.WriteEmptyJSON(w)
}

// handleDeleteTag serves DELETE /v1/library/tags/{id}.
func (h *Handler) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	tagID := r.PathValue("id")
	if h.ownsTag(r, tagID) {
		if err := store.DeleteTag(r.Context(), h.store.Writer(), tagID); err != nil {
			slog.Debug("deleting collection", "tag", tagID, "err", err)
		}
	}
	httpx.WriteEmptyJSON(w)
}

// handleAddTagItems serves POST /v1/library/tags/{id}/items.
func (h *Handler) handleAddTagItems(w http.ResponseWriter, r *http.Request) {
	tagID := r.PathValue("id")

	var req itemsRequest
	if err := httpx.DecodeJSON(r, 4<<20, &req); err != nil {
		httpx.WriteEmptyJSON(w)
		return
	}
	if !h.ownsTag(r, tagID) {
		httpx.WriteEmptyJSON(w)
		return
	}

	err := h.store.Tx(r.Context(), func(tx *sql.Tx) error {
		return store.AddTagItems(r.Context(), tx, tagID, req.bookIDs())
	})
	if err != nil {
		slog.Error("adding books to a collection", "tag", tagID, "err", err)
	}

	w.Header().Set("Content-Type", httpx.ContentTypeJSON)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte("{}"))
}

// handleRemoveTagItems serves POST /v1/library/tags/{id}/items/delete.
//
// Removal is a POST rather than a DELETE because that is what the device sends.
func (h *Handler) handleRemoveTagItems(w http.ResponseWriter, r *http.Request) {
	tagID := r.PathValue("id")

	var req itemsRequest
	if err := httpx.DecodeJSON(r, 4<<20, &req); err != nil {
		httpx.WriteEmptyJSON(w)
		return
	}
	if !h.ownsTag(r, tagID) {
		httpx.WriteEmptyJSON(w)
		return
	}

	err := h.store.Tx(r.Context(), func(tx *sql.Tx) error {
		return store.RemoveTagItems(r.Context(), tx, tagID, req.bookIDs())
	})
	if err != nil {
		slog.Error("removing books from a collection", "tag", tagID, "err", err)
	}
	httpx.WriteEmptyJSON(w)
}

// ownsTag checks the collection belongs to the requesting device's user, so one
// user's device cannot rename or empty another's collection.
func (h *Handler) ownsTag(r *http.Request, tagID string) bool {
	device := deviceFrom(r.Context())
	if device == nil || tagID == "" {
		return false
	}
	tag, err := store.GetTag(r.Context(), h.store.Reader(), tagID)
	if err != nil {
		return false
	}
	return tag.UserID == device.UserID
}

// writeBareString sends a JSON string on its own, which is what the collection
// creation endpoint returns.
func writeBareString(w http.ResponseWriter, status int, value string) {
	buf, err := json.Marshal(value)
	if err != nil {
		httpx.WriteEmptyJSON(w)
		return
	}
	w.Header().Set("Content-Type", httpx.ContentTypeJSON)
	w.WriteHeader(status)
	w.Write(buf)
}
