package kobo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/kobo"
	"github.com/fess932/kobibri/internal/store"
)

type env struct {
	t       *testing.T
	store   *store.Store
	server  *httptest.Server
	handler *kobo.Handler
	token   string
	userID  int64
	dbPath  string
	ctx     context.Context
}

// envOptions tunes the server under test.
type envOptions struct {
	// SyncBatch keeps a test's fixture small while still exercising the
	// continuation path.
	SyncBatch int
	// ProxyUpstream stands in for the Kobo store. Empty keeps every request in
	// the handler, which is what most tests want.
	ProxyUpstream string
	// ResourcesFile is where the /v1/initialization map is read from and saved
	// to. Empty keeps none, which is what most tests want.
	ResourcesFile string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, envOptions{})
}

func newEnvWith(t *testing.T, opts envOptions) *env {
	t.Helper()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "kobibri.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userID, err := store.CreateUser(ctx, st.Writer(), "reader", "x", true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.CreateAPIToken(ctx, st.Writer(), userID, "kobo clara")
	if err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	kepubCache, err := kepubconv.NewCache(kepubconv.Options{
		Dir: filepath.Join(cacheDir, "kepub"), Store: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	coverCache, err := covers.NewCache(filepath.Join(cacheDir, "covers"), st)
	if err != nil {
		t.Fatal(err)
	}

	h := kobo.New(kobo.Options{
		Store:         st,
		URLs:          httpx.URLBuilder{ListenPort: "8078"},
		Kepub:         kepubCache,
		Covers:        coverCache,
		SyncBatch:     opts.SyncBatch,
		ProxyUpstream: opts.ProxyUpstream,
		ResourcesFile: opts.ResourcesFile,
	})
	srv := httptest.NewServer(h.Mount())
	t.Cleanup(srv.Close)

	return &env{t: t, store: st, server: srv, handler: h, token: raw, userID: userID,
		dbPath: dbPath, ctx: ctx}
}

// do sends a request the way a Kobo would.
func (e *env) do(method, path string, body string) *http.Response {
	e.t.Helper()
	return e.doAsDevice("device-abc", method, path, body)
}

// doAsDevice is the same request from a named device, for tests with more than
// one: the device id is what separates two readers sharing a token.
func (e *env) doAsDevice(deviceID, method, path string, body string) *http.Response {
	e.t.Helper()

	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-kobo-deviceid", deviceID)
	req.Header.Set("x-kobo-devicemodel", "Kobo Clara 2E")
	req.Header.Set("x-kobo-appversion", "4.38.23171")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Linux; U; Android 2.0; en-us;) AppleWebKit/538.1 (Kobo Touch 0373/4.38.23171)")

	// A Kobo device does not follow redirects when we want to observe them.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (e *env) kobo(path string) string { return "/kobo/" + e.token + path }

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	return v
}

func TestAuthReturnsUsableTokens(t *testing.T) {
	e := newEnv(t)

	resp := e.do("POST", e.kobo("/v1/auth/device"),
		`{"UserKey":"abc123","DeviceId":"device-abc","AppVersion":"4.38.23171"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decode[map[string]any](t, resp)
	for _, k := range []string{"AccessToken", "RefreshToken", "TokenType", "TrackingId", "UserKey"} {
		if v, ok := body[k]; !ok || v == "" {
			t.Errorf("missing or empty %q in %v", k, body)
		}
	}
	if body["TokenType"] != "Bearer" {
		t.Errorf("TokenType = %v, want Bearer", body["TokenType"])
	}
	if body["UserKey"] != "abc123" {
		t.Errorf("UserKey = %v, want it echoed back", body["UserKey"])
	}
}

// Every response under the Kobo API carries this header.
func TestAPITokenHeaderIsAlwaysSet(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/v1/initialization", "/v1/analytics/gettests", "/v1/whatever"} {
		resp := e.do("GET", e.kobo(path), "")
		if got := resp.Header.Get("x-kobo-apitoken"); got != "e30=" {
			t.Errorf("%s: x-kobo-apitoken = %q, want %q", path, got, "e30=")
		}
	}
}

// resourcesOf reads the initialization map. Two of Kobo's own values are objects
// rather than strings (blackstone_header, free_books_page), so map[string]string
// does not decode a real map at all.
func resourcesOf(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	body := decode[struct {
		Resources map[string]any `json:"Resources"`
	}](t, resp)
	return body.Resources
}

func resourceString(t *testing.T, res map[string]any, key string) string {
	t.Helper()
	v, ok := res[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want a string", key, res[key])
	}
	return v
}

// The initialization response is cached permanently in the device's config
// file. Every key we promise must be present and must point at us.
func TestInitializationOverridesEveryKey(t *testing.T) {
	e := newEnv(t)

	resp := e.do("GET", e.kobo("/v1/initialization"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != httpx.ContentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, httpx.ContentTypeJSON)
	}

	res := resourcesOf(t, resp)

	for _, key := range kobo.OverriddenResourceKeys {
		if _, ok := res[key]; !ok {
			t.Errorf("missing overridden key %q", key)
			continue
		}
		v := resourceString(t, res, key)
		if !strings.HasPrefix(v, e.server.URL) {
			t.Errorf("%s = %q, does not point at this server", key, v)
		}
		if strings.Contains(v, "storeapi.kobo.com") || strings.Contains(v, "cdn.kobo.com") {
			t.Errorf("%s = %q, still points at Kobo", key, v)
		}
	}
}

// The device's analytics carry shelf sizes, reading minutes, storage use and a
// serial number. Not forwarding them upstream is worth nothing while the map
// still tells the reader to post them to Kobo itself.
func TestAnalyticsStaysHere(t *testing.T) {
	var upstream []string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/analytics/") {
			upstream = append(upstream, r.Method+" "+r.URL.Path)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Result": "Success"})
	}))
	defer store.Close()

	e := newEnvWith(t, envOptions{ProxyUpstream: store.URL})
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	for _, key := range []string{"post_analytics_event", "get_tests_request"} {
		if v := resourceString(t, res, key); !strings.HasPrefix(v, e.server.URL) {
			t.Errorf("%s = %q, the device would post its telemetry to Kobo", key, v)
		}
	}

	e.do("POST", e.kobo("/v1/analytics/event"), `{"Events":[{"EventType":"UserMetadataUpdate"}]}`)
	e.do("GET", e.kobo("/v1/analytics/gettests"), "")

	if len(upstream) != 0 {
		t.Errorf("analytics reached the store: %v", upstream)
	}
}

// Every key the server has a route for must name this server, or the built-in
// map hands the device storeapi.kobo.com for reading progress and shelves.
func TestNoServedEndpointIsLeftPointingAtKobo(t *testing.T) {
	e := newEnv(t)
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	for _, key := range []string{
		"library_sync", "library_metadata", "reading_state", "delete_entitlement",
		"post_analytics_event", "get_tests_request",
		"tags", "tag_items", "delete_tag", "rename_tag", "delete_tag_items",
		"image_host", "image_url_template", "image_url_quality_template",
	} {
		v := resourceString(t, res, key)
		if strings.Contains(v, "kobo.com") {
			t.Errorf("%s = %q, still Kobo's while this server answers that path", key, v)
		}
	}

	// The other side of the rule: no route, so the key stays Kobo's rather than
	// advertising a path this server answers with nothing.
	if got := resourceString(t, res, "library_book"); !strings.Contains(got, "storeapi.kobo.com") {
		t.Errorf("library_book = %q, want Kobo's own: there is no route behind it", got)
	}
}

// With no file and no store, the map that ships with the binary is what a device
// gets: a real capture, not the four keys we override.
func TestBuiltInMapIsTheDefault(t *testing.T) {
	e := newEnv(t)
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	if len(res) < 100 {
		t.Fatalf("served %d keys, want the built-in map", len(res))
	}
	if got := resourceString(t, res, "store_host"); got != "www.kobo.com" {
		t.Errorf("store_host = %q, want Kobo's own", got)
	}
	if _, ok := res["blackstone_header"].(map[string]any); !ok {
		t.Errorf("blackstone_header = %#v, want the object Kobo sends", res["blackstone_header"])
	}
	for _, key := range kobo.OverriddenResourceKeys {
		if v := resourceString(t, res, key); !strings.HasPrefix(v, e.server.URL) {
			t.Errorf("%s = %q, the built-in map won over our override", key, v)
		}
	}
}

// All three image keys have to name one place. Kobo's own image_host is the
// literal prefix of its templates -- "//cdn.kobo.com/book-images/" against
// "https://cdn.kobo.com/book-images/{ImageId}/..." -- and a firmware that builds
// a cover URL from image_host rather than a template has to land on the same
// handler, token and all.
func TestImageKeysShareOneTokenScopedPrefix(t *testing.T) {
	e := newEnv(t)
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	host := resourceString(t, res, "image_host")
	want := e.server.URL + e.kobo("/covers/")
	if host != want {
		t.Errorf("image_host = %q, want %q", host, want)
	}
	if !strings.HasSuffix(host, "/") {
		t.Errorf("image_host = %q, must end in a slash: Kobo's own does and the device appends to it", host)
	}

	for _, key := range []string{"image_url_template", "image_url_quality_template"} {
		if got := resourceString(t, res, key); !strings.HasPrefix(got, host) {
			t.Errorf("%s = %q, does not start with image_host %q", key, got, host)
		}
	}
}

// Endpoints kobibri has no library answer for go to the store first, every
// method relayed here rather than redirected: a 307 sends the device off to
// storeapi and the response never passes through to be logged.
func TestProxyRelaysToTheStore(t *testing.T) {
	var seen []string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Deals":["real"]}`))
	}))
	defer store.Close()

	e := newEnvWith(t, envOptions{ProxyUpstream: store.URL})

	resp := e.do("GET", e.kobo("/v1/deals"), "")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the device was redirected to %q instead of being served", loc)
	}
	if string(body) != `{"Deals":["real"]}` {
		t.Errorf("body = %q, want the store's own answer", body)
	}
	if len(seen) != 1 || seen[0] != "GET /v1/deals" {
		t.Errorf("the store saw %v, want one GET /v1/deals", seen)
	}
}

