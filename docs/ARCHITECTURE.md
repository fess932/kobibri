# Architecture

kobibri reads one or more Calibre libraries off the filesystem and serves them to Kobo
e-readers by emulating the Kobo store sync API. This describes what is built and, more
usefully, *why* — the decisions that are not obvious from the code.

The Kobo protocol itself is documented separately in [kobo-protocol.md](kobo-protocol.md).
Current status and the working journal are in [PROGRESS.md](PROGRESS.md).

## Guiding principles

1. **The device is the fragile party.** Every failure mode under `/kobo/` answers
   `200 {}`, never an error status. An error on any endpoint — even an incidental one —
   makes the device abandon the entire sync.
2. **Reconciliation, not deltas.** Server state is materialised into immutable snapshots
   and the wire protocol is a diff of two of them. No timestamp watermarks anywhere in
   the sync path.
3. **Canonical book ids are forever.** They are the only identifier a device knows.
   Nothing in ingest may delete or reissue one.
4. **Few dependencies, one static binary.** Standard library first; every dependency has
   to earn its place.

## Package layout

```
cmd/kobibri/          serve | migrate | source | ingest | convert | token | scan
internal/
  config/             bootstrap settings from the environment
  store/              the server's own SQLite database
  calibre/            reading a Calibre library (never writing to one)
    calibretest/      builds real Calibre libraries on disk, for tests
  ingest/             identity, merging, winner selection, scan scheduling
  kobo/               the Kobo store sync API
  kepubconv/          EPUB → KEPUB conversion, cached and prewarmed
  covers/             cover scaling and caching
  httpx/              HTTP plumbing shared by the Kobo API and the web UI
  web/                the browser interface
```

## Dependencies

