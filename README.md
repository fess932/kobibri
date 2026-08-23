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
| M4 Kobo HTTP layer, auth, initialization | done |
| M5 Sync engine | done |
| M6 Downloads, kepub, covers | done |
| M7 Pagination, reading state, deletion | done |
| M8 Collections | done |
| M9 Web UI | done |
| M10 Hardening and packaging | done |
| M11 Importing books from a link | done |

## Try it

There is a `Makefile` for the usual things — `make build`, `make run`, `make test`,
`make race`, `make check`, `make migrate`, `make docker`; `make help` lists them.
`make run DATA=/srv/kobibri BASE_URL=http://192.168.1.10:8078` is the short way to
start a server. It works on Windows as well as on a shell.

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

The **Series** tab lists every series and shows one in reading order. A series and a
book's number in it can be set there when the library has it wrong or missing; the edit
is kept beside the library rather than in it, so nothing is ever written to Calibre and
the next scan does not undo it. Whether series also become shelves on the reader is a
separate setting, under Libraries.

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
| `KOBIBRI_PROXY_UPSTREAM` | Kobo store tried first for endpoints kobibri cannot answer from the library; `off` disables |
| `KOBIBRI_KEPUBIFY_BIN` | convert with an external kepubify instead of the built-in converter |
| `KOBIBRI_IMPORT_CHECK_EVERY` | how often to look for new chapters, default `24h`, or `off` |
| `KOBIBRI_EBOOK_CONVERT` | Calibre's converter, for books not already in EPUB |
| `KOBIBRI_ADMIN_PASSWORD` | creates the first account on a fresh install |
| `KOBIBRI_TLS_CERT`, `KOBIBRI_TLS_KEY` | serve HTTPS directly instead of behind a proxy |
| `KOBIBRI_KEPUB_CACHE_BYTES` | how much converted-book cache to keep, default 4 GiB |
| `KOBIBRI_EPUB_CACHE_BYTES` | the same for books converted from another format |
| `KOBIBRI_COVER_CACHE_BYTES` | the same for scaled covers, default 1 GiB |
| `KOBIBRI_LOG_LEVEL` | `debug`, `info`, `warn`, `error`. The binary defaults to `info`, the container image to `debug` |
| `KOBIBRI_TRACE_BODY_BYTES` | how much of each request and response body the debug trace keeps, default 4096 |

The Kobo API this server speaks is documented at `/api` in the interface, and the OpenAPI
document behind that page is at `/api/kobo.json` for anything that reads one. It covers both
what kobibri answers itself and everything it will relay to the Kobo store — that second
list is derived from the built-in resource map, so it stays in step on its own.

`/v1/initialization` hands the reader a resource map, which it writes into
`[OneStoreServices]` in its own `Kobo eReader.conf` and keeps. kobibri ships one built in —
144 keys captured from a real response — and lays its own overrides on top of it.

To serve a different one, put it at `<data>/kobo_resources.json`. It may be JSON, or the
`[OneStoreServices]` section copied out of a device's `Kobo eReader.conf` as it is; that
section is the best map there is, because it already matches that reader's firmware and
region. Anything the Kobo store ever hands over is written to the same file automatically.

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

### Permissions

The container runs as **uid 10001**, not root.

A **named volume** — what the compose file uses — needs nothing: Docker creates it
from the image, where `/data` is already owned by that user.

A **bind mount** does not inherit that; the host directory's own ownership wins, so
prepare it once:

```sh
sudo mkdir -p /srv/kobibri
sudo chown -R 10001:10001 /srv/kobibri
```

Your Calibre library is mounted read-only and is never written to. It only has to be
*readable* by that uid, which a library with the usual `0755` directories already is.
If yours is locked down to its owner, either loosen the directories or run the
container as yourself with `user: "1000:1000"` — in which case the data directory has
to be yours too.

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
