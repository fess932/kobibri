package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/fess932/kobibri/internal/calibre"
	"github.com/fess932/kobibri/internal/config"
	"github.com/fess932/kobibri/internal/covers"
	"github.com/fess932/kobibri/internal/ebookconv"
	"github.com/fess932/kobibri/internal/httpx"
	"github.com/fess932/kobibri/internal/ingest"
	"github.com/fess932/kobibri/internal/kepubconv"
	"github.com/fess932/kobibri/internal/kobo"
	"github.com/fess932/kobibri/internal/store"
	"github.com/fess932/kobibri/internal/upload"
	"github.com/fess932/kobibri/internal/web"
	"github.com/fess932/kobibri/internal/webimport"
)

// openStore prepares the data directory and opens the database, applying any
// pending migrations.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	return store.Open(ctx, cfg.DBPath())
}

func cmdMigrate(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	v, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s: schema version %d\n", st.Path(), v)
	return nil
}

// cmdSource manages the registered Calibre libraries.
func cmdSource(ctx context.Context, cfg *config.Config, args []string) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		fs := flag.NewFlagSet("source add", flag.ContinueOnError)
		name := fs.String("name", "", "a short name for this library")
		path := fs.String("path", "", "path to the directory holding metadata.db")
		priority := fs.Int("priority", 100, "lower wins when several sources hold the same book")
		interval := fs.Int("interval", 900, "seconds between automatic scans")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *name == "" || *path == "" {
			return fmt.Errorf("source add: -name and -path are required")
		}
		abs, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		if _, err := calibre.Stat(abs); err != nil {
			return err
		}

		src := &store.Source{Name: *name, LibraryPath: abs, Priority: *priority,
			Enabled: true, ShareAll: true, ScanIntervalSec: *interval}
		id, err := store.CreateSource(ctx, st.Writer(), src)
		if err != nil {
			return err
		}
		fmt.Printf("added source %d (%s) -> %s\n", id, src.Name, src.LibraryPath)
		return nil

	case "list", "":
		sources, err := store.ListSources(ctx, st.Reader())
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			fmt.Println("no sources yet; add one with: kobibri source add -name main -path /path/to/library")
			return nil
		}
		fmt.Printf("%4s  %-14s %-9s %5s  %6s  %s\n", "id", "name", "status", "prio", "books", "path")
		for _, s := range sources {
			state := s.LastStatus
			if !s.Enabled {
				state = "disabled"
			}
			fmt.Printf("%4d  %-14.14s %-9.9s %5d  %6d  %s\n",
				s.ID, s.Name, state, s.Priority, s.BookCount, s.LibraryPath)
			if s.LastError != "" {
				fmt.Printf("      last error: %s\n", s.LastError)
			}
		}
		return nil

	case "enable", "disable":
		fs := flag.NewFlagSet("source "+sub, flag.ContinueOnError)
		id := fs.Int64("id", 0, "source id")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *id == 0 {
			return fmt.Errorf("source %s: -id is required", sub)
		}
		scanner := ingest.NewScanner(st, cfg.TmpDir())
		if err := scanner.SetSourceEnabled(ctx, *id, sub == "enable"); err != nil {
			return err
		}
		fmt.Printf("source %d %sd\n", *id, sub)
		return nil

	default:
		return fmt.Errorf("source: unknown subcommand %q (add, list, enable, disable)", sub)
	}
}

// cmdToken issues the secret that goes into a device's api_endpoint, and prints
// the exact line to put in Kobo eReader.conf.
func cmdToken(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	user := fs.String("user", "", "user to issue the token for (defaults to the only user)")
	label := fs.String("label", "", "a note about which device this is for")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	var u *store.User
	if *user != "" {
		u, err = store.GetUserByName(ctx, st.Reader(), *user)
	} else {
		u, err = onlyUser(ctx, st)
	}
	if err != nil {
		return err
	}

	raw, err := store.CreateAPIToken(ctx, st.Writer(), u.ID, *label)
	if err != nil {
		return err
	}

	base := "http://<this-server>:" + httpx.PortOf(cfg.Listen)
	if cfg.BaseURL != nil {
		base = strings.TrimSuffix(cfg.BaseURL.String(), "/")
	}

	fmt.Printf("token for %s: %s\n\n", u.Name, raw)
	fmt.Printf("On the Kobo, edit .kobo/Kobo/Kobo eReader.conf and set, under [OneStoreServices]:\n\n")
	fmt.Printf("  api_endpoint=%s/kobo/%s\n\n", base, raw)
	fmt.Print(`Back up that file first. If the device fails to resolve a hostname, use the
server's IP address instead. Keep TLS 1.2 enabled if you serve over HTTPS.
This secret is shown once and cannot be recovered; issue one per device so a
single one can be revoked without re-pairing the rest.
`)
	if cfg.BaseURL == nil {
		fmt.Print("\nSet KOBIBRI_BASE_URL to your public URL so the links kobibri hands the\n" +
			"device are correct behind a reverse proxy.\n")
	}
	return nil
}

