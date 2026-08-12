package kobo

import (
	"net/http"
	"time"

	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// Handler serves the Kobo store sync API.
type Handler struct {
	store     *store.Store
	urls      httpx.URLBuilder
	proxy     *Proxy
	tokens    *tokenCache
	syncLocks *deviceLocks
	kepub     *kepubconv.Cache
	covers    *covers.Cache
	syncBatch int
}

type Options struct {
	Store *store.Store
	URLs  httpx.URLBuilder
	// ProxyUpstream is the Kobo store unknown endpoints are forwarded to. Empty
	// disables proxying, in which case those endpoints answer `200 {}`.
	ProxyUpstream string
	// Kepub converts books on download. When nil, books are served as they are
	// on disk, which still reads but loses mid-chapter progress tracking.
	Kepub *kepubconv.Cache
	// Covers renders scaled cover images. When nil, every cover is a placeholder.
	Covers *covers.Cache
	// SyncBatch overrides how many books one sync response covers. Zero picks
	// the default; tests use a small value to exercise the continuation path.
	SyncBatch int
}

func New(opts Options) *Handler {
	return &Handler{
		store:     opts.Store,
		urls:      opts.URLs,
		proxy:     NewProxy(opts.ProxyUpstream),
		tokens:    newTokenCache(60 * time.Second),
		syncLocks: newDeviceLocks(),
		kepub:     opts.Kepub,
		covers:    opts.Covers,
		syncBatch: opts.SyncBatch,
	}
}

// InvalidateToken drops a revoked token from the cache so it stops working at
// once rather than after the cache TTL.
func (h *Handler) InvalidateToken(raw string) { h.tokens.Invalidate(raw) }

// Mount returns the handler for everything under /kobo/.
//
// Route precedence is stdlib ServeMux's: a literal path segment beats a
// wildcard. That is what keeps `/v1/library/tags` from being swallowed by
// `/v1/library/{uuid}` — a real hazard, since the latter is the book deletion
// endpoint.
func (h *Handler) Mount() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /kobo/{token}/v1/auth/device", h.handleAuth)
	mux.HandleFunc("POST /kobo/{token}/v1/auth/refresh", h.handleAuth)
	mux.HandleFunc("GET /kobo/{token}/v1/initialization", h.handleInitialization)

	mux.HandleFunc("GET /kobo/{token}/v1/library/sync", h.handleSync)
	mux.HandleFunc("GET /kobo/{token}/v1/library/{uuid}/metadata", h.handleMetadata)
	mux.HandleFunc("GET /kobo/{token}/v1/library/{uuid}/state", h.handleGetState)

	// PUT overloads one path shape for two unrelated operations: renaming a
	// collection is PUT /v1/library/tags/{id}, and reporting reading progress is
	// PUT /v1/library/{uuid}/state. Both are /v1/library/X/Y, so no routing
	// table can separate them — ServeMux refuses the ambiguity outright. One
	// handler takes both and dispatches.
	mux.HandleFunc("PUT /kobo/{token}/v1/library/{a}/{b}", h.handleLibraryPut)

	// The device sends this when the user deletes a book on it. The literal
	// `tags` routes registered later take precedence over this wildcard, which
	// is what keeps collection deletion from being read as book deletion.
	mux.HandleFunc("DELETE /kobo/{token}/v1/library/{uuid}", h.handleDeleteBook)

	// Guard: without this, `DELETE /v1/library/tags` would match the book
	// deletion route above and be misread as deleting a book called "tags".
	mux.HandleFunc("DELETE /kobo/{token}/v1/library/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	// Collections. Removal of members is a POST, not a DELETE, because that is
	// what the device sends.
	mux.HandleFunc("POST /kobo/{token}/v1/library/tags", h.handleCreateTag)

	mux.HandleFunc("DELETE /kobo/{token}/v1/library/tags/{id}", h.handleDeleteTag)
	mux.HandleFunc("POST /kobo/{token}/v1/library/tags/{id}/items", h.handleAddTagItems)
	mux.HandleFunc("POST /kobo/{token}/v1/library/tags/{id}/items/delete", h.handleRemoveTagItems)

	mux.HandleFunc("GET /kobo/{token}/download/{uuid}/{format}", h.handleDownload)

	// Both cover URL shapes from the resource templates. The five-segment form
	// omits Quality; the device uses whichever template it was handed.
	mux.HandleFunc("GET /kobo/{token}/covers/{imageId}/{width}/{height}/{greyscale}/image.jpg", h.handleCover)
	mux.HandleFunc("GET /kobo/{token}/covers/{imageId}/{width}/{height}/{quality}/{greyscale}/image.jpg", h.handleCover)

	// The bare api_endpoint root: the device probes it and expects an object.
	mux.HandleFunc("GET /kobo/{token}", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteEmptyJSON(w)
	})
	mux.HandleFunc("GET /kobo/{token}/", h.handleUnknown)

	// Answering these ourselves keeps a device that cannot reach the store from
	// stalling on telemetry it does not need.
	mux.HandleFunc("GET /kobo/{token}/v1/analytics/gettests", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"Result": "Success", "TestKey": "", "Tests": map[string]any{},
		})
	})
	mux.HandleFunc("POST /kobo/{token}/v1/analytics/{event...}", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Result": "Success"})
	})

	// Everything else falls through to the proxy, or to `200 {}`. HEAD is not
	// listed: ServeMux already serves it from the GET patterns, and registering
	// it on a broader path conflicts with the specific GET routes above.
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		mux.HandleFunc(method+" /kobo/{token}/", h.handleUnknown)
	}

	return httpx.Chain(mux,
		httpx.Recoverer(h.onPanic),
		httpx.RequestID,
		httpx.AccessLog,
		koboHeaders,
		h.authenticate,
	)
}

// handleLibraryPut separates the two operations that share the PUT
// /v1/library/X/Y shape: renaming a collection and reporting reading progress.
//
// A book id is always a uuid, so a first segment of "tags" can only be the
// collection endpoint. Anything else with a trailing "state" is reading
// progress; anything else at all is not ours and goes to the proxy.
func (h *Handler) handleLibraryPut(w http.ResponseWriter, r *http.Request) {
	a, b := r.PathValue("a"), r.PathValue("b")

	switch {
	case a == "tags":
		r.SetPathValue("id", b)
		h.handleRenameTag(w, r)
	case b == "state":
		r.SetPathValue("uuid", a)
		h.handlePutState(w, r)
	default:
		h.handleUnknown(w, r)
	}
}

// handleUnknown serves an endpoint kobibri does not implement.
func (h *Handler) handleUnknown(w http.ResponseWriter, r *http.Request) {
	h.proxy.Handle(w, r)
}

// onPanic keeps a bug in one handler from aborting the device's whole sync.
func (h *Handler) onPanic(w http.ResponseWriter, r *http.Request) {
	if isBinaryPath(r.URL.Path) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.WriteEmptyJSON(w)
}
