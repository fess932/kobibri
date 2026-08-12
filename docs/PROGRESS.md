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
| M10 | Hardening, janitors, Docker, systemd | done |
| M11 | Importing books from a link | done |

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

### M10 — hardening and packaging

TLS served directly with 1.2 as a minimum and HTTP/2 off, because Kobo firmware cannot
negotiate a 1.3-only server and has been seen to trip over HTTP/2. Half a certificate
pair is now a startup error rather than a silent fall back to plain HTTP.

An hourly janitor trims the rebuildable caches to their budgets and clears abandoned
snapshots older than a week and expired sessions. A device's single completed snapshot is
never touched — it is the baseline its next sync diffs against.

A multi-stage Dockerfile, a hardened systemd unit, a commented environment file and an
nginx fragment. Verified that `CGO_ENABLED=0` produces a statically linked binary for
linux/amd64 and linux/arm64 — that is the whole reason for the cgo-free SQLite driver.
The Docker image itself was **not** built: no daemon was available here.

**The soak test** is the most valuable test in the project. Two sources, two devices,
books edited, a book deleted from Calibre, a source switched off and back on, one device
deleting a book on itself, and syncs interrupted at four different cursor positions with
the device restarting each time — losing its token but not its files. The invariant it
asserts is the one everything else exists to provide: **a device never loses a book it was
given, and is only ever told to archive one it deleted itself.**

Both of its first failures were the test being wrong rather than the server: it modelled a
restarted device as one with an empty library, and it treated a legitimate self-deletion as
data loss. Worth recording because both are easy mistakes to make again.

### M11 — importing from a link

`internal/webimport` on top of `github.com/fess932/novelkit`, plus `kobibri import <url>`.
The provider abstraction sketched in this backlog turned out to already exist upstream, and
in a richer form — search, translations, resumable downloads, EPUB assembly. So kobibri
uses it rather than defining its own; adding a site is a change to novelkit.

Two bugs, both mine:

1. **`job.Store.Open` takes a full path**, and the job directory was being stored as a bare
   name. The reopen failed, and because the same variable answered both "is the cache
   there" and "is this book new", a re-import reported itself as a fresh one. Those are
   different questions and are now separate.
2. **Reusing a planned job never picks up new chapters** — its chapter list is fixed when
   it is planned. `Plan` is already idempotent and additive: it derives the cache directory
   from the book, keeps what is downloaded and appends whatever is new. Always planning is
   both simpler and correct.

The schema test caught the new table on its own, which is what it is for.

**Does a serial update in place, keeping the reader's position?** Measured rather than
assumed, because the answer was not obvious. It does, on both counts. The canonical id
never changes, so the device updates the entitlement instead of showing a second book. And
a position is a koboSpan id inside a named content document, so it only survives if earlier
chapters keep their filenames and their bytes — adding chapters leaves every earlier
chapter byte-identical and every span id in it unchanged; only the table of contents moves,
which it must.

Two tests pin this down. It is a property of novelkit's assembly and kepubify's numbering,
not of anything in kobibri, so if either ever changes the tests are what will say so —
otherwise every saved position in every imported serial would silently move.

Verified offline against a fake `novel.Source`: a book is filed, is syncable as KEPUB, has
a real EPUB behind it; re-importing lands on the same canonical book, downloads only the
new chapters and moves `metadata_rev`; an unsupported link is refused before anything is
created. Against the real site only the refusal path was exercised — no actual title was
downloaded here.

### Translations, and the shape of the flow

A title usually carries several translations. They are different texts, so picking one is
not a detail: the browser asks the site what exists, shows them with their teams and
chapter counts, and downloads only after a choice. The translation is part of the identity
key, so two of them are two books.

Verified against the live site: the real link carries four translations, from 17 to 550
chapters. One of them has none published, and the interface offers no button for it.

Downloads run in the background with progress, because a 550-chapter serial cannot be
fetched inside a request. New chapters are picked up on a timer, default once a day,
one book at a time.

Reworking that turned up a race worth naming: the periodic check and the button on the page
could both start the same book, writing one cache directory and one assembled file from two
goroutines. Everything now goes through a single claim per book and translation, so a
duplicate is refused rather than run.

### Formats other than EPUB

Books the library holds as FB2, AZW3, MOBI and so on now reach a reader: they go through
Calibre's `ebook-convert` to EPUB and on to KEPUB, cached at each step, converted in the
background so a device never waits mid-sync. PDF, CBZ and DJVU stay out — Kobo does not
sync them, and converting a scan produces something nobody wants to read.

