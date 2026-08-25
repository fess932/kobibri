package kobo

import (
	"log/slog"
	"net/http"
	"reflect"
	"strings"

	"github.com/fess932/kobibri/internal/httpx"
)

// handleInitialization answers the single most dangerous response this server
// produces.
//
// The device writes every key of Resources into the [OneStoreServices] section
// of .kobo/Kobo/Kobo eReader.conf and uses those cached values from then on. A
// wrong value is not a failed request; it is a device that has to be repaired
// by hand. See docs/NOTES.md.
func (h *Handler) handleInitialization(w http.ResponseWriter, r *http.Request) {
	resources, base := h.resources.Base(h.proxy, r)
	h.applyOverrides(resources, r)

	slog.Info("answered initialization",
		"root", h.urls.Abs(r, "kobo", "<token>"),
		"image_host", resources["image_host"],
		"resources", len(resources),
		"base", base)

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"Resources": resources})
}

// resourceOverrides is the set of keys this server claims. A field belongs here
// when there is a route behind it and nowhere else when there is not.
//
// Leaving a key unclaimed used to be harmless: the base map was empty and the
// device fell back to api_endpoint. Now that a full map ships, an unclaimed key
// is an explicit instruction to send reading progress and shelves to
// storeapi.kobo.com instead of here.
//
// library_book is absent for the opposite reason: /v1/user/library/books/
// {LibraryItemId} has no route, and claiming it advertised a path this server
// answers with nothing.
//
// The two analytics keys are claimed for a reason of their own. The events the
// device posts carry shelf sizes, reading minutes, storage use and its serial
// number; leaving those keys at Kobo's meant the reader sent all of it to the
// shop directly, whatever this server did or did not forward.
//
// The placeholder casing is Kobo's own and is load-bearing: the device
// substitutes exactly {ImageId}, {Width}, {Height}, {Quality} and {IsGreyscale}.
// calibre-web emits lowercase names and a literal "isGreyscale", and devices
// then request that literal path. Do not copy that. See docs/NOTES.md
// section 1.
type resourceOverrides struct {
	LibrarySync       string `json:"library_sync"`
	LibraryMetadata   string `json:"library_metadata"`
	ReadingState      string `json:"reading_state"`
	DeleteEntitlement string `json:"delete_entitlement"`

	PostAnalyticsEvent string `json:"post_analytics_event"`
	GetTestsRequest    string `json:"get_tests_request"`

	Tags           string `json:"tags"`
	TagItems       string `json:"tag_items"`
	DeleteTag      string `json:"delete_tag"`
	RenameTag      string `json:"rename_tag"`
	DeleteTagItems string `json:"delete_tag_items"`

	ImageHost               string `json:"image_host"`
	ImageURLTemplate        string `json:"image_url_template"`
	ImageURLQualityTemplate string `json:"image_url_quality_template"`
}

func (h *Handler) overridesFor(r *http.Request) resourceOverrides {
	root := h.urls.Abs(r, "kobo", rawTokenFrom(r.Context()))
	covers := root + "/covers/"

	return resourceOverrides{
		LibrarySync:       root + "/v1/library/sync",
		LibraryMetadata:   root + "/v1/library/{Ids}/metadata",
		ReadingState:      root + "/v1/library/{Ids}/state",
		DeleteEntitlement: root + "/v1/library/{Ids}",

		PostAnalyticsEvent: root + "/v1/analytics/event",
		GetTestsRequest:    root + "/v1/analytics/gettests",

		Tags:           root + "/v1/library/tags",
		TagItems:       root + "/v1/library/tags/{TagId}/items",
		DeleteTag:      root + "/v1/library/tags/{TagId}",
		RenameTag:      root + "/v1/library/tags/{TagId}",
		DeleteTagItems: root + "/v1/library/tags/{TagId}/items/delete",

		ImageHost:               covers,
		ImageURLTemplate:        covers + "{ImageId}/{Width}/{Height}/{IsGreyscale}/image.jpg",
		ImageURLQualityTemplate: covers + "{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}/image.jpg",
	}
}

// into writes the overrides over whatever base map won, last and
// unconditionally.
func (o resourceOverrides) into(res map[string]any) {
	v := reflect.ValueOf(o)
	for i, t := 0, v.Type(); i < t.NumField(); i++ {
		res[resourceKey(t.Field(i))] = v.Field(i).String()
	}
}

func (h *Handler) applyOverrides(res map[string]any, r *http.Request) {
	h.overridesFor(r).into(res)
}

// OverriddenResourceKeys is the exact set applyOverrides writes, read off the
// struct so the list cannot drift from the code that fills it.
var OverriddenResourceKeys = overriddenKeys()

func overriddenKeys() []string {
	t := reflect.TypeOf(resourceOverrides{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, resourceKey(t.Field(i)))
	}
	return out
}

func resourceKey(f reflect.StructField) string {
	key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if key == "" {
		return f.Name
	}
	return key
}
