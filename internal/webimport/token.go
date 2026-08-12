package webimport

import (
	"context"
	"strings"
	"sync"

	"github.com/fess932/novelkit/novel"
	"github.com/fess932/novelkit/sources/ranobelib"

	"github.com/fess932/kobibri/internal/store"
)

// Some titles are invisible to anyone not signed in, and the site says 404 for
// them exactly as it does for a book that never existed. An access token makes
// them visible.
//
// It is the site's own token, copied out of a browser that is already signed in.
// Nothing here signs in for anyone, and no password is ever handled.
const tokenKey = "webimport:ranobelib:token"

// tokenState is what the interface needs to show without revealing the secret.
type tokenState struct {
	mu    sync.RWMutex
	value string
}

func (t *tokenState) get() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.value
}

func (t *tokenState) set(value string) {
	t.mu.Lock()
	t.value = value
	t.mu.Unlock()
}

// HasToken reports whether an access token is stored, without handing it out.
func (im *Importer) HasToken() bool { return im.token.get() != "" }

// SetToken stores an access token and rebuilds the providers to use it. An empty
// string clears it.
//
// It is kept in the database rather than only in memory because the daily check
// for new chapters runs long after anyone typed it.
func (im *Importer) SetToken(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if err := store.SetKV(ctx, im.store.Writer(), tokenKey, token); err != nil {
		return err
	}
	im.token.set(token)
	im.rebuildRegistry()
	return nil
}

// loadToken restores the stored token at startup.
func (im *Importer) loadToken(ctx context.Context) {
	token, err := store.GetKV(ctx, im.store.Reader(), tokenKey)
	if err != nil || token == "" {
		return
	}
	im.token.set(token)
	im.rebuildRegistry()
}

// rebuildRegistry replaces the providers with ones carrying the current token.
//
// A whole new registry rather than a mutable client: a source is built once with
// its options, and swapping the registry wholesale means a download already
// running keeps the client it started with.
func (im *Importer) rebuildRegistry() {
	var opts []ranobelib.Option
	if token := im.token.get(); token != "" {
		opts = append(opts, ranobelib.WithToken(token))
	}

	registry := &novel.Registry{}
	registry.Register(ranobelib.NewSource(opts...))

	im.mu.Lock()
	im.registry = registry
	im.mu.Unlock()
}

// providers returns the current registry.
func (im *Importer) providers() *novel.Registry {
	im.mu.RLock()
	defer im.mu.RUnlock()
	return im.registry
}
