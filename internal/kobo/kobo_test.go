package kobo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	ProxyUpstream string
	// SyncBatch keeps a test's fixture small while still exercising the
	// continuation path.
	SyncBatch int
}

func newEnv(t *testing.T, proxyUpstream string) *env {
	t.Helper()
	return newEnvWith(t, envOptions{ProxyUpstream: proxyUpstream})
}

func newEnvWith(t *testing.T, opts envOptions) *env {
	t.Helper()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "kobibri.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

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
		ProxyUpstream: opts.ProxyUpstream,
		Kepub:         kepubCache,
		Covers:        coverCache,
		SyncBatch:     opts.SyncBatch,
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
	e.t.Cleanup(func() { resp.Body.Close() })
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
	e := newEnv(t, "")

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
	e := newEnv(t, "")
	for _, path := range []string{"/v1/initialization", "/v1/analytics/gettests", "/v1/whatever"} {
		resp := e.do("GET", e.kobo(path), "")
		if got := resp.Header.Get("x-kobo-apitoken"); got != "e30=" {
			t.Errorf("%s: x-kobo-apitoken = %q, want %q", path, got, "e30=")
		}
	}
}

// The initialization response is cached permanently in the device's config
// file. Every key we promise must be present and must point at us.
func TestInitializationOverridesEveryKey(t *testing.T) {
	e := newEnv(t, "")

	resp := e.do("GET", e.kobo("/v1/initialization"), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != httpx.ContentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, httpx.ContentTypeJSON)
	}

	body := decode[struct {
		Resources map[string]string `json:"Resources"`
	}](t, resp)

	for _, key := range kobo.OverriddenResourceKeys {
		v, ok := body.Resources[key]
		if !ok {
			t.Errorf("missing overridden key %q", key)
			continue
		}
		if !strings.HasPrefix(v, e.server.URL) {
			t.Errorf("%s = %q, does not point at this server", key, v)
		}
		if strings.Contains(v, "storeapi.kobo.com") || strings.Contains(v, "cdn.kobo.com") {
			t.Errorf("%s = %q, still points at Kobo", key, v)
		}
	}
	if len(body.Resources) != len(kobo.OverriddenResourceKeys) {
		t.Errorf("got %d resource keys, want exactly the %d we override when there is no upstream map",
			len(body.Resources), len(kobo.OverriddenResourceKeys))
	}
}

// The device substitutes these placeholders literally. calibre-web emits
// lowercase names and a literal "isGreyscale"; copying that bug makes devices
// request a path that cannot be served.
func TestCoverTemplatesUseKobosExactPlaceholders(t *testing.T) {
	e := newEnv(t, "")
	body := decode[struct {
		Resources map[string]string `json:"Resources"`
	}](t, e.do("GET", e.kobo("/v1/initialization"), ""))

	plain := body.Resources["image_url_template"]
	quality := body.Resources["image_url_quality_template"]

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

// A truncated or error response from the store must never be cached and served
// on: it would wedge every device that asks afterwards.
func TestTruncatedUpstreamResourcesAreIgnored(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Resources":{"library_sync":"https://storeapi.kobo.com/v1/library/sync"}}`))
	}))
	defer upstream.Close()

	e := newEnv(t, upstream.URL)
	body := decode[struct {
		Resources map[string]string `json:"Resources"`
	}](t, e.do("GET", e.kobo("/v1/initialization"), ""))

	if len(body.Resources) != len(kobo.OverriddenResourceKeys) {
		t.Errorf("got %d keys, want only our %d overrides — the short upstream map should be discarded",
			len(body.Resources), len(kobo.OverriddenResourceKeys))
	}
	if v := body.Resources["library_sync"]; strings.Contains(v, "storeapi.kobo.com") {
		t.Errorf("library_sync = %q, upstream value leaked through the override", v)
	}
}