// The store refuses everything that validates the access token, because that
// token is one kobibri minted: GET /v1/affiliate answers 400 "Invalid token
// version" on every sync. Handing that 400 to the device made a Kobo Libra
// Colour (fw 4.45.23697) log FailedSync reason=WebRequestErr and throw away a
// library sync that had just succeeded. The refusal must stop here.
func TestStoreRefusalNeverReachesTheDevice(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ResponseStatus":{"ErrorCode":"ArgumentException","Message":"Invalid token version."}}`))
	}))
	defer store.Close()

	e := newEnvWith(t, envOptions{ProxyUpstream: store.URL})

	for _, tc := range []struct{ path, want string }{
		{"/v1/affiliate", `{"Affiliate":"Kobo"}`},
		{"/v1/deals", `{"Deals":[]}`},
		{"/v1/user/loyalty/benefits", `{"Benefits":{}}`},
		{"/v1/products/featured", "{}"},
	} {
		resp := e.do("GET", e.kobo(tc.path), "")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("%s: status = %d, want 200 despite the store's 400", tc.path, resp.StatusCode)
		}
		if strings.Contains(string(body), "Invalid token version") {
			t.Errorf("%s: the store's refusal was relayed to the device: %s", tc.path, body)
		}
		if string(body) != tc.want {
			t.Errorf("%s: body = %s, want our own answer %s", tc.path, body, tc.want)
		}
	}
}

