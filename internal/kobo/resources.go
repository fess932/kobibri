package kobo

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// upstreamFloor is the smallest map worth believing from the store. Kobo's own
// answer carries roughly 150 keys; anything dramatically smaller is a truncated
// or error response, and writing it to disk would poison every later device.
const upstreamFloor = 100

// fileFloor is the bar for a map an operator put there on purpose, and it is
// much lower: the best source is a dump of [OneStoreServices] off a device that
// already works, and one of those holds around seventy keys, not a hundred and
// fifty. Only an error page or a stray empty object should be turned away.
const fileFloor = 20

// upstreamRetryAfter is how long a refusal from the store stands. It refuses
// for as long as the reader is registered here, so asking on every
// initialization spends the device's time to be told the same thing.
const upstreamRetryAfter = 24 * time.Hour

// resourceStore holds the base map for /v1/initialization: whatever the store
// last gave us, or whatever an operator put there by hand.
//
// A file rather than a table because it is meant to be edited: the honest way
// to get a full map is to copy [OneStoreServices] off a device that has one.
// See docs/NOTES.md.
type resourceStore struct {
	path string

	mu          sync.Mutex
	loaded      bool
	cached      map[string]any
	refusedAt   time.Time
	haveRefusal bool
}

func newResourceStore(path string) *resourceStore { return &resourceStore{path: path} }

// Base returns the map to lay overrides over, and where it came from. An empty
// map is a valid answer: the device keeps whatever it already has for the keys
// nobody sends it.
func (s *resourceStore) Base(p *Proxy, r *http.Request) (map[string]any, string) {
	if s == nil {
		return map[string]any{}, "overrides only"
	}

	s.mu.Lock()
	if !s.loaded {
		s.cached = s.readFile()
		s.loaded = true
	}
	cached := s.cached
	refused := s.haveRefusal && time.Since(s.refusedAt) < upstreamRetryAfter
	s.mu.Unlock()

	if cached != nil {
		return clone(cached), "saved map (" + s.path + ")"
	}
	if !refused && p.Enabled() {
		fetched, err := p.FetchResources(r)
		switch {
		case err != nil:
			s.rememberRefusal()
			slog.Warn("could not fetch the resource map from the store; falling back",
				"err", err, "hint", "drop a map at "+s.path+" to serve your own")
		case len(fetched) < upstreamFloor:
			s.rememberRefusal()
			slog.Warn("the store's resource map looks truncated; ignoring it",
				"keys", len(fetched), "floor", upstreamFloor)
		default:
			s.save(fetched)
			return clone(fetched), "store, now saved to " + s.path
		}
	}

	return clone(nativeResources), "built in"
}

func (s *resourceStore) readFile() map[string]any {
	if s.path == "" {
		return nil
	}
	buf, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("reading the saved Kobo resource map", "path", s.path, "err", err)
		}
		return nil
	}

	m, format := parseResources(buf)
	if m == nil {
		slog.Warn("the saved Kobo resource map is neither JSON nor a [OneStoreServices] section; ignoring it",
			"path", s.path)
		return nil
	}
	if len(m) < fileFloor {
		slog.Warn("the saved Kobo resource map is too small to be a real one; ignoring it",
			"path", s.path, "keys", len(m), "floor", fileFloor)
		return nil
	}

	slog.Info("using the saved Kobo resource map",
		"path", s.path, "keys", len(m), "format", format)
	return m
}

// parseResources accepts the map either as JSON or as the [OneStoreServices]
// section copied straight out of a device's Kobo eReader.conf, because that
// section is the best map there is and asking anyone to convert it by hand is
// asking for a mistake in the one file that cannot be got wrong.
func parseResources(buf []byte) (map[string]any, string) {
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err == nil && m != nil {
		delete(m, "api_endpoint")
		return m, "json"
	}
	if m := parseOneStoreServices(buf); len(m) > 0 {
		return m, "Kobo eReader.conf"
	}
	return nil, ""
}

// parseOneStoreServices reads the INI section. A file with no section header at
// all is taken whole, so a copied fragment works as well as the real thing.
func parseOneStoreServices(buf []byte) map[string]any {
	out := map[string]any{}
	inSection := true

	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = strings.EqualFold(line, "[OneStoreServices]")
			continue
		}
		if !inSection {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		// api_endpoint is how the device found us in the first place and is not
		// one of the Resources; handing it back would be a server naming itself.
		if !ok || key == "" || key == "api_endpoint" {
			continue
		}
		out[key] = strings.TrimSpace(value)
	}
	return out
}

// save writes the map and keeps it, so the store is asked exactly once.
func (s *resourceStore) save(m map[string]any) {
	s.mu.Lock()
	s.cached = m
	s.haveRefusal = false
	s.mu.Unlock()

	if s.path == "" {
		return
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		slog.Warn("saving the Kobo resource map", "path", s.path, "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		slog.Warn("saving the Kobo resource map", "path", s.path, "err", err)
		return
	}
	slog.Info("saved the Kobo resource map from the store", "path", s.path, "keys", len(m))
}

func (s *resourceStore) rememberRefusal() {
	s.mu.Lock()
	s.refusedAt = time.Now()
	s.haveRefusal = true
	s.mu.Unlock()
}

// clone keeps applyOverrides from writing our server's URLs into the map we
// hold for the next device.
func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
