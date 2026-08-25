package kobo

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tracing must not eat the body it reads: the handler behind it has to see the
// whole thing, or every PUT of reading progress is silently lost.
func TestTracePassesTheBodyOn(t *testing.T) {
	restore := debugLogging(t)
	defer restore()

	body := strings.Repeat("x", traceBodyLimit*2)
	var got string

	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		got = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest("PUT", "/kobo/tok/v1/library/x/state", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != body {
		t.Fatalf("handler saw %d bytes of body, want %d", len(got), len(body))
	}
}

// A book download must not be written into the log.
func TestTraceDoesNotDumpBinaryResponses(t *testing.T) {
	restore := debugLogging(t)
	defer restore()

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	payload := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0x00, 0xff}, 4096)...)
	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		_, _ = w.Write(payload)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/kobo/tok/download/x/KEPUB", nil))

	if rec.Body.Len() != len(payload) {
		t.Fatalf("the client got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	if !strings.Contains(buf.String(), "<binary>") {
		t.Error("a binary response was not marked as such in the trace")
	}
	if strings.Contains(buf.String(), "PK\\x03\\x04") {
		t.Error("binary payload reached the log")
	}
}

// Credentials must never be written down, in a header or a query parameter.
func TestTraceRedactsSecrets(t *testing.T) {
	restore := debugLogging(t)
	defer restore()

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))

	req := httptest.NewRequest("GET", "/kobo/supersecrettoken/v1/library/sync?AccessToken=zzsecretvaluezz&PageSize=100", nil)
	req.Header.Set("Authorization", "Bearer hunter2")
	req.Header.Set("X-Kobo-DeviceId", "abcd")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	for _, secret := range []string{"hunter2", "zzsecretvaluezz", "supersecrettoken"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q reached the log", secret)
		}
	}
	// The shape has to survive, or the trace is useless.
	for _, want := range []string{"PageSize=100", "X-Kobo-Deviceid: abcd"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q is missing from the trace; got %s", want, out)
		}
	}
}

// Above debug the middleware must not read anything at all.
func TestTraceIsInertAboveDebug(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(old)

	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/kobo/tok/v1/initialization", nil))

	if strings.Contains(buf.String(), "kobo trace") {
		t.Error("the trace ran at info level")
	}
}

func debugLogging(t *testing.T) func() {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(old) }
}

// The store answers gzipped now that the device's Accept-Encoding is forwarded.
// Dumping those bytes verbatim filled the log with binary and said nothing.
func TestTraceDecompressesGzippedResponses(t *testing.T) {
	restore := debugLogging(t)
	defer restore()

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"Benefits":{"secretless":true}}`))
	_ = zw.Close()

	h := Trace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz.Bytes())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/kobo/tok/v1/user/loyalty/benefits", nil))

	if rec.Body.Len() != gz.Len() {
		t.Errorf("the client got %d bytes, want the %d compressed ones untouched",
			rec.Body.Len(), gz.Len())
	}
	if !strings.Contains(buf.String(), "secretless") {
		t.Errorf("the gzipped body was not decompressed for the log; got %s", buf.String())
	}
}
