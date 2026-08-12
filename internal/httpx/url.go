// Package httpx holds the HTTP plumbing shared by the Kobo API and the web UI.
package httpx

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// URLBuilder produces the absolute URLs kobibri hands to a device.
//
// Every URL that reaches a Kobo — download links, cover templates, the endpoint
// map in /v1/initialization — goes through here. There is exactly one place
// that can get it wrong, and getting it wrong in the initialization response is
// permanent: the device caches those values in its config file forever.
type URLBuilder struct {
	// Base, when set, wins unconditionally. This is the correct configuration
	// behind any reverse proxy.
	Base *url.URL
	// ListenPort repairs the portless Host header Kobo devices send.
	ListenPort string
	// TrustProxy enables X-Forwarded-Proto / X-Forwarded-Host.
	TrustProxy bool
}

// Abs joins path elements onto the public root.
func (b URLBuilder) Abs(r *http.Request, elem ...string) string {
	root := b.Root(r)
	if len(elem) == 0 {
		return root
	}

	var sb strings.Builder
	sb.WriteString(strings.TrimSuffix(root, "/"))
	for _, e := range elem {
		e = strings.Trim(e, "/")
		if e == "" {
			continue
		}
		sb.WriteByte('/')
		sb.WriteString(e)
	}
	return sb.String()
}

// Root is the public scheme://host[:port] this server is reachable at.
func (b URLBuilder) Root(r *http.Request) string {
	if b.Base != nil {
		return strings.TrimSuffix(b.Base.String(), "/")
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host

	if b.TrustProxy {
		if p := firstForwarded(r.Header.Get("X-Forwarded-Proto")); p != "" {
			scheme = p
		}
		if h := firstForwarded(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	return scheme + "://" + host
}

func firstForwarded(v string) string {
	if v == "" {
		return ""
	}
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// RepairHost fixes the malformed Host headers Kobo devices send.
//
// Firmware routinely omits the port, which makes every absolute URL we build
// point at the default port instead of the one we are actually served on. Komga
// ships a dedicated filter for this; calibre-web has a configuration option.
// Doing it once, early, means nothing downstream has to think about it.
func RepairHost(listenPort string, trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Host = repairHost(r, listenPort, trustProxy)
			next.ServeHTTP(w, r)
		})
	}
}

func repairHost(r *http.Request, listenPort string, trustProxy bool) string {
	host := r.Host

	// A proxy in front of us knows the real host; believe it when told to.
	if trustProxy {
		if h := firstForwarded(r.Header.Get("X-Forwarded-Host")); h != "" {
			host = h
		} else if _, _, _, ok := parseForwarded(r.Header.Get("Forwarded")); ok {
			_, _, h, _ := parseForwarded(r.Header.Get("Forwarded"))
			host = h
		}
	}

	host = strings.TrimSuffix(host, ":")
	if host == "" {
		return r.Host
	}

	// A bare IPv6 literal must be bracketed before a port can be attached.
	if strings.Count(host, ":") > 1 && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if listenPort == "" {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), listenPort)
}

// parseForwarded pulls the host out of an RFC 7239 Forwarded header. Only the
// first element is considered, and only the host parameter is used.
func parseForwarded(v string) (proto, by, host string, ok bool) {
	if v == "" {
		return "", "", "", false
	}
	first, _, _ := strings.Cut(v, ",")
	for part := range strings.SplitSeq(first, ";") {
		k, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "host":
			host, ok = val, val != ""
		case "proto":
			proto = val
		case "by":
			by = val
		}
	}
	return proto, by, host, ok
}

// PortOf extracts the port from a listen address like ":8078" or "1.2.3.4:80".
func PortOf(listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return ""
	}
	return port
}
