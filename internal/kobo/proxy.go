package kobo

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fess932/kobibri/internal/httpx"
)

// Proxy forwards endpoints kobibri does not answer itself to the real Kobo
// store. See docs/NOTES.md.
type Proxy struct {
	upstream string
	client   *http.Client
}

func NewProxy(upstream string) *Proxy {
	if upstream == "off" {
		upstream = ""
	}
	return &Proxy{
		upstream: strings.TrimSuffix(upstream, "/"),
		client: &http.Client{
			// No overall deadline: a firmware image comes through here and a
			// blanket timeout would cut it off part-way.
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   4,
				ResponseHeaderTimeout: 20 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
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

// Relay forwards one request to the store and copies the answer back, reporting
// whether the device was served.
//
// A refusal is logged and nothing is written, leaving the caller to answer
// instead. Handing the store's 4xx to the device is what aborts its whole sync:
// a failure on this host reads as the sync server being broken, where the same
// failure collected from storeapi.kobo.com reads as the shop being away. See
// docs/NOTES.md.
func (p *Proxy) Relay(w http.ResponseWriter, r *http.Request) bool {
	if !p.Enabled() {
		return false
	}

	log := slog.With(
		"req", httpx.RequestIDFrom(r.Context()),
		"endpoint", endpointShape(r),
		"query", redactQuery(r.URL.RawQuery))
	if device := deviceFrom(r.Context()); device != nil {
		log = log.With("device", device.ID)
	}

	target := p.upstreamURL(r)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target,
		http.MaxBytesReader(nil, r.Body, 8<<20))
	if err != nil {
		log.Warn("kobo store request could not be built", "err", err)
		return false
	}
	copyRequestHeaders(req, r)

	log.Debug("kobo store request", "target", p.upstream+req.URL.Path,
		"headers", traceHeaders(req.Header))

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		log.Warn("kobo store request failed", "err", err,
			"took", time.Since(start).Round(time.Millisecond))
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	took := time.Since(start).Round(time.Millisecond)

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		log.Warn("kobo store refused; answering the device ourselves",
			"status", resp.StatusCode, "took", took,
			"headers", traceHeaders(resp.Header),
			"body", renderBody(resp.Header.Get("Content-Type"),
				resp.Header.Get("Content-Encoding"), body, false))
		return false
	}

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
		"status", resp.StatusCode, "bytes", written, "took", took)
	return true
}

// FetchResources asks the store for its /v1/initialization map, using the
// credentials the device sent us on this very request. It is the only way to
// obtain one: the endpoint answers 401 without device credentials, so a server
// cannot fetch it on its own. See docs/NOTES.md.
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
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	took := time.Since(start).Round(time.Millisecond)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		slog.Warn("the store refused its resource map",
			"status", resp.StatusCode, "took", took,
			"body", renderBody(resp.Header.Get("Content-Type"),
				resp.Header.Get("Content-Encoding"), body, false))
		return nil, &upstreamStatusError{code: resp.StatusCode}
	}

	var body struct {
		Resources map[string]any `json:"Resources"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}
	slog.Info("fetched the resource map from the store",
		"keys", len(body.Resources), "took", took)
	return body.Resources, nil
}

type upstreamStatusError struct{ code int }

func (e *upstreamStatusError) Error() string {
	return "kobo store returned " + http.StatusText(e.code)
}

// copyRequestHeaders sends the device's headers on verbatim and adds none of
// ours: to the store this has to look like the reader talking to it directly.
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
