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
			Timeout:   10 * time.Second,
			Transport: &http.Transport{MaxIdleConnsPerHost: 4},
		},
	}
}

func (p *Proxy) Enabled() bool { return p != nil && p.upstream != "" }

// forwardedHeaders is what we pass upstream. Anything else is dropped: the
// device sends identifying headers we have no business relaying beyond what the
// store needs to answer.
var forwardedHeaders = []string{
	"Authorization", "User-Agent", "Accept", "Accept-Language", "Content-Type",
}

// hopByHop must never be copied between the two connections.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Transfer-Encoding", "Content-Encoding",
	"Content-Length", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
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

// Handle serves an endpoint we do not implement.
//
// GET is answered with a 307 redirect, which is cheap and works. Anything else
// must be proxied for real: the device downgrades non-GET methods to GET when
// it follows a redirect, which would silently turn a state-changing call into a
// read. See docs/kobo-protocol.md §5.
func (p *Proxy) Handle(w http.ResponseWriter, r *http.Request) {
	if !p.Enabled() {
		httpx.WriteEmptyJSON(w)
		return
	}

	target := p.upstreamURL(r)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target,
		http.MaxBytesReader(nil, r.Body, 8<<20))
	if err != nil {
		slog.Debug("building upstream request", "err", err)
		httpx.WriteEmptyJSON(w)
		return
	}
	copyRequestHeaders(req, r)

	resp, err := p.client.Do(req)
	if err != nil {
		// A store that is slow, blocked or simply unreachable must not turn
		// into an error the device sees.
		slog.Debug("upstream request failed", "url", target, "err", err)
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
	io.Copy(w, resp.Body)
}

// FetchResources asks the store for the real resource map, using the
// credentials the device sent us on this very request.
func (p *Proxy) FetchResources(r *http.Request) (map[string]any, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		p.upstream+"/v1/initialization", nil)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req, r)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
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
	for _, h := range forwardedHeaders {
		if v := src.Header.Get(h); v != "" {
			dst.Header.Set(h, v)
		}
	}
	// Every x-kobo-* header goes through, except the sync token: ours is not
	// the store's, and handing it over would confuse both sides.
	for k, vs := range src.Header {
		lower := strings.ToLower(k)
		if !strings.HasPrefix(lower, "x-kobo-") || lower == hdrSyncToken {
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
