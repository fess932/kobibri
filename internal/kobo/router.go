package kobo

import (
	"net/http"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// Handler serves the Kobo store sync API.
type Handler struct {
	store     *store.Store
	urls      httpx.URLBuilder
	proxy     *Proxy
	tokens    *tokenCache
	syncLocks *deviceLocks
}

type Options struct {
	Store *store.Store
	URLs  httpx.URLBuilder
	// ProxyUpstream is the Kobo store unknown endpoints are forwarded to. Empty
	// disables proxying, in which case those endpoints answer `200 {}`.
	ProxyUpstream string
}

func New(opts Options) *Handler {
	return &Handler{
		store:     opts.Store,
		urls:      opts.URLs,
		proxy:     NewProxy(opts.ProxyUpstream),
		tokens:    newTokenCache(60 * time.Second),
		syncLocks: newDeviceLocks(),
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