// A full upstream map is merged in, with our overrides applied last.
func TestFullUpstreamResourcesAreMergedAndOverridden(t *testing.T) {
	full := map[string]string{}
	for i := range 150 {
		full["native_key_"+string(rune('a'+i%26))+itoa(i)] = "https://storeapi.kobo.com/v1/thing"
	}
	full["library_sync"] = "https://storeapi.kobo.com/v1/library/sync"
	full["image_url_template"] = "https://cdn.kobo.com/book-images/{ImageId}/{Width}/{Height}/false/image.jpg"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Resources": full})
	}))
	defer upstream.Close()

	e := newEnv(t, upstream.URL)
	body := decode[struct {
		Resources map[string]string `json:"Resources"`
	}](t, e.do("GET", e.kobo("/v1/initialization"), ""))

	if len(body.Resources) < len(full) {
		t.Errorf("got %d keys, want at least the %d from upstream", len(body.Resources), len(full))
	}
	for _, key := range kobo.OverriddenResourceKeys {
		if v := body.Resources[key]; !strings.HasPrefix(v, e.server.URL) {
			t.Errorf("%s = %q, our override did not win over the upstream value", key, v)
		}
	}
	if v := body.Resources["native_key_a0"]; v != "https://storeapi.kobo.com/v1/thing" {
		t.Errorf("a native key we do not override was altered: %q", v)
	}
}

// An unknown token must not produce an error status: the device would abort.
func TestUnknownTokenIsQuietNotAnError(t *testing.T) {
	e := newEnv(t, "")

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
	e := newEnv(t, "")
	if resp := e.do("GET", "/kobo/deadbeef/download/some-uuid/KEPUB", ""); resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A revoked token must stop working, cache or no cache.
func TestRevokedTokenStopsWorking(t *testing.T) {
	e := newEnv(t, "")

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
func TestUnknownEndpointIsEmptyJSONWithoutProxy(t *testing.T) {
	e := newEnv(t, "")

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

// Every method is relayed, GET included. A redirect would send the device to the
// store directly, where the response never passes through here to be logged.
func TestProxyRelaysEveryMethodItself(t *testing.T) {
	var gotMethods []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	e := newEnv(t, upstream.URL)

	resp := e.do("GET", e.kobo("/v1/products/featured"), "")
	if resp.StatusCode != 200 {
		t.Errorf("GET status = %d, want the relayed 200", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("the device was redirected to %q instead of being served", loc)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want the store's own answer", body)
	}

	resp = e.do("POST", e.kobo("/v1/user/wishlist"), `{"add":1}`)
	if resp.StatusCode != 200 {
		t.Errorf("POST status = %d, want the proxied 200", resp.StatusCode)
	}

	want := []string{"GET /v1/products/featured", "POST /v1/user/wishlist"}
	if len(gotMethods) != 2 || gotMethods[0] != want[0] || gotMethods[1] != want[1] {
		t.Errorf("upstream saw %v, want %v", gotMethods, want)
	}
}

// To the store this has to look like the reader itself, so the headers reach it
// unaltered and nothing of ours is added.
func TestProxyForwardsTheDevicesHeadersUntouched(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	e := newEnv(t, upstream.URL)

	req, _ := http.NewRequest("POST", e.server.URL+e.kobo("/v1/user/wishlist"), strings.NewReader("{}"))
	sent := map[string]string{
		"x-kobo-deviceid":      "device-abc",
		"x-kobo-synctoken":     "whatever-the-device-sent",
		"x-kobo-devicemodel":   "Kobo Libra Colour",
		"x-kobo-platformid":    "00000000-0000-0000-0000-000000000390",
		"x-kobo-affiliatename": "Kobo",
		"Authorization":        "Bearer abc",
		"Accept-Language":      "en-US, *;q=0.9",
		"User-Agent":           "Mozilla/5.0 (Kobo Touch 0390/4.45.23697)",
	}
	for k, v := range sent {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for k, v := range sent {
		if got.Get(k) != v {
			t.Errorf("%s reached the store as %q, want %q", k, got.Get(k), v)
		}
	}
	for _, added := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "Via", "Forwarded"} {
		if v := got.Get(added); v != "" {
			t.Errorf("we announced ourselves to the store with %s: %q", added, v)
		}
	}
}

// An unreachable store must not turn into an error the device sees.
func TestProxyFailureIsStillAnEmptySuccess(t *testing.T) {
	e := newEnv(t, "http://127.0.0.1:1")

	resp := e.do("POST", e.kobo("/v1/user/wishlist"), `{}`)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 despite the upstream being unreachable", resp.StatusCode)
	}
}

// The device is recorded on first contact, keyed by token and device id.
func TestDeviceIsRecorded(t *testing.T) {
	e := newEnv(t, "")
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
	e := newEnv(t, "")

	for _, id := range []string{"device-one", "device-two"} {
		req, _ := http.NewRequest("GET", e.server.URL+e.kobo("/v1/initialization"), nil)
		req.Header.Set("x-kobo-deviceid", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
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

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}