// A store that is off, or simply not answering, is the same case as one that
// refuses: the device still gets a usable reply.
func TestUnreachableStoreFallsBackToOurOwnAnswer(t *testing.T) {
	store := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	store.Close()

	for name, upstream := range map[string]string{
		"off":         "off",
		"unset":       "",
		"unreachable": store.URL,
	} {
		e := newEnvWith(t, envOptions{ProxyUpstream: upstream})
		resp := e.do("GET", e.kobo("/v1/deals"), "")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != 200 || string(body) != `{"Deals":[]}` {
			t.Errorf("upstream %s: got %d %s, want 200 {\"Deals\":[]}", name, resp.StatusCode, body)
		}
	}
}

// A map on disk is served as the base, with our overrides laid on top. This is
// how a full map gets in without vendoring anyone else's copy: an operator dumps
// [OneStoreServices] off a device that has one and drops it there.
func TestSavedResourceMapIsServedUnderOurOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kobo_resources.json")
	saved := map[string]any{"library_sync": "https://storeapi.kobo.com/v1/library/sync"}
	for i := 0; i < 150; i++ {
		saved[fmt.Sprintf("native_key_%d", i)] = fmt.Sprintf("https://storeapi.kobo.com/v1/%d", i)
	}
	buf, _ := json.Marshal(saved)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEnvWith(t, envOptions{ResourcesFile: path})
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	want := len(saved)
	for _, key := range kobo.OverriddenResourceKeys {
		if _, already := saved[key]; !already {
			want++
		}
	}
	if len(res) != want {
		t.Errorf("served %d keys, want the saved map's %d plus our overrides = %d",
			len(res), len(saved), want)
	}
	if got := resourceString(t, res, "native_key_7"); got != "https://storeapi.kobo.com/v1/7" {
		t.Errorf("a key we do not override was not passed through: %q", got)
	}
	if _, builtin := res["store_host"]; builtin {
		t.Error("the built-in map leaked in alongside the operator's file")
	}
	for _, key := range kobo.OverriddenResourceKeys {
		if v := resourceString(t, res, key); !strings.HasPrefix(v, e.server.URL) {
			t.Errorf("%s = %q, the saved map won over our override", key, v)
		}
	}
}

