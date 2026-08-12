// Package config holds the bootstrap configuration. Only the settings needed to
// reach the database live here; everything else (sources, users, tokens) lives
// in the database and is edited through the web UI.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	DataDir  string   // where kobibri.db, caches and temp scan copies live
	Listen   string   // host:port to listen on
	BaseURL  *url.URL // absolute public URL, wins over Host-header sniffing
	LogLevel string   // debug|info|warn|error

	TrustProxy    bool   // honour X-Forwarded-Proto / X-Forwarded-Host
	ProxyUpstream string // Kobo store to proxy unknown endpoints to; empty disables proxying

	AdminPassword string // first-run bootstrap only

	// TLSCert and TLSKey serve HTTPS directly. Leave empty behind a reverse
	// proxy, which is the usual arrangement.
	TLSCert string
	TLSKey  string

	KepubifyBin     string // escape hatch: use this binary instead of the embedded library
	KepubCacheBytes int64
	CoverCacheBytes int64
}

const (
	DefaultProxyUpstream = "https://storeapi.kobo.com"
	DefaultListen        = ":8078"
)

// Load builds a Config from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		DataDir:         envOr("KOBIBRI_DATA_DIR", defaultDataDir()),
		Listen:          envOr("KOBIBRI_LISTEN", DefaultListen),
		LogLevel:        envOr("KOBIBRI_LOG_LEVEL", "info"),
		ProxyUpstream:   envOr("KOBIBRI_PROXY_UPSTREAM", DefaultProxyUpstream),
		AdminPassword:   os.Getenv("KOBIBRI_ADMIN_PASSWORD"),
		TLSCert:         os.Getenv("KOBIBRI_TLS_CERT"),
		TLSKey:          os.Getenv("KOBIBRI_TLS_KEY"),
		KepubifyBin:     os.Getenv("KOBIBRI_KEPUBIFY_BIN"),
		KepubCacheBytes: 4 << 30,
		CoverCacheBytes: 1 << 30,
	}

	var err error
	if c.TrustProxy, err = envBool("KOBIBRI_TRUST_PROXY", false); err != nil {
		return nil, err
	}
	if c.KepubCacheBytes, err = envBytes("KOBIBRI_KEPUB_CACHE_BYTES", c.KepubCacheBytes); err != nil {
		return nil, err
	}
	if c.CoverCacheBytes, err = envBytes("KOBIBRI_COVER_CACHE_BYTES", c.CoverCacheBytes); err != nil {
		return nil, err
	}

	if raw := os.Getenv("KOBIBRI_BASE_URL"); raw != "" {
		u, err := url.Parse(strings.TrimSuffix(raw, "/"))
		if err != nil {
			return nil, fmt.Errorf("KOBIBRI_BASE_URL: %w", err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("KOBIBRI_BASE_URL %q: need an absolute URL like https://books.example.com", raw)
		}
		c.BaseURL = u
	}

	if c.DataDir, err = filepath.Abs(c.DataDir); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}

	// Half a TLS pair is a configuration mistake worth catching at startup
	// rather than serving plain HTTP and leaving the operator to wonder.
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("set both KOBIBRI_TLS_CERT and KOBIBRI_TLS_KEY, or neither")
	}
	return c, nil
}

// DBPath is the path to the server's own SQLite database.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "kobibri.db") }

// CacheDir holds derived artefacts (converted kepubs, scaled covers).
func (c *Config) CacheDir() string { return filepath.Join(c.DataDir, "cache") }

// TmpDir holds snapshot copies of Calibre databases during a scan.
func (c *Config) TmpDir() string { return filepath.Join(c.DataDir, "tmp") }

// EnsureDirs creates the directory tree the server needs.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.DataDir, c.CacheDir(), c.TmpDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

// ProxyEnabled reports whether unknown Kobo endpoints are forwarded upstream.
func (c *Config) ProxyEnabled() bool {
	return c.ProxyUpstream != "" && c.ProxyUpstream != "off"
}

func defaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "kobibri")
	}
	return "./kobibri-data"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func envBytes(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: expected a non-negative byte count, got %q", key, v)
	}
	return n, nil
}
