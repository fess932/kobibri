package kobo

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// kvUpstreamResources caches the resource map fetched from the real Kobo store.
const kvUpstreamResources = "kobo:upstream_resources"

// kvUpstreamRefusedAt is when the store last refused to hand over the map.
//
// It refuses for good once we have issued a token of our own: the device then
// sends us that token in Authorization, we forward it, and the store answers
// "Invalid token version" because it minted no such thing. Retrying on every
// initialization spends a few hundred milliseconds of the device's time and
// says the same thing every time, so a refusal is remembered.
const kvUpstreamRefusedAt = "kobo:upstream_refused_at"

// upstreamRetryAfter is how long a refusal stands. Long, because the answer
// only changes if the device is re-registered from scratch; not forever,
// because that is the sort of thing that becomes impossible to undo.
const upstreamRetryAfter = 24 * time.Hour

// upstreamResourceFloor is the smallest upstream map we will trust. Kobo's real
// response carries roughly two hundred keys; anything dramatically smaller is a
// truncated or error response, and caching it would poison every later device.
const upstreamResourceFloor = 100

// handleInitialization answers the single most dangerous response this server
// produces.
//
// The device writes every key of Resources into the [OneStoreServices] section
// of .kobo/Kobo/Kobo eReader.conf and uses those cached values from then on. A
// wrong value is not a failed request; it is a device that has to be repaired
// by hand. See docs/kobo-protocol.md §1.
//
// Strategy, in order of preference:
//
//  1. Fetch the real map from Kobo using the device's own credentials, which it
//     helpfully sends us on this very request, and cache it. This is exactly
//     what the device would have received on its own.
//  2. Use the cached map from an earlier successful fetch.
//  3. Send only the keys we override. Keys we omit are simply left at whatever
//     the device already has — which is Kobo's own endpoints — so the device is
//     never left with a half-configured store. This is what calibre-web and
//     Komga do, and it is the proven path.
//
// In every case our overrides are applied last and unconditionally.
func (h *Handler) handleInitialization(w http.ResponseWriter, r *http.Request) {
	resources, origin := h.baseResources(r)
	h.applyOverrides(resources, r)

	// Logged at INFO on purpose, and this is the one response worth a line of
	// its own: the device caches it into Kobo eReader.conf permanently, and if
	// the root here is an address it cannot reach, it simply stops after
	// initialization and asks for nothing else. That failure is silent from
	// both ends, so the address we handed out has to be in the log.
	slog.Info("answered initialization",
		"root", h.urls.Abs(r, "kobo", "<token>"),
		"image_host", h.urls.Root(r),
		"resources", len(resources),
		"base", origin)

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"Resources": resources})
}

// baseResources returns the upstream map to build on, or an empty map when we
// have none. It never returns a partial upstream response.
//
// The second return says where the map came from — cached, upstream, or nothing
// at all. It exists only to be logged: "overrides only" is a materially
// different answer to give a device than the full Kobo map, and until it was
// said out loud the difference was invisible from the log.
func (h *Handler) baseResources(r *http.Request) (map[string]any, string) {
	if cached := h.cachedResources(r.Context()); cached != nil {
		return cached, "cached"
	}
	if !h.proxy.Enabled() {
		return map[string]any{}, "overrides only (proxy off)"
	}
	if since, ok := h.upstreamRefusedWithin(r.Context()); ok {
		return map[string]any{}, "overrides only (store refused us " + since + " ago)"
	}

	fetched, err := h.proxy.FetchResources(r)
	if err != nil {
		h.rememberUpstreamRefusal(r.Context())
		// Warn rather than Debug: the device gets a smaller map because of this,
		// and someone reading the log at the default level needs to see why.
		slog.Warn("could not fetch the upstream resource map; sending overrides only",
			"err", err)
		return map[string]any{}, "overrides only (upstream failed)"
	}
	if len(fetched) < upstreamResourceFloor {
		h.rememberUpstreamRefusal(r.Context())
		slog.Warn("upstream resource map looks truncated; ignoring it",
			"keys", len(fetched), "floor", upstreamResourceFloor)
		return map[string]any{}, "overrides only (upstream truncated)"
	}

	if buf, err := json.Marshal(fetched); err == nil {
		if err := store.SetKV(r.Context(), h.store.Writer(), kvUpstreamResources, string(buf)); err != nil {
			slog.Debug("caching upstream resources", "err", err)
		}
	}
	store.SetKV(r.Context(), h.store.Writer(), kvUpstreamRefusedAt, "")
	slog.Info("cached the upstream Kobo resource map", "keys", len(fetched))
	return fetched, "upstream"
}

func (h *Handler) upstreamRefusedWithin(ctx context.Context) (string, bool) {
	raw, err := store.GetKV(ctx, h.store.Reader(), kvUpstreamRefusedAt)
	if err != nil || raw == "" {
		return "", false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", false
	}
	if since := time.Since(at); since < upstreamRetryAfter {
		return since.Round(time.Minute).String(), true
	}
	return "", false
}

func (h *Handler) rememberUpstreamRefusal(ctx context.Context) {
	if err := store.SetKV(ctx, h.store.Writer(), kvUpstreamRefusedAt,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Debug("recording an upstream refusal", "err", err)
	}
}

func (h *Handler) cachedResources(ctx context.Context) map[string]any {
	raw, err := store.GetKV(ctx, h.store.Reader(), kvUpstreamResources)
	if err != nil || raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) < upstreamResourceFloor {
		return nil
	}
	return m
}

// applyOverrides points every endpoint the device needs from us at us.
//
// The placeholder casing below is Kobo's own and is load-bearing: the device
// substitutes exactly {ImageId}, {Width}, {Height}, {Quality} and {IsGreyscale}.
// calibre-web emits lowercase names and a literal "isGreyscale", and devices
// then request that literal path. Do not copy that.
func (h *Handler) applyOverrides(res map[string]any, r *http.Request) {
	root := h.urls.Abs(r, "kobo", rawTokenFrom(r.Context()))

	res["library_sync"] = root + "/v1/library/sync"
	res["library_metadata"] = root + "/v1/library/{Ids}/metadata"
	res["library_book"] = root + "/v1/library/{LibraryItemId}"
	res["reading_state"] = root + "/v1/library/{Ids}/state"
	res["delete_entitlement"] = root + "/v1/library/{Ids}"

	res["tags"] = root + "/v1/library/tags"
	res["tag_items"] = root + "/v1/library/tags/{TagId}/items"
	res["delete_tag"] = root + "/v1/library/tags/{TagId}"
	res["rename_tag"] = root + "/v1/library/tags/{TagId}"
	res["delete_tag_items"] = root + "/v1/library/tags/{TagId}/items/delete"

	// Covers live on their own host in Kobo's map, so they need their own root
	// rather than being left to inherit the API endpoint.
	covers := root + "/covers"
	res["image_host"] = h.urls.Root(r)
	res["image_url_template"] = covers + "/{ImageId}/{Width}/{Height}/{IsGreyscale}/image.jpg"
	res["image_url_quality_template"] = covers + "/{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}/image.jpg"
}

// OverriddenResourceKeys is the exact set applyOverrides writes. Tests assert
// against it so a key can never be dropped silently.
var OverriddenResourceKeys = []string{
	"library_sync",
	"library_metadata",
	"library_book",
	"reading_state",
	"delete_entitlement",
	"tags",
	"tag_items",
	"delete_tag",
	"rename_tag",
	"delete_tag_items",
	"image_host",
	"image_url_template",
	"image_url_quality_template",
}
