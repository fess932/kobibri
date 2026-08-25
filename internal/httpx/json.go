package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ContentTypeJSON is the exact value Kobo devices expect. calibre-web goes out
// of its way to bypass Flask's jsonify to get this, noting that the device
// mishandles the encoding otherwise. See docs/NOTES.md.
const ContentTypeJSON = "application/json; charset=utf-8"

// WriteJSON writes a JSON response with the Kobo-compatible content type.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		slog.Error("encoding json response", "err", err)
		WriteEmptyJSON(w)
		return
	}
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// WriteEmptyJSON answers `200 {}`.
//
// This is the failure mode for everything under the Kobo API: an error on an
// incidental endpoint makes the device abort the entire sync, so a 4xx or 5xx
// is far more damaging than an empty success. See docs/NOTES.md.
func WriteEmptyJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// DecodeJSON reads a request body, capping it so a malformed device cannot
// exhaust memory.
func DecodeJSON(r *http.Request, limit int64, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit))
	return dec.Decode(v)
}
