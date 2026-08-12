package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

var requestCounter atomic.Uint64

// Chain applies middleware so that the first listed runs outermost.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// RequestID tags each request so log lines can be correlated.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strconv.FormatUint(requestCounter.Add(1), 36)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Recoverer turns a panic into whatever onPanic writes, and logs the stack.
//
// Under the Kobo API onPanic must write `200 {}`: a 500 on any endpoint makes
// the device abandon the whole sync.
func Recoverer(onPanic func(http.ResponseWriter, *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					if p == http.ErrAbortHandler {
						panic(p)
					}
					slog.Error("panic serving request",
						"req", RequestIDFrom(r.Context()),
						"method", r.Method, "path", RedactPath(r.URL.Path),
						"panic", p, "stack", string(debug.Stack()))
					onPanic(w, r)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog logs one line per request with the token redacted.
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		slog.Debug("request",
			"req", RequestIDFrom(r.Context()),
			"method", r.Method, "path", RedactPath(r.URL.Path),
			"status", rec.status, "bytes", rec.written,
			"took", time.Since(start).Round(time.Millisecond))
	})
}

// RedactPath replaces the secret in /kobo/<token>/... with a short hint, so a
// token that lives in a device's config file forever never lands in a log file
// or a pasted bug report.
func RedactPath(path string) string {
	const prefix = "/kobo/"
	if !strings.HasPrefix(path, prefix) {
		return path
	}
	rest := path[len(prefix):]
	token, tail, hasTail := strings.Cut(rest, "/")

	hint := token
	if len(hint) > 6 {
		hint = hint[:6]
	}
	out := prefix + hint + "…"
	if hasTail {
		out += "/" + tail
	}
	return out
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int
	wrote   bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, which downloads
// need for per-request write deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
