package kobo

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/store"
)

// kvUpstreamResources caches the resource map fetched from the real Kobo store.
const kvUpstreamResources = "kobo:upstream_resources"

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
	resources := h.baseResources(r)
	h.applyOverrides(resources, r)

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"Resources": resources})
}

// baseResources returns the upstream map to build on, or an empty map when we
// have none. It never returns a partial upstream response.
func (h *Handler) baseResources(r *http.Request) map[string]any {
	if cached := h.cachedResources(r.Context()); cached != nil {
		return cached
	}
	if !h.proxy.Enabled() {
		return map[string]any{}
	}

	fetched, err := h.proxy.FetchResources(r)
	if err != nil {
		slog.Debug("could not fetch upstream resources; sending overrides only", "err", err)
		return map[string]any{}
	}
	if len(fetched) < upstreamResourceFloor {
		slog.Warn("upstream resource map looks truncated; ignoring it",
			"keys", len(fetched), "floor", upstreamResourceFloor)
		return map[string]any{}
	}

	if buf, err := json.Marshal(fetched); err == nil {
		if err := store.SetKV(r.Context(), h.store.Writer(), kvUpstreamResources, string(buf)); err != nil {
			slog.Debug("caching upstream resources", "err", err)
		}
	}
	slog.Info("cached the upstream Kobo resource map", "keys", len(fetched))
	return fetched
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