// Too small to be a real map means it is an error page or a truncated write.
// Serving it would put a handful of keys into every device permanently.
func TestUndersizedSavedMapIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kobo_resources.json")
	if err := os.WriteFile(path, []byte(`{"invented_key":"http://nowhere.invalid"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEnvWith(t, envOptions{ResourcesFile: path})
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	if _, ok := res["invented_key"]; ok {
		t.Error("a key from the undersized map was served anyway")
	}
	if len(res) < 100 {
		t.Errorf("served %d keys, want the built-in map instead of the undersized file", len(res))
	}
}

// The map may also be the [OneStoreServices] section lifted straight out of a
// device's Kobo eReader.conf, which is where the best one always is.
func TestSavedMapMayBeAKoboEReaderConfSection(t *testing.T) {
	var conf strings.Builder
	conf.WriteString(`[Something Else]
ignored_key=nope

[OneStoreServices]
api_endpoint=http://192.168.0.1:8078/kobo/someone-elses-token
`)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&conf, "conf_key_%d=https://storeapi.kobo.com/v1/%d", i, i)
		conf.WriteString("\n")
	}

	path := filepath.Join(t.TempDir(), "kobo_resources.json")
	if err := os.WriteFile(path, []byte(conf.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEnvWith(t, envOptions{ResourcesFile: path})
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	if got := resourceString(t, res, "conf_key_3"); got != "https://storeapi.kobo.com/v1/3" {
		t.Errorf("conf_key_3 = %q, the section was not read", got)
	}
	if _, ok := res["ignored_key"]; ok {
		t.Error("a key from another section was served")
	}
	if _, ok := res["api_endpoint"]; ok {
		t.Error("api_endpoint was served back as a resource; it is the bootstrap, not a resource")
	}
	if len(res) != 40+len(kobo.OverriddenResourceKeys) {
		t.Errorf("served %d keys, want the section's 40 plus our %d overrides",
			len(res), len(kobo.OverriddenResourceKeys))
	}
}

// If the store ever does hand over a map, it is kept: the fetch only works
// while the device still holds a token the store minted, which is at most once.
func TestResourceMapFromTheStoreIsSavedToDisk(t *testing.T) {
	native := map[string]any{}
	for i := 0; i < 150; i++ {
		native[fmt.Sprintf("native_key_%d", i)] = fmt.Sprintf("https://storeapi.kobo.com/v1/%d", i)
	}
	hits := 0
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"Resources": native})
	}))
	defer store.Close()

	path := filepath.Join(t.TempDir(), "kobo_resources.json")
	e := newEnvWith(t, envOptions{ProxyUpstream: store.URL, ResourcesFile: path})

	_ = e.do("GET", e.kobo("/v1/initialization"), "").Body.Close()
	_ = e.do("GET", e.kobo("/v1/initialization"), "").Body.Close()

	if hits != 1 {
		t.Errorf("the store was asked %d times, want once: the answer is kept", hits)
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the store's map was not written to disk: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(buf, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != len(native) {
		t.Errorf("saved %d keys, want the store's %d", len(onDisk), len(native))
	}
	if _, ours := onDisk["library_sync"]; ours {
		t.Error("our own override was written into the saved map; the next device would get this server's URL as its native one")
	}
}

// The device substitutes these placeholders literally. calibre-web emits
// lowercase names and a literal "isGreyscale"; copying that bug makes devices
// request a path that cannot be served.
func TestCoverTemplatesUseKobosExactPlaceholders(t *testing.T) {
	e := newEnv(t)
	res := resourcesOf(t, e.do("GET", e.kobo("/v1/initialization"), ""))

	plain := resourceString(t, res, "image_url_template")
	quality := resourceString(t, res, "image_url_quality_template")

	for _, want := range []string{"{ImageId}", "{Width}", "{Height}", "{IsGreyscale}"} {
		if !strings.Contains(plain, want) {
			t.Errorf("image_url_template %q is missing %s", plain, want)
		}
	}
	for _, want := range []string{"{ImageId}", "{Width}", "{Height}", "{Quality}", "{IsGreyscale}"} {
		if !strings.Contains(quality, want) {
			t.Errorf("image_url_quality_template %q is missing %s", quality, want)
		}
	}
	for _, bad := range []string{"{width}", "{height}", "{imageId}", "{isGreyscale}"} {
		if strings.Contains(plain, bad) || strings.Contains(quality, bad) {
			t.Errorf("a template uses the wrong placeholder casing %s", bad)
		}
	}
	// The literal-value bug: the placeholder replaced by a bare word.
	if strings.Contains(quality, "/isGreyscale/") {
		t.Error("image_url_quality_template contains a literal isGreyscale instead of {IsGreyscale}")
	}
	if !strings.HasSuffix(plain, "/image.jpg") || !strings.HasSuffix(quality, "/image.jpg") {
		t.Error("cover templates must end in /image.jpg")
	}
}

// An unknown token must not produce an error status: the device would abort.
func TestUnknownTokenIsQuietNotAnError(t *testing.T) {
	e := newEnv(t)

	resp := e.do("GET", "/kobo/deadbeefdeadbeef/v1/initialization", "")
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 so the device does not abandon the sync", resp.StatusCode)
	}
	body := decode[map[string]any](t, resp)
	if len(body) != 0 {
		t.Errorf("body = %v, want an empty object", body)
	}
}

// Binary endpoints are not part of the sync conversation, so they may 404.
func TestUnknownTokenOnBinaryPathIs404(t *testing.T) {
	e := newEnv(t)
	if resp := e.do("GET", "/kobo/deadbeef/download/some-uuid/KEPUB", ""); resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A revoked token must stop working, cache or no cache.
func TestRevokedTokenStopsWorking(t *testing.T) {
	e := newEnv(t)

	if resp := e.do("GET", e.kobo("/v1/initialization"), ""); resp.StatusCode != 200 {
		t.Fatalf("status = %d before revocation", resp.StatusCode)
	}

	tok, err := store.LookupAPIToken(e.ctx, e.store.Reader(), e.token)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIToken(e.ctx, e.store.Writer(), tok.TokenHash); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LookupAPIToken(e.ctx, e.store.Reader(), e.token); err == nil {
		t.Fatal("a revoked token still resolves in the store")
	}

	// The handler caches lookups, so revoking must also invalidate the cache or
	// the token keeps working for up to a minute.
	e.handler.InvalidateToken(e.token)

	resp := e.do("GET", e.kobo("/v1/initialization"), "")
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want a quiet 200 for a revoked token", resp.StatusCode)
	}
	body := decode[map[string]any](t, resp)
	if _, ok := body["Resources"]; ok {
		t.Error("a revoked token still received a resource map")
	}
}

// Unknown endpoints answer 200 {} when proxying is off, never 404.
func TestUnknownEndpointIsEmptyJSON(t *testing.T) {
	e := newEnv(t)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/user/profile"},
		{"GET", "/v1/products/featured"},
		{"POST", "/v1/analytics/event"},
		{"PUT", "/v1/user/wishlist"},
		{"DELETE", "/v1/something"},
	} {
		resp := e.do(tc.method, e.kobo(tc.path), `{}`)
		if resp.StatusCode != 200 {
			t.Errorf("%s %s: status = %d, want 200", tc.method, tc.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != httpx.ContentTypeJSON {
			t.Errorf("%s %s: Content-Type = %q", tc.method, tc.path, ct)
		}
	}
}

// The device is recorded on first contact, keyed by token and device id.
func TestDeviceIsRecorded(t *testing.T) {
	e := newEnv(t)
	e.do("GET", e.kobo("/v1/initialization"), "")

	devices, err := store.ListDevices(e.ctx, e.store.Reader(), e.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}
	d := devices[0]
	if d.KoboDeviceID != "device-abc" {
		t.Errorf("KoboDeviceID = %q", d.KoboDeviceID)
	}
	if d.Model != "Kobo Clara 2E" {
		t.Errorf("Model = %q", d.Model)
	}
	if d.Firmware != "4.38.23171" {
		t.Errorf("Firmware = %q", d.Firmware)
	}
}

// Two Kobos sharing one token are still two devices: they need separate sync
// state and separate tombstones.
func TestTwoDevicesOnOneTokenAreDistinct(t *testing.T) {
	e := newEnv(t)

	for _, id := range []string{"device-one", "device-two"} {
		req, _ := http.NewRequest("GET", e.server.URL+e.kobo("/v1/initialization"), nil)
		req.Header.Set("x-kobo-deviceid", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	devices, err := store.ListDevices(e.ctx, e.store.Reader(), e.userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(devices))
	}
}

// The token lives in a device config file forever; it must not reach the logs.
func TestRedactPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/kobo/0123456789abcdef/v1/library/sync", "/kobo/012345…/v1/library/sync"},
		{"/kobo/0123456789abcdef", "/kobo/012345…"},
		{"/kobo/short", "/kobo/short…"},
		{"/healthz", "/healthz"},
		{"/", "/"},
	}
	for _, tt := range tests {
		if got := httpx.RedactPath(tt.in); got != tt.want {
			t.Errorf("RedactPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestURLBuilderRepairsPortlessHost(t *testing.T) {
	tests := []struct {
		name, host, want string
	}{
		{"portless host", "books.example.com", "http://books.example.com:8078"},
		{"host with port", "books.example.com:9000", "http://books.example.com:9000"},
		{"trailing colon", "books.example.com:", "http://books.example.com:8078"},
		{"bare ipv4", "192.168.1.10", "http://192.168.1.10:8078"},
		{"ipv6 bracketed with port", "[fd00::1]:9000", "http://[fd00::1]:9000"},
		{"bare ipv6", "fd00::1", "http://[fd00::1]:8078"},
	}

	b := httpx.URLBuilder{ListenPort: "8078"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://placeholder/x", nil)
			r.Host = tt.host

			var got string
			httpx.RepairHost("8078", false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = b.Root(r)
			})).ServeHTTP(httptest.NewRecorder(), r)

			if got != tt.want {
				t.Errorf("Root() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestURLBuilderBaseWins(t *testing.T) {
	base, _ := url.Parse("https://books.example.com")
	b := httpx.URLBuilder{Base: base, ListenPort: "8078"}

	r := httptest.NewRequest("GET", "http://whatever/x", nil)
	r.Host = "internal-host:1234"

	if got := b.Root(r); got != "https://books.example.com" {
		t.Errorf("Root() = %q, want the configured base to win", got)
	}
	if got := b.Abs(r, "kobo", "tok", "v1/library/sync"); got != "https://books.example.com/kobo/tok/v1/library/sync" {
		t.Errorf("Abs() = %q", got)
	}
}

// The device reads these as structures, not as opaque JSON. A bare {} has no
// Items and no ItemCount, and the sync stops there. The shapes are the store's
// own, captured from a trace while proxying was still on.
func TestStoreShapedEndpointsAreNotBareObjects(t *testing.T) {
	e := newEnv(t)

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/v1/user/wishlist?PageIndex=0&PageSize=100",
			[]string{`"Items":[]`, `"ItemCount":0`, `"ItemsPerPage":100`, `"VersionCode":2`}},
		{"/v1/user/loyalty/benefits", []string{`"Benefits":{}`}},
		{"/v1/deals", []string{`"Deals":[]`}},
		{"/v1/user/profile", []string{`"IsOneStore":true`, `"UserId":"`, `"StoreFront":"`}},
	} {
		resp := e.do("GET", e.kobo(tc.path), "")
		if resp.StatusCode != 200 {
			t.Errorf("%s: status = %d", tc.path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		for _, want := range tc.want {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s: body %s is missing %s", tc.path, body, want)
			}
		}
	}
}