// onlyUser returns the single user, bootstrapping one if the database is empty.
func onlyUser(ctx context.Context, st *store.Store) (*store.User, error) {
	n, err := store.CountUsers(ctx, st.Reader())
	if err != nil {
		return nil, err
	}
	switch n {
	case 0:
		id, err := store.CreateUser(ctx, st.Writer(), "admin", "", true)
		if err != nil {
			return nil, err
		}
		return store.GetUser(ctx, st.Reader(), id)
	case 1:
		var id int64
		if err := st.Reader().QueryRowContext(ctx, `SELECT id FROM users`).Scan(&id); err != nil {
			return nil, err
		}
		return store.GetUser(ctx, st.Reader(), id)
	default:
		return nil, fmt.Errorf("several users exist; pass -user")
	}
}

// cmdIngest scans a registered source into the canonical library.
func cmdIngest(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	id := fs.Int64("source", 0, "source id (0 scans every enabled source)")
	force := fs.Bool("force", false, "scan even if metadata.db is unchanged")
	confirm := fs.Bool("confirm-vanish", false, "accept a mass disappearance the guard would refuse")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	sources, err := store.ListSources(ctx, st.Reader())
	if err != nil {
		return err
	}

	scanner := ingest.NewScanner(st, cfg.TmpDir())
	opts := ingest.ScanOptions{Force: *force, ConfirmVanish: *confirm}
	var failed error

	for _, s := range sources {
		if (*id != 0 && s.ID != *id) || (*id == 0 && !s.Enabled) {
			continue
		}
		res, err := scanner.Scan(ctx, s.ID, opts)
		switch {
		case err != nil:
			fmt.Printf("%-14s ERROR  %v\n", s.Name, err)
			failed = err
		case res.Skipped:
			fmt.Printf("%-14s unchanged\n", s.Name)
		default:
			fmt.Printf("%-14s seen %d, added %d, updated %d, vanished %d\n",
				s.Name, res.Seen, res.Added, res.Updated, res.Vanished)
		}
	}

	var total, syncable int
	st.Reader().QueryRowContext(ctx,
		`SELECT count(*), COALESCE(sum(syncable), 0) FROM books WHERE merged_into IS NULL`).
		Scan(&total, &syncable)
	fmt.Printf("\nlibrary: %d books, %d syncable\n", total, syncable)
	return failed
}

// cmdConvert converts every imported book that has no KEPUB yet.
//
// The server does this in the background on its own; this exists so a library
// can be prepared before a device is ever pointed at it.
func cmdConvert(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	cache, err := kepubconv.NewCache(kepubconv.Options{
		Dir:         filepath.Join(cfg.CacheDir(), "kepub"),
		Store:       st,
		KepubifyBin: cfg.KepubifyBin,
	})
	if err != nil {
		return err
	}

	ebookCache, err := ebookconv.New(ebookconv.Options{
		Dir: filepath.Join(cfg.CacheDir(), "epub"), Store: st, Bin: cfg.EbookConvert,
	})
	if err != nil {
		return err
	}

	converted, err := kepubconv.NewPrewarmer(cache, st, ebookCache).Pass(ctx)
	if err != nil {
		return err
	}

	var cached, failed int
	st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kepub_cache`).Scan(&cached)
	st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM kepub_failures`).Scan(&failed)

	fmt.Printf("converted %d book(s) using %s\n", converted, cache.Impl())
	fmt.Printf("%d cached, %d could not be converted (served as the original EPUB)\n", cached, failed)
	return nil
}

