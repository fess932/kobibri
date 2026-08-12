// Command kobibri serves a Calibre library to Kobo e-readers.
package main

import (
	"context"

	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/fess932/kobibri/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kobibri:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)

	// Signals cancel the context so every command shuts down the same way.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return cmdServe(ctx, cfg, args)
	case "migrate":
		return cmdMigrate(ctx, cfg, args)
	case "scan":
		return cmdScan(ctx, cfg, args)
	case "source":
		return cmdSource(ctx, cfg, args)
	case "ingest":
		return cmdIngest(ctx, cfg, args)
	case "token":
		return cmdToken(ctx, cfg, args)
	case "convert":
		return cmdConvert(ctx, cfg, args)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `kobibri — sync a Calibre library to Kobo e-readers

usage: kobibri [command] [flags]

commands:
  serve      run the HTTP server (default)
  migrate    create or upgrade the database, then exit
  source     manage Calibre libraries: add, list, enable, disable
  ingest     scan registered sources into the canonical library
  token      issue a device token and print the Kobo eReader.conf line
  convert    convert imported books to KEPUB ahead of time
  scan       read a Calibre library and print what kobibri sees (read-only)

environment:
  KOBIBRI_DATA_DIR          data directory (database, caches)
  KOBIBRI_LISTEN            listen address (default `+config.DefaultListen+`)
  KOBIBRI_BASE_URL          absolute public URL; strongly recommended behind a proxy
  KOBIBRI_TRUST_PROXY       honour X-Forwarded-Proto / X-Forwarded-Host
  KOBIBRI_PROXY_UPSTREAM    Kobo store to proxy unknown endpoints to ("off" disables)
  KOBIBRI_ADMIN_PASSWORD    first-run admin password
  KOBIBRI_TLS_CERT          serve HTTPS directly; pair with KOBIBRI_TLS_KEY
  KOBIBRI_TLS_KEY           the private key for the certificate above
  KOBIBRI_KEPUBIFY_BIN      use an external kepubify binary instead of the library
  KOBIBRI_LOG_LEVEL         debug|info|warn|error
`)
}

func setupLogging(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
