package kobo

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/store"
)

// Handler serves the Kobo store sync API.
type Handler struct {
	store *store.Store
	urls  httpx.URLBuilder
	proxy *Proxy
	// resources is the base map for /v1/initialization, kept on disk.
	resources *resourceStore
	// seen remembers which unimplemented endpoints have already been logged.
	seen      sync.Map
	tokens    *tokenCache
	syncLocks *deviceLocks
	kepub     *kepubconv.Cache
	ebook     *ebookconv.Cache
	covers    *covers.Cache
	syncBatch int
}

type Options struct {
	Store *store.Store
	URLs  httpx.URLBuilder
	// ProxyUpstream is the Kobo store that endpoints kobibri cannot answer from
	// its own library are forwarded to. Empty, or "off", keeps every request
	// here. A store refusal never reaches the device either way.
	ProxyUpstream string
	// ResourcesFile is where the /v1/initialization map is kept. An operator can
	// put one there by hand; otherwise the store's own is saved there the first
	// time it hands one over. Empty keeps nothing and asks every time.
	ResourcesFile string
	// Kepub converts books on download. When nil, books are served as they are
	// on disk, which still reads but loses mid-chapter progress tracking.
	Kepub *kepubconv.Cache
	// Covers renders scaled cover images. When nil, every cover is a placeholder.
	Covers *covers.Cache
	// Ebook turns other formats into EPUB on the way to KEPUB.
	Ebook *ebookconv.Cache
	// SyncBatch overrides how many books one sync response covers. Zero picks
	// the default; tests use a small value to exercise the continuation path.
	SyncBatch int
}

func New(opts Options) *Handler {
	return &Handler{
		store:     opts.Store,
		urls:      opts.URLs,
		proxy:     NewProxy(opts.ProxyUpstream),
		resources: newResourceStore(opts.ResourcesFile),
		tokens:    newTokenCache(60 * time.Second),
		syncLocks: newDeviceLocks(),
		kepub:     opts.Kepub,
		ebook:     opts.Ebook,
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
	// Kobo spells this one /Items; ServeMux is case-sensitive. See kobo-protocol.md section 5.
	mux.HandleFunc("POST /kobo/{token}/v1/library/tags/{id}/Items", h.handleAddTagItems)
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

	// Shapes captured from the real store while proxying was still on, so these
	// are what the device actually expects rather than a guess. An empty object
	// is not the same thing: a paged list with no Items and no ItemCount is a
	// structure the device cannot read. See docs/kobo-protocol.md section 5.
	mux.HandleFunc("GET /kobo/{token}/v1/user/wishlist", h.proxyOr(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"TotalCountByProductType": map[string]any{},
			"Items":                   []any{},
			"ItemCount":               0,
			"TotalPageCount":          0,
			"TotalItemCount":          0,
			"CurrentPageIndex":        0,
			"ItemsPerPage":            100,
			"VersionCode":             2,
		})
	}))
	mux.HandleFunc("GET /kobo/{token}/v1/user/profile", h.proxyOr(func(w http.ResponseWriter, r *http.Request) {
		device := deviceFrom(r.Context())
		platform := r.Header.Get(hdrPlatformID)
		user := ""
		if device != nil {
			user = accountUUID(device.UserID)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"IsOneStore":              true,
			"IsChildAccount":          false,
			"CountryCode":             "US",
			"Geo":                     "US",
			"StoreFront":              "US",
			"PlatformId":              platform,
			"PartnerId":               "00000000-0000-0000-0000-000000000001",
			"AffiliateName":           "Kobo",
			"IsoCultureCode":          "en-US",
			"IsLibraryMigrated":       false,
			"VipMembershipPurchased":  false,
			"HasPurchased":            false,
			"HasPurchasedBook":        false,
			"HasPurchasedAudiobook":   false,
			"SafeSearch":              false,
			"AudiobooksEnabled":       false,
			"IsOrangeAffiliated":      false,
			"IsEligibleForOrangeDeal": false,
			"PrivacyPermissions":      []any{},
			"LinkedAccounts":          []any{},
			"UserId":                  user,
		})
	}))
	mux.HandleFunc("GET /kobo/{token}/v1/user/loyalty/benefits", h.proxyOr(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Benefits": map[string]any{}})
	}))
	mux.HandleFunc("GET /kobo/{token}/v1/deals", h.proxyOr(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Deals": []any{}})
	}))
	mux.HandleFunc("GET /kobo/{token}/v1/affiliate", h.proxyOr(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Affiliate": "Kobo"})
	}))

	// Analytics stops here rather than going upstream: the events carry shelf
	// sizes, reading minutes, storage use and the device's serial number, and
	// the resource map now points the device at us for both of them.
	mux.HandleFunc("GET /kobo/{token}/v1/analytics/gettests", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"Result": "Success", "TestKey": "", "Tests": map[string]any{},
		})
	})
	mux.HandleFunc("POST /kobo/{token}/v1/analytics/{event...}", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Result": "Success"})
	})

	// Everything else answers `200 {}`. HEAD is not
	// listed: ServeMux already serves it from the GET patterns, and registering
	// it on a broader path conflicts with the specific GET routes above.
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		mux.HandleFunc(method+" /kobo/{token}/", h.handleUnknown)
	}

	return httpx.Chain(mux,
		httpx.Recoverer(h.onPanic),
		httpx.RequestID,
		httpx.AccessLog,
		Trace,
		koboHeaders,
		h.authenticate,
	)
}