// baseURLString is the public root, or "" when it has to be sniffed per request.
func baseURLString(cfg *config.Config) string {
	if cfg.BaseURL == nil {
		return ""
	}
	return strings.TrimSuffix(cfg.BaseURL.String(), "/")
}

// cmdImport downloads a book from a link and files it in the library.
func cmdImport(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	edition := fs.String("edition", "", "which translation to download; omit to list what is available")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("import: give the link to a book")
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	im, err := webimport.New(webimport.Options{Store: st, Root: cfg.ImportsDir()})
	if err != nil {
		return err
	}

	var failed error
	for _, url := range fs.Args() {
		if !im.Supports(url) {
			fmt.Printf("%s\n  no provider handles that link; supported: %s\n",
				url, strings.Join(im.Providers(), ", "))
			failed = fmt.Errorf("unsupported link")
			continue
		}

		// A title usually carries several translations, and they are different
		// texts. Downloading one at random would be a coin toss, so with more
		// than one on offer and none chosen, show them and stop.
		editions, err := im.Editions(ctx, url)
		if err != nil {
			fmt.Printf("%s\n  ERROR  %v\n", url, err)
			failed = err
			continue
		}
		if *edition == "" && len(editions) > 1 {
			fmt.Printf("%s\n  %d translation(s):\n", url, len(editions))
			for _, e := range editions {
				who := strings.Join(e.Teams, ", ")
				if who != "" {
					who = " — " + who
				}
				fmt.Printf("    %-12s %s%s (%d chapters)\n", e.ID, e.Name, who, e.Chapters)
			}
			fmt.Printf("  choose one with: kobibri import -edition <id> %s\n", url)
			continue
		}

		res, err := im.Import(ctx, url, webimport.ImportOptions{EditionID: *edition})
		if err != nil {
			fmt.Printf("%s\n  ERROR  %v\n", url, err)
			failed = err
			continue
		}

		what := "updated"
		if res.New {
			what = "imported"
		}
		fmt.Printf("%s\n  %s %q — %d chapter(s), %s\n",
			url, what, res.Title, res.Chapters, humanSize(res.Size))
		if res.Missing > 0 {
			fmt.Printf("  %d chapter(s) could not be downloaded and were left out\n", res.Missing)
		}
	}
	return failed
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// cmdScan reads a Calibre library and prints what kobibri sees, without
// touching either database. It is the fastest way to check a real library
// before wiring it up as a source.
func cmdScan(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	path := fs.String("path", "", "path to a Calibre library (the directory holding metadata.db)")
	limit := fs.Int("limit", 20, "how many books to print (0 for all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("scan: -path is required")
	}
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}

	db, err := calibre.Open(*path, cfg.TmpDir())
	if err != nil {
		return err
	}
	defer db.Close()

	stubs, err := db.Stubs(ctx)
	if err != nil {
		return err
	}
	ids := make([]int64, len(stubs))
	for i, s := range stubs {
		ids[i] = s.ID
	}

	books, err := db.Books(ctx, ids)
	if err != nil {
		return err
	}

	var withCover, withEPUB, missingFiles int
	for _, b := range books {
		if b.CoverRelPath != "" {
			withCover++
		}
		if f, ok := b.Format("EPUB"); ok && f.Present {
			withEPUB++
		}
		for _, f := range b.Formats {
			if !f.Present {
				missingFiles++
			}
		}
	}

	fmt.Printf("library: %s\n", db.LibraryPath())
	fmt.Printf("books: %d   with cover: %d   with a readable EPUB: %d   missing files: %d\n\n",
		len(books), withCover, withEPUB, missingFiles)

	shown := books
	if *limit > 0 && len(shown) > *limit {
		shown = shown[:*limit]
	}
	for _, b := range shown {
		formats := make([]string, 0, len(b.Formats))
		for _, f := range b.Formats {
			mark := ""
			if !f.Present {
				mark = "!"
			}
			formats = append(formats, f.Format+mark)
		}
		series := ""
		if b.HasSeries {
			series = fmt.Sprintf("  [%s #%g]", b.SeriesName, b.SeriesIndex)
		}
		cover := ""
		if b.CoverRelPath == "" {
			cover = "  (no cover)"
		}
		fmt.Printf("%6d  %-45.45s  %-28.28s  %v%s%s\n",
			b.ID, b.Title, strings.Join(b.AuthorNames(), ", "), formats, series, cover)
	}
	if len(shown) < len(books) {
		fmt.Printf("\n... %d more (use -limit 0 to print all)\n", len(books)-len(shown))
	}
	if missingFiles > 0 {
		fmt.Printf("\n! marks a format Calibre lists but whose file is not on disk\n")
	}
	return nil
}

