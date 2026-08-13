# kobibri

Serves one or more Calibre libraries to Kobo e-readers by emulating the Kobo store
sync API. Point a device's `api_endpoint` at kobibri and your books arrive over Wi-Fi,
as KEPUB, with covers and reading progress.

> Work in progress. The whole sync path works end to end against a simulated device:
> libraries are ingested, books download as KEPUB with covers, reading progress
> syncs between devices, and deleting a book on one Kobo leaves it on another.
> Everything the plan set out is built and tested. It has not yet been run against
> a physical Kobo — the sync conversation is exercised against a simulated device.
> See [docs/PROGRESS.md](docs/PROGRESS.md).

## Why another one

Existing self-hosted implementations track what a device has using timestamp
high-water marks. That design cannot express deletions, loses books when a paged
sync is interrupted, and keeps one set of state for all of a user's devices.

kobibri syncs by **reconciling immutable snapshots** instead. Each sync materialises
what the device should hold and sends the difference from the previous snapshot. Three
properties fall out of that:

- **A book that disappears from the server stays on the device.** It is still in the
  previous snapshot, so the difference is empty and nothing is sent. No error, no
  re-download, no lost file.
- **Deleting a book on one Kobo does not delete it on another.** The deletion is
  recorded as a per-device tombstone, permanently, and that device is never offered
  the book again.
- **An interrupted sync resumes without losing books,** because the snapshot it is
  draining cannot change underneath it — not even while a library scan is running.

Canonical book ids are issued once and never reissued. Remove a library and add it
back and the device sees no change at all.

## Multiple libraries

Several Calibre libraries merge into one. Rows are matched by Calibre uuid, then by
ISBN (checksum-validated, because hand-typed ISBNs are common), then by normalised
title and author. The highest-priority source wins the record as a whole — no
Frankenstein metadata — with only empty fields filled in from the others, and a
source that actually has a readable file beats one that does not.

## Status

| Milestone | |
|---|---|
| M1 Skeleton, store, migrations | done |
| M2 Calibre reader | done |
| M3 Ingest, identity, merge | done |
| M4 Kobo HTTP layer, auth, initialization, proxy | done |
| M5 Sync engine | done |
| M6 Downloads, kepub, covers | done |
| M7 Pagination, reading state, deletion | done |
| M8 Collections | done |
| M9 Web UI | done |
| M10 Hardening and packaging | done |
| M11 Importing books from a link | done |

## Try it

```sh
go build ./cmd/kobibri

# Look at a library without registering it — read-only, never writes to Calibre.
./kobibri scan -path ~/Calibre\ Library

# Register sources and ingest them.
./kobibri source add -name main -path ~/Calibre\ Library -priority 10
./kobibri ingest
./kobibri source list

# Convert imported books to KEPUB ahead of time. The server also does this in
# the background, so this is only needed to prepare a library up front.
./kobibri convert

# Books published on the web can be imported by link — a title usually carries
# several translations, so the browser asks which one you want. They join the
# library as ordinary books, and new chapters are picked up on a timer.
./kobibri import https://ranobelib.me/ru/book/...

# Issue a device token; this prints the exact api_endpoint line to use.
./kobibri token -label "clara 2e"

KOBIBRI_ADMIN_PASSWORD=... KOBIBRI_BASE_URL=http://192.168.1.10:8078 ./kobibri serve
```

Then open the server in a browser to manage libraries, browse the library, download
a book as EPUB or KEPUB, and set up readers. The interface is in English or Russian —
it follows the browser and can be switched by hand.

Then on the Kobo, in `.kobo/Kobo/Kobo eReader.conf` under `[OneStoreServices]`:

```ini
api_endpoint=http://192.168.1.10:8078/kobo/<token>
```

**Back that file up first.** The device caches the endpoint map it receives into that
file permanently, so a bad response has to be repaired by hand.

### Configuration

