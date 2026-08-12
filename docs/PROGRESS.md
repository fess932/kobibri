# Progress

Design and reasoning: [ARCHITECTURE.md](ARCHITECTURE.md).
The Kobo protocol: [kobo-protocol.md](kobo-protocol.md) — sections marked **LANDMINE**
are where a mistake fails silently rather than loudly.

This file is the working journal: what is built, what it cost, and what is still open.

## Milestones

| M | | Status |
|---|---|---|
| M1 | Skeleton, config, store, migrations, `/healthz`, graceful shutdown | done |
| M2 | Calibre reader: snapshot-copied `metadata.db`, two-phase scan, file resolution | done |
| M3 | Ingest: identity, merging, winner selection, revisions, scheduling | done |
| M4 | Kobo HTTP layer: routing, auth, initialization, proxy | done |
| M5 | Sync engine: snapshots, diff, entitlements, metadata | done |
| M6 | Downloads, KEPUB conversion, covers | done |
| M7 | Pagination, reading progress, on-device deletion | done |
| M8 | Collections | done |
| M9 | Web interface, multi-user, localisation | done |
| M10 | Hardening, janitors, Docker, systemd | in progress |

## What each milestone cost

### M1 — skeleton

`internal/config` (environment plus defaults), `internal/store` (two pools: a
single-connection writer and a reader pool; `Tx` with rollback), stepwise migrations over
`PRAGMA user_version` from `embed.FS` — each in its own transaction together with its
version bump — the full 23-table schema, `serve` and `migrate`, structured logging,
graceful shutdown, `/healthz`.

The server's `WriteTimeout` is deliberately zero: a book download sets its own per-request
write deadline, otherwise a large file over slow Wi-Fi is cut off part-way.

### M2 — reading Calibre

`Open` with the snapshot copy, `Stat` for a cheap pre-check, two-phase reads, file and
cover resolution, a path guard against escaping the library root, and a lenient timestamp
parser. Errors split into `ErrUnreachable` and `ErrCorrupt`.

`calibretest` was pulled out into its own package rather than a `_test.go` file, because
ingest, sync and web tests all needed it later.

**The WAL test was initially worthless.** It skipped silently: modernc checkpoints and
deletes `-wal` when the last connection closes, so there was nothing left to prove. The
fixture now holds a connection open and fails if no `-wal` appears — only then does the
test actually verify that the WAL is copied alongside the database.

### M3 — ingest

Identity keys, merging with permanent aliases, whole-record winner selection with
empty-field fallback, `serving_hash` and revision bumping, the vanish guard, and a
scheduler with jitter, a global scan slot and exponential backoff.

Two bugs the tests found:

1. **Enabling or disabling a source did not re-resolve its books.** A scan could not fix
   it either: nothing had changed in Calibre, so the books never entered the changed set,
   and `syncable` stayed zero forever. The flag and the re-resolve are now one operation
   that cannot be separated.
2. **A new book was created at revision 2** — the empty `serving_hash` on creation counted
   as a change. The first resolve no longer moves the revision.

### M4 — the Kobo HTTP layer

Routing, token authentication with a cache and immediate invalidation, device
registration, `/v1/auth/*`, `/v1/initialization`, the store proxy, `Host` repair, and
token redaction in logs.

**`r.PathValue` is empty in middleware** — it is only populated once ServeMux has matched
a route, and authentication runs ahead of the mux. The token came back empty and the
server silently rejected every request. The tests showed it as zero devices and an empty
resource map.

**Verified 2026-08-12:** `storeapi.kobo.com/v1/initialization` answers **401** without
device credentials. A server cannot fetch the native resource map on its own, so kobibri
does not vendor a copy of anyone else's. It fetches the map using the credentials the
device sends on its own initialization request, and otherwise sends only its own keys.

### M5 — the sync engine

Wire types, the sync token, snapshot creation and diffing, the drain state machine, and
`/v1/library/{uuid}/metadata`.

Two bugs, both striking exactly the properties the design exists to provide:

1. **A vanished book still produced a change event.** `Resolve` cleared the download
   format, which moved `serving_hash`, which bumped the revision, which put the book in
   the changed category and re-announced it with no download URL. Serving metadata is now
   frozen when a book goes unavailable, and availability is no longer part of the hash.
2. **Hiding a book did not take it off the device** — the carry-forward pulled it straight
   back into the snapshot. Carry-forward now skips hidden books. The distinction is
   deliberate: the union absorbs accidental disappearance, hiding is intentional removal.

### M6 — downloads, conversion, covers

The `Converter` interface with two implementations, the conversion cache with
`singleflight` and LRU eviction, the background prewarmer, fixed-layout detection from the
OPF, the cover pipeline, and the download and cover handlers.

kepubify's API turned out to be `Convert(ctx, io.Writer, fs.FS)`, so it is used as a
library and the subprocess path stays as an escape hatch.

### M7 — pagination, progress, deletion

The batch size became configurable, which let the continuation path be exercised on a
small fixture instead of a hundred-book one. Reading progress round-trips with the kobo
span location intact, a finished book no longer stores the bogus first-resource position
the device sends, and on-device deletion records a permanent per-device tombstone.

### M8 — collections