func cmdServe(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", cfg.Listen, "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Listen = *listen

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	version, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	scanner := ingest.NewScanner(st, cfg.TmpDir())
	scheduler := ingest.NewScheduler(scanner, st)

	urls := httpx.URLBuilder{
		Base:       cfg.BaseURL,
		ListenPort: httpx.PortOf(cfg.Listen),
		TrustProxy: cfg.TrustProxy,
	}
	upstream := ""
	if cfg.ProxyEnabled() {
		upstream = cfg.ProxyUpstream
	}
	kepubCache, err := kepubconv.NewCache(kepubconv.Options{
		Dir:         filepath.Join(cfg.CacheDir(), "kepub"),
		Store:       st,
		KepubifyBin: cfg.KepubifyBin,
	})
	if err != nil {
		return err
	}
	coverCache, err := covers.NewCache(filepath.Join(cfg.CacheDir(), "covers"), st)
	if err != nil {
		return err
	}
	ebookCache, err := ebookconv.New(ebookconv.Options{
		Dir: filepath.Join(cfg.CacheDir(), "epub"), Store: st, Bin: cfg.EbookConvert,
	})
	if err != nil {
		return err
	}
	// Books held in another format are only offered to a device when there is
	// something here that can turn them into EPUB.
	ingest.SetConversionAvailable(ebookCache.Available())

	importer, err := webimport.New(webimport.Options{Store: st, Root: cfg.ImportsDir()})
	if err != nil {
		return err
	}
	uploads, err := upload.New(st, cfg.UploadsDir())
	if err != nil {
		return err
	}

	koboHandler := kobo.New(kobo.Options{
		Store: st, URLs: urls, ProxyUpstream: upstream,
		Kepub: kepubCache, Covers: coverCache, Ebook: ebookCache,
	})

	// Convert imported books in the background so the web UI can offer the
	// converted file and no device ever waits on a conversion mid-sync.
	prewarmer := kepubconv.NewPrewarmer(kepubCache, st, ebookCache)
	scheduler.OnScanComplete(prewarmer.Trigger)
	go prewarmer.Run(ctx)

	if err := scheduler.Start(ctx); err != nil {
		return err
	}
	defer scheduler.Stop()

	go runJanitor(ctx, st, kepubCache, coverCache, ebookCache, cfg)
	go importer.RunPeriodicRefresh(ctx, cfg.ImportCheckEvery)

	webServer, err := web.New(ctx, web.Options{
		Store: st, Scanner: scanner, Scheduler: scheduler,
		Kepub: kepubCache, Covers: coverCache, Prewarmer: prewarmer,
		Ebook: ebookCache, Imports: importer, Uploads: uploads,
		BaseURL: baseURLString(cfg), ListenAddr: cfg.Listen,
		AdminPassword: cfg.AdminPassword,
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/kobo/", koboHandler.Mount())
	mux.Handle("/", webServer.Mount())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := st.Reader().PingContext(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           httpx.RepairHost(httpx.PortOf(cfg.Listen), cfg.TrustProxy)(mux),
		ReadHeaderTimeout: 15 * time.Second,
		// WriteTimeout stays 0: book downloads set their own per-request write
		// deadline, and a slow Wi-Fi transfer must not be killed mid-file.
		IdleTimeout: 120 * time.Second,
	}

	if cfg.TLSCert != "" {
		// Kobo firmware ships an old TLS stack: a TLS-1.3-only profile is
		// rejected at handshake, and HTTP/2 has been seen to confuse it. Serving
		// HTTP/1.1 over TLS 1.2 is what actually works on a device.
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "data_dir", cfg.DataDir,
			"schema", version, "tls", cfg.TLSCert != "")
		if cfg.TLSCert != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
			return
		}
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	}
}