| Concern | Choice | Why |
|---|---|---|
| Router | stdlib `net/http.ServeMux` | Go 1.22 patterns; literal segments beat wildcards, which is what keeps `/v1/library/tags` out of the book-deletion route |
| SQLite | `modernc.org/sqlite` | cgo-free, so `CGO_ENABLED=0` produces a static binary for a NAS or a Raspberry Pi. Used for both our database and Calibre's |
| Migrations | hand-rolled over `PRAGMA user_version` + `embed.FS` | goose and golang-migrate pull in large trees for something trivially small |
| UUID | `github.com/google/uuid` | v3 over `NAMESPACE_DNS` is needed for `Series.Id`, and it has to match other implementations bit for bit |
| KEPUB | `github.com/pgaskin/kepubify/v4/kepub` | see [Conversion](#conversion) |
| Image scaling | `golang.org/x/image/draw` | CatmullRom, official, small |
| Concurrency | `golang.org/x/sync` | `singleflight` and `semaphore`, exactly what the conversion cache needs |
| Passwords | `golang.org/x/crypto/bcrypt` | web UI only |
| Templates | `html/template` + `embed` | no build step, no external assets, no SPA/API duplication |

Note on `modernc.org/sqlite`: its DSN pragma syntax is `_pragma=name(value)`, not
mattn's `_busy_timeout=…`. Verified against v1.56.0 (SQLite 3.53.3): `_txlock=immediate`,
`mode=ro`, `journal_mode(WAL)`, `busy_timeout(N)`, `foreign_keys(1)` and
`synchronous(NORMAL)` all work.

## The database

Two `*sql.DB` handles over one file: a **writer** capped at one connection, which removes
`SQLITE_BUSY` as a class of failure, and a **reader** pool sized to the machine.
Timestamps are stored as RFC3339 UTC text, so string ordering and time ordering agree.

Schema highlights — the full DDL is `internal/store/migrations/0001_init.sql`:

- `sources`, `source_acl` — the Calibre libraries and who can see them.
- `source_books`, `source_book_files` — one row per Calibre book per library. **Never
  deleted by ingest**; a book that disappears is flagged `missing`.
- `books` — the canonical merged book. `id` is issued once and never reissued;
  `merged_into` turns a superseded row into a permanently resolvable alias.
- `book_identities` — many identity keys pointing at one canonical book.
- `devices`, `device_tombstones` — per-device state; a tombstone records an on-device
  deletion and is permanent.
- `sync_points`, `sync_point_books`, `sync_point_tags` — immutable snapshots.
- `reading_states`, `tags`, `tag_books` — progress and collections, per user.
- `kepub_cache`, `kepub_failures`, `cover_cache` — derived artefacts, all rebuildable.

**Invariant:** ingest never issues `DELETE FROM books` or `DELETE FROM source_books`.
Only an explicit administrative purge removes a source's rows, and even that leaves the
canonical books intact.

## Reading Calibre

`calibre.Open` takes a private snapshot of `metadata.db` — together with its `-wal` and
`-shm` sidecars — into a temporary directory, verifies the signature did not change
mid-copy, runs `PRAGMA quick_check`, and works on the copy.

Copying rather than opening in place is deliberate. Calibre keeps `metadata.db` in WAL
mode and may be running; opening it read-only in place requires SQLite to map or create
the `-shm` file, which fails on read-only mounts and misbehaves over SMB and NFS. Working
on a copy also means a scan sees one consistent state even if the user edits the library
halfway through. **The user's real database is never opened writable.**

A scan is two phases. Phase A reads `id, uuid, last_modified` for every book, which is
enough to detect what is new, what changed and what vanished. Phase B reads the full
record for that set only, in batches, joining the linked tables in Go rather than with
`group_concat` — ordering inside an aggregate is only guaranteed from SQLite 3.44 and
the bundled version varies.

Failure is split into `ErrUnreachable` and `ErrCorrupt`. An unreachable library changes
**nothing** in the database: an unmounted share must never be mistaken for a library that
lost every book.

## Identity and merging

Each source row yields identity keys, strongest first:

1. `calibre_uuid` — clones and backups of one library, the dominant real-world case.
2. `isbn` — ISBN-10 converted to 13, checksum validated. Rejecting bad checksums matters:
   an invalid ISBN shared by two unrelated books would merge them.
3. `titleauthor` — normalised title and author sort form. Always present, so every book
   has at least one key; also the only one that can produce a false merge.

Normalisation folds to NFKD, drops combining marks, lowercases, expands `&`, strips
punctuation, removes a leading article and a trailing parenthetical.

`Attach` resolves those keys. No match creates a canonical book; one match attaches to it;
several means this row bridges books previously thought distinct, so they merge. The
survivor is the oldest by `created_at`, ties broken by the smallest id — deterministic,
independent of scan order, and the id devices are most likely to already hold. Losing rows
keep `merged_into` set so an id a device has held since before the merge still resolves.

`Resolve` recomputes the merged record. The winner is taken **whole** rather than
field-by-field, to avoid Frankenstein metadata; only fields the winner leaves empty fall
back to the next-ranked source. Ordering puts a source that actually has a readable EPUB
ahead of one that does not, then priority, then ids.

Two behaviours worth stating plainly:

- **`metadata_rev` moves only when `serving_hash` changes.** The hash covers exactly what
  a device can observe. Bumping the revision on every scan would push the whole library to
  every device, every time.
- **When a book becomes unavailable its serving metadata is frozen, not recomputed.**
  Clearing the title, cover and download format would change the hash, bump the revision,
  and announce a change for a book that merely stopped being on disk. Availability is a
  server-side fact and is deliberately absent from the hash, so a book vanishing and
  coming back is not two metadata changes.

Any change to which source rows are live — enabling, disabling, priority, removal — has
to be followed by re-resolving the affected books. A scan will not do it: nothing changed
in Calibre, so the books never enter the changed set. `Scanner.SetSourceEnabled` binds the
two together so they cannot be separated.

A **vanish guard** refuses a scan that would flag more than 20% of a source's books, or 25
of them, as missing. That shape is far more often a half-mounted share than a real
deletion. The transaction is rolled back and an operator confirms in the UI.

## The sync engine

This is the heart of the project.

### The desired set

For a device belonging to a user, at snapshot time:

```
Snapshot = ( { syncable, visible books } ∪ Books(parent snapshot) )
           \ Tombstones(device)
```

The union with the parent is the whole mechanism behind the headline property. A book that
has vanished from every source is still in the parent, so it is still here, so the diff
produces **nothing at all** for it — no removal, no re-add, no error. The device keeps the
file it is happily holding.

Hidden books are excluded from the carry-forward, and that is the one deliberate
exception. The union exists to absorb *accidental* disappearance — an unmounted share, a
deleted file, a disabled source. Hiding a book is an operator saying "take this off the
device", so it falls out of the snapshot and is retracted.

Tombstones are subtracted **after** the union, so a book the user deleted on the device
can never come back, not even on a full resync.

### Snapshots and the diff

A sync materialises the set into `sync_points` + `sync_point_books` with one
`INSERT … SELECT`, and the snapshot is never modified afterwards. That immutability is
what lets an interrupted sync resume exactly where it stopped, even while a scan is
rewriting `books` underneath it.

Materialising is not free, though, and a device checks in every few minutes with nothing
to be told. So a snapshot also records a **fingerprint** of everything it was built from —
counts and revision sums of visible books, of the user's reading states and collections, of
the device's tombstones, of enabled sources. A sync whose fingerprint still matches its own
last completed snapshot answers `[]` and writes nothing.

The fingerprint is computed from the data rather than kept as a counter on the side. A
counter must be bumped wherever anything is written, and a forgotten bump means a device
silently stops receiving updates — the worst failure here. An aggregate cannot be
forgotten. `TestEveryChangeIsNoticed` walks each kind of change and fails if one of them
does not arrive.

The diff is seven keyset-paginated queries over two snapshots, drained in a fixed order:
new books, changed books, removed books, changed reading states, new tags, changed tags,
deleted tags. Categories must not interleave — the device wants each one exhausted before
the next begins.

Emission respects the protocol's quirks:

| Category | What goes on the wire |
|---|---|
| New | `NewEntitlement` |
| Changed | `NewEntitlement` + `ChangedProductMetadata` + `ChangedReadingState` |
| Removed | `ChangedEntitlement` with `IsRemoved: true` — there is no `DeletedEntitlement` |
| Reading state | `ChangedReadingState` |
| Tags | `NewTag` / `ChangedTag` / `DeletedTag` |

A changed book is **not** sent as a `ChangedEntitlement` carrying a nested reading state:
the device ignores it.

### Continuation and the token

The response budget counts books, not JSON objects — a changed book costs three. When the
budget runs out the cursor is saved and `x-kobo-sync: continue` tells the device to come
straight back.

The token carries only references: `{ongoing, last, raw}`, prefixed `KOBIBRI.`. Everything
that matters lives in the sync point rows, so a token cannot go stale in a way that loses
books. A token without our prefix belongs to the real Kobo store and is kept verbatim, so
proxied syncs keep working.

The parent snapshot is deleted **only** when its child completes. Until then it is the
fallback for a device that reconnects with a stale token, so an interrupted sync loses
nothing.

## The Kobo HTTP layer

Authorisation is an opaque secret in the URL path, one per device. A Kobo's DeviceId and
UserKey are effectively irrevocable credentials and are unsuitable as access control, so
they are ignored; `/v1/auth/*` returns random tokens that are never checked again. Only
the hash of our secret is stored, and it is redacted from logs.

`/v1/initialization` is the most dangerous response the server produces: the device writes
every key of `Resources` into its config file permanently. The full set of URL keys is
overridden, with Kobo's exact placeholder casing. When proxying is on, the base map is
fetched from the store using the credentials the device sends on that very request, and
cached; a response with too few keys is discarded rather than cached. Without proxying,
only our own keys are sent, and the device keeps Kobo's endpoints for everything else.

Unknown endpoints are proxied — GET as a 307 redirect, anything else really proxied,
because the device downgrades non-GET to GET when it follows a redirect. Any transport
failure answers `200 {}` rather than an error.

Two structural notes:

- `r.PathValue` is **empty in middleware**: it is only populated once ServeMux has matched
  a route. The token is parsed from `r.URL.Path` instead.
- `PUT /v1/library/tags/{id}` (renaming a collection) and `PUT /v1/library/{uuid}/state`
  (reading progress) are the same path shape, so no routing table can separate them and
  ServeMux refuses the ambiguity. One handler takes both and dispatches on the segments.

All absolute URLs handed to a device are built by `httpx.URLBuilder` — one place that can
get it wrong. It also repairs the portless and bare-IPv6 `Host` headers Kobo firmware
sends.

## Conversion

Two stages, deliberately kept apart:

**Any format → EPUB** is Calibre's `ebook-convert`, called as a subprocess. Fifteen years
of per-format workarounds live in it, and the library kobibri reads is a Calibre library,
so the tool is usually already there. FB2, AZW3, MOBI and the rest go through it on the way
to KEPUB; PDF, CBZ and DJVU do not, because Kobo will not sync them at all.

One rule governs it: **a book is only offered to a device when it can actually be
delivered.** If no converter is present, books in other formats are simply not advertised
— offering one and then failing its download is worse than never offering it. That fact is
checked when the converter is constructed rather than taken from configuration, because a
misconfigured path would otherwise look available and break every such download.

**EPUB → KEPUB** uses kepubify as a library. The `Converter` interface has two
implementations — in-process and a subprocess via `KOBIBRI_KEPUBIFY_BIN` — so the day it
has to be replaced, the blast radius is one file. kepubify has had no release since 2022;
[PROGRESS.md](PROGRESS.md) records what replacing it would take and why a differential test
on span ids has to come first.

Conversion is **lazy on download and prewarmed in the background**, never inside a scan.
Converting a library synchronously is what makes other implementations' first sync take an
age, and it would hold the single SQLite writer connection for the duration. The cache is
keyed on `book id + fingerprint(path, size, mtime)`, converts once under `singleflight`,
and writes to a temporary name before renaming so a crash cannot leave a truncated file
that later looks cached.

Exactly one format is advertised to the device. A pre-paginated book is offered as
`EPUB3FL` and never converted — it already has one page per chapter, and conversion breaks
full-screen rendering. Everything else reflowable is `KEPUB`. Offering both KEPUB and EPUB
would let the device pick EPUB and silently lose span-level reading progress.

A book the library already holds as a KEPUB is served untouched: `convert_from` is set to
`KEPUB`, which every path reads as "there is nothing left to do to this". Converting an
already-converted book would nest koboSpan ids inside each other and throw the reading
position away.

The formats are tried in this order: a pre-paginated EPUB, then a KEPUB the library holds,
then a reflowable EPUB, then whatever Calibre's converter can turn into one. So when both
an EPUB and a KEPUB are present, **the KEPUB wins**. A library holding both has already
run this conversion, and redoing it spends the converter, the prewarm queue and the cache
directory to arrive at a file that is already on disk. This reverses the earlier rule,
which preferred the EPUB so the served file would be one the server could rebuild on
demand; that turned out not to be worth converting a whole library for.

Nothing is lost by serving the library's file. Conversion only wraps text in `koboSpan`
elements — it never touches metadata, and what the device displays comes from
`metadata.db` through the sync API rather than from inside the file. So the two KEPUBs
differ in nothing that reaches a reader.

A pre-paginated EPUB still outranks a KEPUB beside it, because conversion is precisely
what breaks full-screen rendering.

Because a scan only re-resolves books Calibre reports as changed, a rule change like this
one does not reach a library that was already scanned. `ingest.ReresolveLibraryKepubs` is
the one-off sweep that does, keyed in `kv` so it runs once.

The `.kepub.epub` suffix is load-bearing and has to survive from the cache path through to
the `Content-Disposition` filename: Kobo picks its renderer by filename — including when
the file was never converted here.

## Collections

Kobo calls them collections; a person calls them shelves. Devices create their own, and
the library's tags and series can be mirrored onto them as well — off by default, because
a library with two hundred tags would otherwise put two hundred shelves on someone's
reader unasked.

Collections are per user, since visibility is. The rebuild runs after every scan and is
idempotent: it compares membership before writing, so a shelf whose contents did not change
keeps its revision and is not re-announced to any device. A shelf a reader deleted stays
deleted — deletions made here are marked with a different origin, so a tag that leaves
Calibre and comes back is rebuilt while one a reader threw away is not.

### Series, and editing one here

A series reaches a device twice over. The metadata of every book carries a `Series` object
— name, number, and an `Id` that is uuid3 over `NAMESPACE_DNS` of the name, matching every
other implementation so a device that has synced elsewhere does not see the series twice.
That part is unconditional. On top of it, series can also become collections, which is the
`collections:mode` setting above and is off by default.

The series itself can be set here rather than in Calibre. That needs care, because a
series is a **derived** field: `Resolve` takes the winning source row whole and rewrites
`books` from it, so an edit written into `books` would last exactly until the next scan
touched that book. The edit therefore lives in `book_series_overrides` and is laid over
the top by `apply`, after the winner and every empty-field fallback have had their say —
the same shape as `books.hidden`, a decision made here that survives a scan because a scan
never writes it.

The row's **presence** is the override, not its contents. An empty `series_name` means
"this book is in no series", which is a real thing to want and cannot be said by leaving
the row out — that means "whatever the library says". Removing the row hands the book back
to Calibre.

Nothing extra is needed to tell a device: `servingFields` already covers the series name,
number and uuid, so an edit changes `serving_hash`, `metadata_rev` moves, and the next
sync sends `ChangedProductMetadata` on its own. Shelves are rebuilt from the resolved
series, so the handler follows the edit with a rebuild.

`/series` lists every series with a book the asking person may see, by the same sharing
rule the sync snapshot uses; `/series/{uuid}` is one series in reading order, with a book
that has no number sorted last rather than first — an unnumbered volume is nearly always a
companion, and putting it before book one is worse than putting it after the last.

### The library's own columns

A `#shelf` or `#status` column in Calibre becomes collections on the reader, chosen per
source. They are deliberately **not** mapped onto the device's `Genre` field, which holds a
category uuid from Kobo's own taxonomy rather than free text.

Values live in `source_book_columns`, apart from `tags_json`: a tag is the library's word
for a book, a custom column is its owner's private taxonomy. Choosing a column forces one
full re-read of that library, because a scan otherwise reads only what changed.

### When a merge is wrong

`titleauthor` is the only key every book has, and the only one that can be wrong: two
different books really do share a title and an author. The duplicates report lists the
merges that rest on that key alone — not every book with several copies, since a merge
backed by a uuid or an ISBN is evidence rather than a guess.

Splitting one apart keeps the original id, because that is what readers hold, and gives the
copy that leaves a new book. `source_books.pinned_book_id` is what makes it stick: the keys
that joined them still match, so without a pin the next scan would merge them back.

## The catalogue

`/opds` is an OPDS 1.2 feed, for reading apps that are not a Kobo. It authenticates with
HTTP Basic rather than a session, because a reading app has neither a browser nor a way to
fill in a login form, and it enforces the same per-user source visibility the sync snapshot
does. Only books that can actually be delivered are listed.

## Books put here by hand

Uploaded files land in a single source of their own, created on the first upload, with
priority 0 — above every Calibre library. When the same book is in both, the copy someone
chose to put here is the one that reaches a reader.

From there nothing is special about them: a `source_books` row, identity keys, `Attach`,
`Resolve`, the same merge and the same conversion. Metadata comes out of the EPUB, so a
file exported from Calibre carries that library's uuid and merges with the library's copy
rather than arriving as a second book. Removing one deletes the file and marks the row
missing, exactly as a vanished Calibre book — the canonical id never goes anywhere.

## Reading in the browser

`internal/reader` is not a reading app. It opens the file that syncs and shows it a chapter
at a time, so the question "did this conversion actually come out right" has an answer
without a Kobo in hand.

The book is untrusted: it is framed with `sandbox=""`, served with a restrictive CSP, and
every path is checked against the zip's real entries. Files keep the paths they have inside
the zip so the book's own relative links work unrewritten.

## Importing from a link

A book published as a web serial enters through `internal/webimport` and then follows
exactly the same path as anything else: a `source_books` row, an identity key, a canonical
book. Merging, conversion and sync never learn where it came from.

Downloading, chapter caching and EPUB assembly are `github.com/fess932/novelkit`'s job. It
already carries the provider abstraction this needed — `novel.Source` plus a registry — so
kobibri registers the sites it wants and otherwise stays out of the way. Adding a site is
a change to novelkit, not here.

Three details are ours:

- **Identity is the link.** Such a book has neither a Calibre uuid nor an ISBN, so
  `weburl:<link>` is its strongest key. Re-importing the same link lands on the same
  canonical book even if the title changed on the site.
- **Its source is never scanned.** A web source has no `metadata.db`; a scan that tried to
  read one would mark every imported book as vanished. The scanner returns early on
  `SourceKindWeb`.
- **Re-importing is the update path.** novelkit derives its cache directory from the book,
  so planning again keeps what was downloaded and adds newly published chapters. A longer
  book is a different file, which moves `serving_hash`, which moves `metadata_rev` — and
  the reader picks the new version up on its next sync without anything special.

  The book **updates in place rather than appearing twice**: its canonical id never
  changes, and the device keys entitlements on that id.

  Reading position survives too, and that is not luck. A Kobo stores a position as a
  koboSpan id inside a named content document, so it only survives if the earlier chapters
  keep both their filenames and their contents. Measured: adding chapters leaves every
  earlier chapter byte-identical and every span id in them unchanged. The only thing that
  moves is the table of contents, which has to. Two tests hold that down, because it is a
  property of novelkit's assembly and kepubify's numbering rather than of anything here —
  if either ever changes, every saved position in every imported serial moves with it.

**Choosing a translation is a first-class step.** A title usually carries several, and they
are different texts — different wording, often different chapter numbering. So the browser
flow is two steps: paste a link, see what translations exist, pick one. Only then is
anything downloaded. The translation is part of the identity key, so two translations of a
title are two books rather than one silently replacing the other.

Downloads run in the background and report progress: a serial can run to hundreds of
chapters, each fetched politely, which is far too slow to hold a request open. One download
per book at a time — a periodic check and someone pressing the button would otherwise write
the same cache directory and the same assembled file at once.

Imported serials are checked for new chapters on a timer, one book at a time. These are
other people's sites, and a serial that is a few hours out of date is not worth hammering
them for. `KOBIBRI_IMPORT_CHECK_EVERY` sets the interval, or switches it off.

The web source is created on first use with a deliberately high priority number, so a real
Calibre copy of the same book wins over a scraped one.

## Covers

Scaled into three buckets by requested height, always JPEG, never upscaled, cached on
disk. Serving full-resolution images visibly stalls the device's library browsing.

`CoverImageId` embeds the cover's modification time, because the device caches covers by
image id indefinitely — a replaced cover has to arrive under a new id or it is never
refetched. The handler strips the suffix, so old ids keep resolving.

A book with no cover gets a neutral placeholder with `200`, not a 404: the device retries
failing cover URLs relentlessly.

## The web interface

Server-rendered `html/template` with embedded assets, cookie sessions, bcrypt, and a CSRF
token on every mutating form. No build step and no external files, so the whole thing
stays one binary.

The palette takes after the device it serves — an e-ink screen: near-monochrome, high
contrast, one restrained accent, neutrals biased toward it. It is a tool to be scanned and
operated rather than read, so state is encoded in form as well as words, and warnings are
surfaced above the numbers. On a book, contributing libraries are listed in the order the
winner is picked, which explains why the merged record looks the way it does.

Books download as the original and as the converted KEPUB; only the KEPUB is what a Kobo
receives.

Navigation is two levels. The spine on the left is one entry per section; the overview,
the books and the series are three views of one section, so they share an entry there and
separate along the top of the page. `libraryNav` in the template func map decides which
pages belong together, and both levels ask it, so they cannot disagree. The strip along
the top is underlined rather than pill-shaped on purpose — it divides a section the spine
has already highlighted, and a second row of pills would compete with it instead.

The interface is available in English and Russian. English is the default, the browser's
preference is honoured, and an explicit choice outranks it. The server itself — logs,
errors, the sync API — is English throughout. A phrase missing from the catalogue falls
back to English rather than showing its key.

## Testing

- **`calibretest`** builds real Calibre libraries on disk: the authentic DDL subset, a
  directory tree, and valid tiny EPUBs — reflowable, pre-paginated, EPUB2 and a broken
  zip. Ingest, sync and web tests all run against it.
- **A fake device** replays the sync conversation and keeps the library a real Kobo would
  end up holding, so tests assert on the outcome rather than on individual responses.
- **Landmines have tests.** Every quirk in [kobo-protocol.md](kobo-protocol.md) marked
  LANDMINE has a test that fails if it is reintroduced.
- **Rendering is tested**, because a template that calls a missing function compiles fine
  and only fails when parsed at startup.
- **A soak test** runs two sources and two devices through edits, a deletion in Calibre, a
  source switched off and back on, a self-deletion on one device, and syncs interrupted at
  several cursor positions with the device restarting each time — losing its token but not
  its files. It asserts the invariant everything else exists to provide: a device never
  loses a book it was given, and is only ever told to archive one it deleted itself.
- **The race detector is not in CI.** It roughly triples the job, and the test workflow
  gates every push. `make race` runs it, and it is worth running before a release and
  after anything touching the scanner, the scheduler or the sync engine — a scan rewrites
  the same rows a paginated sync is reading from, which is what it exists to catch.

## Deployment

`CGO_ENABLED=0` and the cgo-free SQLite driver produce one statically linked binary, so a
NAS or a Raspberry Pi needs nothing but the file. `deploy/` has a hardened systemd unit, a
commented environment file and an nginx fragment; the Dockerfile is multi-stage and runs
unprivileged.

Three deployment facts come from the device rather than from preference: Kobo firmware
cannot negotiate a TLS-1.3-only server, so TLS 1.2 stays enabled and kobibri turns HTTP/2
off when it serves TLS itself; sync responses overflow a reverse proxy's default buffers
and the device sees a 502 partway through; and some firmware fails to resolve hostnames
where a raw IP works.

An hourly janitor trims the rebuildable caches to their budgets and removes abandoned
snapshots and expired sessions. A device's single completed snapshot is never touched — it
is the baseline its next sync diffs against.
