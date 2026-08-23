package kobo

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/fess932/kobibri/internal/httpx"
)

const (
	traceBodyLimit    = 4 << 10
	traceHeaderSecret = "<redacted>"
)

var secretHeaders = []string{
	"authorization", "cookie", "set-cookie", "proxy-authorization",
	"x-kobo-usertoken", "x-kobo-synctoken", "x-kobo-userkey",
}

// Trace logs the whole wire conversation with a device at debug level: query,
// headers and bodies both ways. It costs nothing above debug, where it returns
// before touching the request.
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slog.Default().Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}

		reqBody := captureRequestBody(r)
		log := slog.With(
			"req", httpx.RequestIDFrom(r.Context()),
			"method", r.Method,
			"path", httpx.RedactPath(r.URL.Path),
			"query", redactQuery(r.URL.RawQuery))

		log.Debug("kobo trace: request",
			"headers", traceHeaders(r.Header),
			"body", reqBody)

		rec := &traceRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		log.Debug("kobo trace: response",
			"status", rec.status,
			"bytes", rec.written,
			"headers", traceHeaders(rec.Header()),
			"body", rec.snippet())
	})
}

func captureRequestBody(r *http.Request) string {
	if r.Body == nil || r.ContentLength == 0 {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, traceBodyLimit))
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}

	return renderBody(r.Header.Get("Content-Type"), buf, len(buf) == traceBodyLimit)
}

func traceHeaders(h http.Header) string {
	out := make([]string, 0, len(h))
	for k, vs := range h {
		if isSecretHeader(k) {
			out = append(out, k+": "+traceHeaderSecret)
			continue
		}
		out = append(out, k+": "+strings.Join(vs, ", "))
	}
	sort.Strings(out)
	return strings.Join(out, " | ")
}

func isSecretHeader(name string) bool {
	lower := strings.ToLower(name)
	for _, s := range secretHeaders {
		if lower == s {
			return true
		}
	}
	return false
}

func renderBody(contentType string, buf []byte, truncated bool) string {
	if len(buf) == 0 {
		return ""
	}
	if !printableBody(contentType, buf) {
		return "<binary>"
	}
	out := string(buf)
	if truncated {
		out += "…<truncated>"
	}
	return out
}

// printableBody keeps a book download out of the log. Content-Type decides when
// it says something useful; otherwise a NUL byte in the first chunk does.
func printableBody(contentType string, buf []byte) bool {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json"),
		strings.Contains(ct, "text/"),
		strings.Contains(ct, "xml"),
		strings.Contains(ct, "x-www-form-urlencoded"):
		return true
	case ct != "":
		return false
	}
	return !bytes.ContainsRune(buf, 0)
}

type traceRecorder struct {
	http.ResponseWriter
	status  int
	written int
	wrote   bool
	body    bytes.Buffer
}

func (t *traceRecorder) WriteHeader(status int) {
	if !t.wrote {
		t.status, t.wrote = status, true
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *traceRecorder) Write(b []byte) (int, error) {
	t.wrote = true
	if room := traceBodyLimit - t.body.Len(); room > 0 {
		t.body.Write(b[:min(room, len(b))])
	}
	n, err := t.ResponseWriter.Write(b)
	t.written += n
	return n, err
}

func (t *traceRecorder) snippet() string {
	return renderBody(t.Header().Get("Content-Type"), t.body.Bytes(),
		t.written > t.body.Len())
}

// Unwrap lets http.ResponseController reach the real writer, which a book
// download needs for its per-request write deadline.
func (t *traceRecorder) Unwrap() http.ResponseWriter { return t.ResponseWriter }
