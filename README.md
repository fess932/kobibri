# kobibri

Serves one or more Calibre libraries to Kobo e-readers by emulating the Kobo store
sync API. Point a device's `api_endpoint` at kobibri and your books arrive over Wi-Fi,
as KEPUB, with covers and reading progress.

> Work in progress. The whole sync path works end to end against a simulated device:
> libraries are ingested, books download as KEPUB with covers, reading progress
> syncs between devices, and deleting a book on one Kobo leaves it on another.
> Collections sync too. The web UI is next. See [docs/PROGRESS.md](docs/PROGRESS.md).

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
| M9 Web UI | in progress |
| M10 Hardening and packaging | |

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

# Issue a device token; this prints the exact api_endpoint line to use.
./kobibri token -label "clara 2e"

KOBIBRI_BASE_URL=http://192.168.1.10:8078 ./kobibri serve
```

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
| `KOBIBRI_KEPUBIFY_BIN` | use an external kepubify instead of the built-in library |

Notes for real deployments: Kobo's TLS stack is old, so keep TLS 1.2 enabled; some
firmware fails to resolve hostnames where a raw IP works; reverse proxies need
enlarged buffers.

## Documentation

- [docs/PLAN.md](docs/PLAN.md) — architecture, schema, the sync engine, milestones
- [docs/kobo-protocol.md](docs/kobo-protocol.md) — the reverse-engineered Kobo API,
  including the quirks that fail silently
- [docs/PROGRESS.md](docs/PROGRESS.md) — what is built, what is known to be risky

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