The governing rule is that a book is only advertised when it can be delivered. Without a
converter, such books are not offered at all.

A test caught a bug in exactly that rule: an explicitly configured but non-existent
`ebook-convert` was reported as available, so every book in another format would have been
advertised and every one of those downloads would have failed. Availability is now
established by resolving the binary, not by trusting the setting.

### M12 — shelves from the library's own organisation

Calibre's tags and series can now be mirrored onto a reader's collections. The collections
machinery was already there from M8 and only ever served shelves a device made for itself;
this feeds it from the library.

It is **off by default**, and that is the point rather than caution: a library with two
hundred tags would put two hundred shelves on someone's Kobo without being asked. The
setting lives on the Libraries page — tags, series, both, or nothing — and applies at once,
because a setting that only takes effect after the next scan looks broken.

Three rules make it safe to run after every scan:

- **A rebuild that changes nothing bumps no revision.** The diff compares revisions, so a
  pass that touched every shelf would re-announce the lot to every device on every scan.
  Membership is compared before it is written.
- **A shelf someone deleted on their reader stays deleted.** Putting it back on the next
  scan is an argument nobody can win. Deletions this code makes itself are marked with a
  different origin, so a tag that leaves Calibre and comes back is rebuilt, while one a
  reader threw away is not.
- **Shelves a device made for itself are never touched.** Only rows with a `calibre`
  origin are managed here.

Collections are per user, because visibility is: two people sharing a server see the books
their sources allow, and their shelves follow from that.

### A KEPUB is not converted again

The book page listed two KEPUB rows for a library that already held one — the library's own
file and a "converted" one — pointing at the same URL and described identically. Behind
that were two real faults:

- A book the library holds **only** as a KEPUB was not syncable at all. `applyDownload`
  looked for an EPUB and gave up; KEPUB is not in the convertible list either. Such a book
  is now served untouched, marked `convert_from = 'KEPUB'`, which every path reads as "there
  is nothing left to do to this". Running kepubify over it a second time would nest
  koboSpan ids inside each other and lose the reading position.
- The download list described a row by its format, so the library's own KEPUB was labelled
  "converted for Kobo". Each row now carries the phrase that belongs to it, built where the
  language is known.

When a library holds both an EPUB and a KEPUB the EPUB wins, because the conversion this
server makes is the one it can prewarm, cache and rebuild on demand.

### Phrases that name something

`T` only translated fixed phrases, so anything that had to mention a library or a count was
assembled in Go and shipped in English — including the dashboard warnings, where a Russian
prefix was concatenated with an English sentence and read as nonsense.

`Msg(key, arg)` now packs the value into the string after a separator and `T` unpacks it,
which is what lets a flash message survive the round trip through a redirect and still be
translated on the far side. Only one argument is supported, deliberately: every phrase here
names exactly one thing. `TestEveryPageRenders` fails if a separator ever reaches the page,
which is the tell-tale of a phrase that is not in the catalogue.

## Notes for novelkit

Found while integrating. On **v0.4.1** two of the three are fixed.

1. ~~**`ParseSlug` does not handle `/ru/manga/<slug>`**~~ — fixed in v0.4.1. It now walks
   the path segments and takes the first that looks like a slug, which covers that shape
   and any future section name. Verified: the full `/ru/manga/…` link resolves and lists
   its translations.

2. ~~**`Registry.Resolve` reports two different failures the same way**~~ — fixed in
   v0.4.1 with `ErrBadReference` alongside `ErrUnsupported`. kobibri dropped its own
   workaround and maps the library's two errors instead.

3. **Still open: an empty edition and the explicit id of the same edition get different
   cache directories.** `dirName` in `job/job.go` returns `…--default` when the edition is
   empty. Import a title without choosing, then import it again naming the translation it
   had defaulted to, and everything downloads a second time. Resolving an empty edition to
   its concrete id before naming the directory would fix it.

   Exposure here is small: the browser flow always names a translation, picking the only
   one itself when there is just one. Only the command line can pass an empty edition.

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

- **Our own EPUB → KEPUB conversion**, with the differential span-id test described above.
- **Reading a book in the browser.** Enough to leaf through the pages: unzip the KEPUB,
  follow the spine, serve each chapter with its own resources. It does not have to be a
  good reader — it has to answer "is this file actually all right?" without a Kobo in hand.
- **A duplicate report** based on content hashes, plus a way to split books merged in
  error.
- **Calibre custom columns** mapped onto `Genre`.
- **An OPDS feed.**