// handleLibraryPut separates the two operations that share the PUT
// /v1/library/X/Y shape: renaming a collection and reporting reading progress.
//
// A book id is always a uuid, so a first segment of "tags" can only be the
// collection endpoint. Anything else with a trailing "state" is reading
// progress; anything else at all is not ours.
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

// handleUnknown answers an endpoint kobibri does not implement.
//
// Always `200 {}` — see docs/kobo-protocol.md section 5. One line is logged the
// first time an endpoint is seen at all, so the distinct list of what a device
// wants and does not get is one grep away rather than a scroll through every
// sync.
func (h *Handler) handleUnknown(w http.ResponseWriter, r *http.Request) {
	if shape := endpointShape(r); shape != "" {
		if _, already := h.seen.LoadOrStore(shape, true); !already {
			slog.Info("new Kobo endpoint kobibri does not implement",
				"endpoint", shape, "query", redactQuery(r.URL.RawQuery),
				"proxied", h.proxy.Enabled())
		}
	}
	if h.proxy.Relay(w, r) {
		return
	}
	httpx.WriteEmptyJSON(w)
}

// proxyOr offers an endpoint to the store first and falls back to an answer of
// our own when the store is off, unreachable, or refuses.
//
// The fallback is not a consolation prize: for wishlist, profile and the rest
// it is a shape captured from the real store, which is more than the store
// itself will give us for an account it declines to authenticate.
func (h *Handler) proxyOr(fallback http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.proxy.Relay(w, r) {
			return
		}
		fallback(w, r)
	}
}

// endpointShape strips the token and collapses ids, so ten thousand book
// requests are one line rather than ten thousand.
func endpointShape(r *http.Request) string {
	path := strings.TrimPrefix(r.URL.Path, "/kobo/")
	if _, tail, ok := strings.Cut(path, "/"); ok {
		path = tail
	}

	parts := strings.Split(path, "/")
	for i, part := range parts {
		if looksLikeID(part) {
			parts[i] = "{id}"
		}
	}
	return r.Method + " /" + strings.Join(parts, "/")
}

// looksLikeID is deliberately loose: a uuid, or a long run of digits, is an
// identifier rather than a name. A short number is left alone — in a path like
// .../rating/4 it is the interesting part, not an id.
func looksLikeID(s string) bool {
	if s == "" {
		return false
	}
	digits, hex := 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
			hex++
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-':
			hex++
		}
	}
	return (digits == len(s) && len(s) >= 4) || (hex == len(s) && len(s) >= 16)
}

// onPanic keeps a bug in one handler from aborting the device's whole sync.
func (h *Handler) onPanic(w http.ResponseWriter, r *http.Request) {
	if isBinaryPath(r.URL.Path) {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	httpx.WriteEmptyJSON(w)
}

// accountUUID gives a person a stable uuid to be known by, since the device
// expects one and kobibri keys people by integer id.
func accountUUID(userID int64) string {
	return uuid.NewMD5(uuid.NameSpaceOID,
		[]byte("kobibri-user-"+strconv.FormatInt(userID, 10))).String()
}