| Variable | |
|---|---|
| `KOBIBRI_DATA_DIR` | database and caches |
| `KOBIBRI_LISTEN` | listen address, default `:8078` |
| `KOBIBRI_BASE_URL` | public URL; set this behind any reverse proxy |
| `KOBIBRI_TRUST_PROXY` | honour `X-Forwarded-Proto` / `X-Forwarded-Host` |
| `KOBIBRI_PROXY_UPSTREAM` | Kobo store for unimplemented endpoints; `off` disables |
| `KOBIBRI_KEPUBIFY_BIN` | convert with an external kepubify instead of the built-in converter |
| `KOBIBRI_IMPORT_CHECK_EVERY` | how often to look for new chapters, default `24h`, or `off` |
| `KOBIBRI_EBOOK_CONVERT` | Calibre's converter, for books not already in EPUB |
| `KOBIBRI_ADMIN_PASSWORD` | creates the first account on a fresh install |
| `KOBIBRI_TLS_CERT`, `KOBIBRI_TLS_KEY` | serve HTTPS directly instead of behind a proxy |
| `KOBIBRI_KEPUB_CACHE_BYTES` | how much converted-book cache to keep, default 4 GiB |
| `KOBIBRI_EPUB_CACHE_BYTES` | the same for books converted from another format |
| `KOBIBRI_COVER_CACHE_BYTES` | the same for scaled covers, default 1 GiB |
| `KOBIBRI_LOG_LEVEL` | `debug`, `info` (default), `warn`, `error` |

## Reading somewhere else

`/opds` is a catalogue feed for reading apps that are not a Kobo — KOReader, Foliate,
Moon+ and the rest. Add `https://your-server/opds` as an OPDS catalogue and sign in
with your kobibri name and password. Each person sees the books their sources allow,
the same ones they would get on a device.

## Deploying

```sh
cd deploy && docker compose up -d
```

Images are built on every push to `main` and published to
`ghcr.io/fess932/kobibri` for `linux/amd64` and `linux/arm64`. Tags: `latest`,
`main`, and the version for each release tag.

`CGO_ENABLED=0` and the cgo-free SQLite driver mean the image is one static binary
on Alpine, so it runs on a NAS or a Raspberry Pi without a C toolchain anywhere.

Keep the data volume. It holds the canonical book ids, which are what your readers
have; losing it makes every device treat the whole library as new. It also holds the
only copy of anything you uploaded by hand or imported from the web — Calibre has
those nowhere.

Docker, with whatever reverse proxy you already have in front. There is a
[compose file](deploy/compose.yaml) and a [Caddyfile](deploy/Caddyfile) to start from.

EPUB, KEPUB and **FB2** need nothing installed — FB2 is converted by kobibri
itself. Kindle and other formats (AZW3, MOBI, LIT, DOCX, RTF, TXT) go through
Calibre's `ebook-convert`, which the image does not ship, since it would multiply
the image size for something many libraries never need. To sync those too, use an
image with Calibre in it and point `KOBIBRI_EBOOK_CONVERT` at the binary. The
uploads page lists what the running server can actually convert.

Three things about real deployments come from the device rather than from taste:

- **Keep TLS 1.2 enabled.** Kobo firmware ships an old TLS stack and cannot negotiate
  a TLS-1.3-only server. When kobibri serves HTTPS itself it also turns HTTP/2 off.
- **Do not buffer sync responses.** They are large, and a proxy that buffers with
  small defaults gives the device a 502 partway through. Caddy streams by default;
  nginx needs its `proxy_buffers` raised.
- **Some firmware fails to resolve hostnames.** If a reader cannot reach the server,
  point `api_endpoint` at its IP address instead.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — the design and the reasoning behind it
- [docs/kobo-protocol.md](docs/kobo-protocol.md) — the reverse-engineered Kobo API,
  including the quirks that fail silently
- [docs/PROGRESS.md](docs/PROGRESS.md) — what is built, what it cost, what is still open

## Credits

The Kobo sync API has no official documentation. The protocol reference was assembled
by reading [calibre-web](https://github.com/janeczku/calibre-web),
[Calibre-Web-Automated](https://github.com/crocodilestick/Calibre-Web-Automated),
[Komga](https://github.com/gotson/komga), [kobink](https://github.com/potatoeggy/kobink)
and [kobo-book-downloader](https://github.com/subdavis/kobo-book-downloader), whose
authors did the hard reverse-engineering. Conversion to KEPUB uses
[kepubify](https://github.com/pgaskin/kepubify).

No code from those projects is vendored here.

## License

MIT
