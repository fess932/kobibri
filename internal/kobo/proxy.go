package kobo

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
)

// Proxy forwards endpoints kobibri does not implement to the real Kobo store,
// so the shop, firmware updates and previously purchased books keep working.
type Proxy struct {
	upstream string
	client   *http.Client
}

func NewProxy(upstream string) *Proxy {
	return &Proxy{
		upstream: strings.TrimSuffix(upstream, "/"),
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   4,
				ResponseHeaderTimeout: 20 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

func (p *Proxy) Enabled() bool { return p != nil && p.upstream != "" }

var hopByHop = []string{
	"Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length",
	"Upgrade", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
}

// upstreamURL maps a request path of the form /kobo/<token>/rest onto the store.
func (p *Proxy) upstreamURL(r *http.Request) string {
	path := r.URL.Path
	const prefix = "/kobo/"
	if strings.HasPrefix(path, prefix) {
		rest := path[len(prefix):]
		if _, tail, ok := strings.Cut(rest, "/"); ok {
			path = "/" + tail
		} else {
			path = "/"
		}
	}

	url := p.upstream + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	return url
}

// Handle relays an endpoint we do not implement. See docs/kobo-protocol.md §5.
func (p *Proxy) Handle(w http.ResponseWriter, r *http.Request) {
	log := slog.With(
		"req", httpx.RequestIDFrom(r.Context()),
		"endpoint", endpointShape(r),
		"query", redactQuery(r.URL.RawQuery))
	if device := deviceFrom(r.Context()); device != nil {
		log = log.With("device", device.ID)
	}

	if !p.Enabled() {
		log.Info("kobo store request swallowed", "reason", "proxying is off")
		httpx.WriteEmptyJSON(w)
		return
	}

	target := p.upstreamURL(r)
	start := time.Now()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target,
		http.MaxBytesReader(nil, r.Body, 8<<20))
	if err != nil {
		log.Warn("kobo store request could not be built", "err", err)
		httpx.WriteEmptyJSON(w)
		return
	}
	copyRequestHeaders(req, r)

	resp, err := p.client.Do(req)
	if err != nil {
		log.Warn("kobo store request failed", "err", err,
			"took", time.Since(start).Round(time.Millisecond),
			"answered", "200 {}")
		httpx.WriteEmptyJSON(w)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	written, _ := io.Copy(w, resp.Body)

	log.Info("kobo store request forwarded",
		"status", resp.StatusCode, "bytes", written,
		"took", time.Since(start).Round(time.Millisecond))
}

var secretParams = []string{"token", "key", "secret", "password", "signature", "auth", "code"}

func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "<unparseable>"
	}

	out := make([]string, 0, len(values))
	for key := range values {
		if isSecretParam(key) {
			out = append(out, key+"=<redacted>")
			continue
		}
		out = append(out, key+"="+strings.Join(values[key], ","))
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

func isSecretParam(key string) bool {
	lower := strings.ToLower(key)
	for _, secret := range secretParams {
		if strings.Contains(lower, secret) {
			return true
		}
	}
	return false
}

// FetchResources asks the store for the real resource map, using the
// credentials the device sent us on this very request.
func (p *Proxy) FetchResources(r *http.Request) (map[string]any, error) {
	target := p.upstream + "/v1/initialization"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req, r)
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		slog.Warn("kobo store request failed", "endpoint", "GET /v1/initialization",
			"err", err, "took", time.Since(start).Round(time.Millisecond))
		return nil, err
	}
	defer resp.Body.Close()

	slog.Info("kobo store request forwarded", "endpoint", "GET /v1/initialization",
		"status", resp.StatusCode, "took", time.Since(start).Round(time.Millisecond))

	if resp.StatusCode != http.StatusOK {
		if snippet, err := io.ReadAll(io.LimitReader(resp.Body, 512)); err == nil && len(snippet) > 0 {
			slog.Debug("kobo store refused the resource map",
				"status", resp.StatusCode, "body", string(snippet),
				"sent_query", redactQuery(r.URL.RawQuery))
		}
		return nil, &upstreamStatusError{code: resp.StatusCode}
	}

	var body struct {
		Resources map[string]any `json:"Resources"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Resources, nil
}

type upstreamStatusError struct{ code int }

func (e *upstreamStatusError) Error() string {
	return "kobo store returned status " + http.StatusText(e.code)
}

func copyRequestHeaders(dst *http.Request, src *http.Request) {
	for k, vs := range src.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
	dst.Host = ""
}

func isHopByHop(header string) bool {
	for _, h := range hopByHop {
		if strings.EqualFold(header, h) {
			return true
		}
	}
	return false
}