Five endpoints, soft deletion, revision bumping on membership changes, and owner checks.

**A genuine routing collision.** Renaming a collection is `PUT /v1/library/tags/{id}` and
reporting progress is `PUT /v1/library/{uuid}/state` — both are `/v1/library/X/Y`, so no
routing table can separate them and ServeMux panics at registration. Neither registration
order nor a more specific third pattern helps, since conflicts are checked pairwise. One
handler takes both and dispatches on the segments.

### M9 — the web interface

Sessions, bcrypt, CSRF, seven pages, downloads of the original and the converted file,
tombstone and sync-state escape hatches, and localisation in English and Russian.

**A template that calls a function the map does not have compiles fine** and only fails
when the template is parsed at startup — so the binary built, the tests passed, and the
server refused to start. The cause was an edit of my own: `gofmt` realigned the func map's
keys before a scripted substitution ran, the anchor no longer matched, and the edit
silently did nothing. There is now a test that renders every page and fails on a catalogue
key leaking into the output, which catches both that and a missing translation.

## Decisions

### Converting formats to KEPUB

Two stages, and they should not be conflated.

**Any format → EPUB.** Not ours to write. Fifteen years of per-format workarounds already
exist in Calibre, which is where the library comes from anyway; calling `ebook-convert` as
a subprocess is the plan. PDF, CBZ and DJVU stay out of scope — Kobo does not accept them
over sync at all.

**EPUB → KEPUB.** Writing our own does make sense eventually. The task is small and
well-bounded: wrap text nodes in `koboSpan` elements, add Kobo's wrappers and CSS, adjust
the OPF, repack the zip with `mimetype` first and uncompressed. On the order of 500 lines.

Why it is worth doing: kepubify has had no release since March 2022 and no commit since
May 2022, with a number of open issues, and we depend on it at exactly one point.

Why **not yet**: the danger is not making it work, it is making it *stable*.
`Location.Value = "kobo.N.M"` is the anchor for reading position, stored on the device. If
our segmentation ever diverges from our own earlier segmentation, every saved position in
the library moves. So the replacement needs a differential test first — run a corpus
through kepubify and through ours, compare span ids, and only switch when they agree. That
test is a precondition, not a nicety.

The machinery for the swap is already in place: the `Converter` interface, two
implementations, and `KOBIBRI_KEPUBIFY_BIN` to switch at runtime.

### When conversion happens

Every imported book should end up with a KEPUB, so the web interface can offer it and no
device ever waits on a conversion mid-sync.

That runs on a background queue, not inside a scan. Converting a whole library
synchronously at import is what makes other implementations' first sync take an age, and
it would hold the single SQLite writer connection for the duration. The outcome is the
same — the files exist shortly after import — but the scan finishes at once. The queue is
kicked after every productive scan and every fifteen minutes; `kobibri convert` runs it by
hand. Books whose conversion failed are remembered and not retried on every pass.

## Closed risks

- **`modernc.org/sqlite` DSN syntax.** Verified on v1.56.0 (SQLite 3.53.3):
  `_pragma=name(value)`, plus `_txlock=immediate` and `mode=ro`. Not mattn's
  `_busy_timeout=…` form. Incidentally SQLite 3.53 is well past 3.44, so ordered
  `group_concat` would be available — but the Go-side join stays, to avoid depending on
  the bundled version.
- **The kepubify API.** `Convert(ctx context.Context, w io.Writer, r fs.FS) error`, taking
  the EPUB zip as an `fs.FS`. No subprocess needed, though one remains available.

## Open risks

- **Dependence on an unmaintained kepubify.** It works, and a test proves koboSpan output,
  but there have been no releases since 2022. Replacement plan above.
- **False merges on `titleauthor`.** Different translations, or several books called
  "Selected Poems". It is the weakest key and the UI shows contributing sources per book.
  If false positives show up in practice, make that key opt-in per pair of sources.

## Backlog

- **Import a book from a link.** A new source kind: paste a title's URL, the server pulls
  the chapters, builds an EPUB and files it as an ordinary book — from there the normal
  path applies (identity → KEPUB → sync).

  Downloading goes through `github.com/fess932/novelkit`. On top of it there needs to be a
  **provider interface** so different sites plug in uniformly, roughly:

  ```go
  type Provider interface {
      Match(u *url.URL) bool                                     // is this site mine?
      Title(ctx context.Context, u *url.URL) (TitleInfo, error)  // metadata and chapter list
      Chapter(ctx context.Context, ref ChapterRef) (Chapter, error)
  }
  ```

  The EPUB builder and everything downstream stay site-agnostic. Fetching new chapters
  has to bump `metadata_rev` so an updated title reaches the reader on its own. Open
  question: identity for such books — there is no Calibre uuid and no ISBN, so they need a
  key of their own, something like `weburl:<canonical url>`.

- **Our own EPUB → KEPUB conversion**, with the differential span-id test described above.
- **Format normalisation via `ebook-convert`**, so books that are not already EPUB sync.
- **Mapping Calibre tags and series onto Kobo collections.**
- **A duplicate report** based on content hashes, plus a way to split books merged in
  error.
- **Calibre custom columns** mapped onto `Genre`.
- **An OPDS feed.**
