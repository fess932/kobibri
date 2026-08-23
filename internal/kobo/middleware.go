// Package kobo implements the Kobo store sync API a device talks to.
//
// The governing rule of this package: the device is the fragile party. Every
// failure mode answers `200 {}` rather than an error status, because an error
// on any endpoint — even an incidental one — makes the device abandon the whole
// sync. See docs/kobo-protocol.md.
package kobo

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

type ctxKey int

const (
	tokenKey ctxKey = iota
	rawTokenKey
	deviceKey
)

// pathToken pulls the secret out of /kobo/<token>/...
//
// It is read from the URL rather than via r.PathValue because authentication
// runs as middleware, ahead of the mux: PathValue is only populated once a
// route has matched, so it is empty here and every request would be rejected.
func pathToken(path string) string {
	const prefix = "/kobo/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	token, _, _ := strings.Cut(path[len(prefix):], "/")
	return token
}

// rawTokenFrom returns the secret as it appeared in the request path, for
// rebuilding URLs that point back at us.
func rawTokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(rawTokenKey).(string)
	return t
}

// Device headers a Kobo sends on every request.
const (
	hdrDeviceID    = "x-kobo-deviceid"
	hdrDeviceModel = "x-kobo-devicemodel"
	hdrAppVersion  = "x-kobo-appversion"
	hdrSerial      = "x-kobo-serialnumber"
	hdrSyncToken   = "x-kobo-synctoken"
	hdrSync        = "x-kobo-sync"
	hdrAPIToken    = "x-kobo-apitoken"
)

// tokenFrom returns the authenticated token for a request.
func tokenFrom(ctx context.Context) *store.APIToken {
	t, _ := ctx.Value(tokenKey).(*store.APIToken)
	return t
}

// deviceFrom returns the device that sent a request.
func deviceFrom(ctx context.Context) *store.Device {
	d, _ := ctx.Value(deviceKey).(*store.Device)
	return d
}

// tokenCache keeps recent token lookups out of the database. Devices poll
// often, and every request carries the token in its path.
type tokenCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	items map[string]tokenCacheEntry
}

type tokenCacheEntry struct {
	token   *store.APIToken
	expires time.Time
}

func newTokenCache(ttl time.Duration) *tokenCache {
	return &tokenCache{ttl: ttl, items: map[string]tokenCacheEntry{}}
}

func (c *tokenCache) get(raw string) (*store.APIToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[raw]
	if !ok || time.Now().After(e.expires) {
		delete(c.items, raw)
		return nil, false
	}
	return e.token, true
}

func (c *tokenCache) put(raw string, t *store.APIToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) > 1024 {
		clear(c.items)
	}
	c.items[raw] = tokenCacheEntry{token: t, expires: time.Now().Add(c.ttl)}
}

// Invalidate drops a cached token, so revoking one takes effect immediately.
func (c *tokenCache) Invalidate(raw string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, raw)
}

// authenticate resolves the token in the request path and the device that sent
// the request, attaching both to the context.
//
// An unknown token answers `200 {}` for API paths rather than 401: a device
// whose token was revoked should go quiet, not abort mid-sync with an error the
// user cannot interpret. Binary endpoints (downloads, covers) answer 404, since
// they are not part of the sync conversation.
func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := pathToken(r.URL.Path)
		if raw == "" {
			h.deny(w, r)
			return
		}

		tok, cached := h.tokens.get(raw)
		if !cached {
			var err error
			tok, err = store.LookupAPIToken(r.Context(), h.store.Reader(), raw)
			if err != nil {
				slog.Debug("rejected kobo request with an unrecognised token",
					"path", httpx.RedactPath(r.URL.Path), "err", err)
				h.deny(w, r)
				return
			}
			h.tokens.put(raw, tok)
			// Recording last use is best-effort; it must never fail a request.
			if err := store.TouchAPIToken(r.Context(), h.store.Writer(), tok.TokenHash); err != nil {
				slog.Debug("recording token use", "err", err)
			}
		}

		device, err := store.UpsertDevice(r.Context(), h.store.Writer(), store.DeviceIdentity{
			TokenHash:    tok.TokenHash,
			UserID:       tok.UserID,
			KoboDeviceID: r.Header.Get(hdrDeviceID),
			Model:        r.Header.Get(hdrDeviceModel),
			Serial:       r.Header.Get(hdrSerial),
			Firmware:     r.Header.Get(hdrAppVersion),
			UserAgent:    r.UserAgent(),
		})
		if err != nil {
			slog.Error("recording device", "err", err)
			h.deny(w, r)
			return
		}

		// Debug rather than Info: a reader checks in every few minutes and this
		// would bury everything else. But it has to exist — only the endpoints
		// kobibri does *not* implement were being logged, so a device that
		// asked for one it does and got nowhere left no trace whatsoever.
		// KOBIBRI_LOG_LEVEL=debug is what turns the trace on.
		slog.Debug("kobo request",
			"method", r.Method,
			"path", httpx.RedactPath(r.URL.Path),
			"device", device.ID,
			"device_id", device.KoboDeviceID != "",
			"firmware", device.Firmware)

		ctx := context.WithValue(r.Context(), tokenKey, tok)
		ctx = context.WithValue(ctx, rawTokenKey, raw)
		ctx = context.WithValue(ctx, deviceKey, device)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// deny answers a request we will not serve, in the least damaging way for the
// path in question.
func (h *Handler) deny(w http.ResponseWriter, r *http.Request) {
	if isBinaryPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	httpx.WriteEmptyJSON(w)
}

func isBinaryPath(path string) bool {
	return strings.Contains(path, "/download/") || strings.Contains(path, "/covers/")
}

// koboHeaders stamps the response header every Kobo endpoint carries.
func koboHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// base64 of "{}". Both calibre-web and Komga send it on every response
		// under the Kobo API; the device expects the header to be present.
		w.Header().Set(hdrAPIToken, "e30=")
		next.ServeHTTP(w, r)
	})
}
