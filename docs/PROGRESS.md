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
server makes is the one it can prewarm, cache and rebuild on demand. **Reversed later —
see below.**

### The library's own KEPUB wins after all

Reported from a real library: books that already had a KEPUB beside their EPUB were
imported and immediately started converting. The rule above was doing exactly what it
said, and it was the wrong rule. Being able to rebuild the served file on demand is worth
something, but not a whole library's worth of conversion to produce a file that is already
sitting on disk — and kepubify is the same tool at both ends, so the result is not even
better.

There is no cost to weigh against it either. Conversion only wraps text in `koboSpan`
elements; it touches no metadata, and the metadata a device shows comes from `metadata.db`
over the sync API, not from inside the file. The library's KEPUB and one we would make
differ in nothing a reader can observe.

`applyDownload` now tries, in order: a pre-paginated EPUB, a KEPUB the library holds, a
reflowable EPUB, then anything Calibre's converter can reach. The fixed-layout case stays
first: conversion is what breaks full-screen rendering, so a pre-paginated EPUB still
outranks a KEPUB next to it.

The trap was that fixing it changes nothing on its own. A scan re-resolves only what
Calibre reports as changed, so an already-scanned library would have kept converting
forever. `ingest.ReresolveLibraryKepubs` sweeps every book whose source holds a KEPUB,
once, gated on a `kv` version the way `BackfillCovers` is.

### Phrases that name something

`T` only translated fixed phrases, so anything that had to mention a library or a count was
assembled in Go and shipped in English — including the dashboard warnings, where a Russian
prefix was concatenated with an English sentence and read as nonsense.

`Msg(key, arg)` now packs the value into the string after a separator and `T` unpacks it,
which is what lets a flash message survive the round trip through a redirect and still be
translated on the far side. Only one argument is supported, deliberately: every phrase here
names exactly one thing. `TestEveryPageRenders` fails if a separator ever reaches the page,
which is the tell-tale of a phrase that is not in the catalogue.

### M13 — reading a book in the browser

`internal/reader` opens an EPUB or KEPUB far enough to leaf through it: container.xml to
the OPF, the manifest and spine for reading order, the EPUB 3 navigation document or the
EPUB 2 NCX for chapter names. Contents are read on demand, so opening a large book is
cheap.

It reads **the KEPUB** — the file that actually syncs — because that is the question it
exists to answer: did the conversion produce readable chapters, in the right order, with
their images and stylesheets. There was no way to find that out without a Kobo in hand.

Two things it is careful about, both because the path arrives from a URL and the book is
untrusted content:

- Files are served under the paths they have **inside the zip**, so relative links between
  a book's own files resolve without being rewritten. A path that does not name a real
  entry is refused; a path that walks upward never gets that far.
- The frame is `sandbox=""` — no scripts, no forms, an opaque origin — and assets go out
  with a restrictive CSP and `nosniff`. EPUBs may carry JavaScript and remote references.

Chapters are served as `text/html` rather than `application/xhtml+xml` on purpose: browsers
refuse to render XHTML that is even slightly malformed, and a page that will not open tells
us nothing about whether the conversion worked.

### M14 — books put here by hand

There is now a place to upload files directly, without going through Calibre.
`internal/upload` files them exactly as a scan would: a `source_books` row, identity keys,
`Attach`, `Resolve`. Nothing downstream learns where a book came from.

Everything uploaded belongs to **one source**, created on the first upload, with priority
0. That is the point of it: when the same book is in a Calibre library too, the copy
someone chose to put here by hand is the one that reaches a reader.

Three things had to change for that to be true:

- **Priority 0 could not be passed to `CreateSource`**, which reads 0 as "not given" and
  substitutes 100. It is set with an explicit `UPDATE` instead. The form for a Calibre
  library will not go below 1, so nothing can tie with it.
- **Candidate ranking said "has an EPUB" and now says "has a file".** The old rule meant a
  Calibre EPUB beat an uploaded KEPUB whatever the priority. Which file is actually served
  is decided later, across every candidate, so an upload can supply the record while a
  Calibre EPUB still supplies the download.
- **The author had to be a sort form.** Identity compares `author_sort`, Calibre stores
  "Lastname, Firstname", and a display name folds to the same words in the other order —
  so an uploaded "Jane Author" would never match the library's "Author, Jane" and the book
  would arrive twice. `upload` builds the sort form; the comment on `NormalizeAuthor` used
  to claim the two forms were equivalent, which they are not.

Metadata is read straight from the EPUB, which means a file exported from Calibre carries
that library's own uuid and merges with the library's copy even under a different filename
and title. Anything else has only its filename: "Title - Author.fb2" is the shape almost
every downloaded book arrives in.

Removing an upload deletes the file and marks the source row missing, exactly as a
vanished Calibre book. The canonical book stays — its id is what every reader holds.

The CSRF check had to change too. It read the token with `FormValue`, which parses the
body; for a multipart upload that consumed the very stream the handler was about to read,
and the file arrived empty. An upload form carries its token in the query instead.

**Known asymmetry, left alone deliberately:** `webimport` still writes a display name into
`author_sort`, so a web-imported serial will not merge with a Calibre copy by title and
author. Fixing it would change the identity key of every book already imported, and a book
whose key changes is re-attached to a *new* canonical id — which is the one failure this
whole design exists to prevent. It needs a migration that rewrites the keys in place, not
a one-line change.

### Writing down what goes to the store

Every request the proxy handles is now logged: the endpoint, the query, the upstream status,
the byte count and how long it took — and a warning, not a debug line, when the store cannot
be reached. A GET is a redirect rather than a fetch, so it is logged as one; there is no
status to report when the device talks to the store itself.

Three things are stripped before anything is written:

- **The token.** It lives in a device's config file forever and would otherwise sit in a log
  file and every pasted bug report.
- **Book ids**, collapsed to `{id}`, so a device asking about a thousand books is one
  endpoint rather than a thousand lines.
- **Anything in the query that looks like a credential.** The store's API is undocumented,
  so the rule is by shape — a key containing token, key, secret, password, signature, auth
  or code keeps its name and loses its value. The parameter is still visible, which is the
  part worth knowing.

This is what turns "can Kobo sync ratings?" from speculation into a task: rate a book on a
real device and read the line.

### M15 — undoing a merge that should not have happened

The open risk about false merges on `titleauthor` now has a way out.

`SuspectMerges` lists books whose copies were joined **on title and author alone** —
deliberately not every book with several copies. A merge backed by a Calibre uuid or a
shared ISBN is evidence; this one is a guess, and it is the only kind that can be wrong.
Most entries in the report are still correct, and it says so: burying the real cases in
noise is how a report gets ignored.

`Split` moves one copy onto a book of its own. The book that stays **keeps its id**,
because that is what every reader holds; the copy that leaves becomes a new book and
arrives on a device as one. There is no way around that, and it is the point — they were
two books all along.

The part that needed schema is that a split cannot survive on its own: the keys that
joined the two still match, so the very next scan would merge them straight back. Migration
0004 adds `source_books.pinned_book_id`, and `Attach` returns it without looking anything
up — and without claiming identity keys either, which could otherwise take them from the
book it was split from. `Rejoin` clears the pin and lets the keys decide again.

Splitting the last copy is refused: it would leave an empty book behind.

### M16 — a catalogue for everything that is not a Kobo

`/opds` serves an OPDS 1.2 catalogue: a navigation feed, all books by title, recently added,
and search with an OpenSearch document so a reader can find it. Version 1.2 rather than 2.0
because every reading app supports it and the newer one still does not.

Two things it does not share with the browser interface:

- **HTTP Basic, not a session.** A reading app has no browser and no way to fill in a login
  form. Comparing a bcrypt hash is slow enough to notice and a reader fetches a feed, a
  cover and a book in quick succession, so a successful pair is remembered for a minute.
- **Visibility is enforced in the query.** `LibraryQuery.UserID` applies the same
  visible-source rule the sync snapshot uses, so two people sharing a server see the same
  books in the catalogue that they would see on a device.

Only books that can actually be handed over are listed, and a test downloads every
acquisition link in the feed. An entry whose download fails is what makes a reading app
show an error instead of a library.

EPUB is the first acquisition link, KEPUB the second: several readers take the first link
they understand, and outside a Kobo the plain file is the right one.

### M17 — a library's own columns

The backlog said "map Calibre custom columns onto `Genre`". That turned out to be the wrong
target, and the protocol notes already said so: `Genre` holds a **category uuid from Kobo's
own taxonomy**, not free text. Putting a library's own words there would be ignored at best.

What such a column is actually for is shelves. A person who keeps a `#shelf`, `#status` or
`#mood` column in Calibre has already organised their library; this makes that organisation
appear on the reader. Columns are chosen per source, since they belong to a library, and
each chosen column's values become collections through the machinery from M12.

Three details that were not obvious:

- **Calibre stores a column in one of two shapes**, and says which by the `normalized`
  flag: a normalized column keeps its values in a table of their own with a link table,
  exactly as tags do; a plain one keeps the value on the row. Guessing between them by
  datatype is how other readers of this schema get it wrong.
- **Only some datatypes can name a shelf.** Text, enumeration and series can. Numbers,
  dates and yes/no cannot — a shelf called "true" or "2019-04-01" helps nobody — so those
  columns are not offered.
- **Choosing a column has to force one full re-read.** A scan reads only what Calibre says
  changed, so a column chosen afterwards would leave every book already here without a
  value for it. The chosen set is remembered and compared, so this self-heals whatever path
  the choice was made through.

Values are stored in `source_book_columns` (migration 0005) rather than folded into
`tags_json`: a tag is the library's word for a book, a custom column is its owner's private
taxonomy, and conflating them would make a shelf of every tag and lose which is which.

### Covers for books that are not from Calibre

Every book imported from a link or uploaded by hand had **no cover at all** — a blank
rectangle on the reader and a placeholder in the browser. `CoverRelPath` was only ever set
by the Calibre scanner; nothing else filled it in, and nothing noticed because no fixture
had a cover to lose.

A book from either of those routes carries its cover inside the file and nowhere else, so
`reader.Cover` pulls it out and `store.ExtractCover` writes it beside the book as
`cover.<ext>` — the same shape a Calibre library has, which means nothing downstream had to
learn where a book came from.

Finding it takes three tries, because a book names its cover in one of three ways: EPUB 3
marks it in the manifest with `properties="cover-image"`; EPUB 2 has no such thing and
points at it from a metadata entry, which is what Calibre and most converters write; and
failing both, an image merely named like one. A book with no cover is not an error.

Books filed before any of this have none, and nothing else would ever give them one: a scan
does not touch those sources, and re-importing would download the whole book again.
`BackfillCovers` opens each such book once at startup and takes the cover out. It is a
one-off, remembered by a version in `kv` — a book that genuinely has no cover would
otherwise be opened on every start for the rest of its life.

A second thing was wrong in the browser reader, and it took the same shape: the chapters
were served with `default-src 'self'`, and the frame is sandboxed **without**
`allow-same-origin`, so its origin is opaque and `'self'` matches nothing. Every picture and
every stylesheet in the book was refused. The policy now names this server's own address
outright, and a test asserts both that a picture is served and that the policy is not
written in terms of `'self'`.

### The download filename

A download of a Russian book saved itself as
`=_utf-8_q_=D0=A0=D1=83=D0=BF...`. The name was being encoded with
`mime.QEncoding`, which produces the `=?utf-8?q?…?=` form — that belongs to **mail
headers**. `Content-Disposition` wants RFC 5987: percent-encoded UTF-8. Nothing rejects the
wrong one; the browser simply shows it, character for character, as the name of the file.

Both copies of the header builder — one in `kobo`, one in `web` — had it, so it now lives
once in `httpx` and a test parses the header back with `mime.ParseMediaType` and compares
it to the original name.

The ASCII fallback in `filename` was equally useless: every non-ASCII character became an
underscore, so a Cyrillic title arrived as `________.epub` in anything that ignores
`filename*`. It transliterates now — `Долгие сумерки Земли` becomes `Dolgie sumerki Zemli`.

### Covers: a placeholder that outstayed its welcome

The library grid showed grey rectangles for books whose covers the book page displayed
perfectly well. Measured rather than argued: a test fetches the same book's cover at both
sizes and neither is a placeholder, so the server was right and the browser was holding an
old answer.

That was ours to prevent, though. The cover URL does not change when a cover appears, and a
missing cover was served with `max-age=300` — so a book photographed as blank stayed blank.
A placeholder is now `no-store`, and the pages ask for `/cover?v=<CoverImageId>`, which
changes the moment there is something new to fetch.

### Newest first, and a token for hidden titles

Three small things, all of them fixing something that was quietly wrong.

**The library is ordered newest first.** Alphabetical order buries what someone came to
look at, which is nearly always what just arrived. The other order is still a click away.
The dashboard's "Recently added" panel was sorting by title too, so it had been showing
the wrong twelve books since it was written.

**novelkit v0.5.0.** It brought exactly what was needed here: `novel.ErrNotFound` and
`ranobelib.WithToken`.

**An access token for titles the site hides.** Some titles answer 404 to anyone not signed
in — the same answer as a book that never existed — so a perfectly good link looked broken
and there was nothing to say about it. The token is entered on the imports page, kept in
the database because the daily chapter check runs long after anyone typed it, and never
shown back. Setting it rebuilds the providers wholesale rather than mutating a client, so a
download already running keeps the one it started with.

The error now says which case it is: without a token, that an account might be needed;
with one, that the book may simply not be there. That difference is what separates a person
giving up from a person pasting in a token.

### M21 — FB2 without Calibre

An uploaded FB2 did not convert, and the reason was two layers deep. Calibre was
installed but its converter lives inside the macOS application bundle and is
never on PATH, so a machine with Calibre working looked exactly like one without
it. That is fixed — the known install locations are searched.

The better answer was to stop needing Calibre for the format that actually turns
up. `internal/fb2` converts FB2 to EPUB directly: it is a single XML file with
its pictures inlined as base64, and everything an EPUB needs is already in there
— title, authors, series, annotation, cover, sections, poems, epigraphs,
footnotes.

Two things a hand-written fixture would never have caught, and a real book did at
once:

- **Encoding.** The first real file was windows-1251, and Go's XML decoder knows
  only UTF-8. It failed on the first Cyrillic character, which is to say on the
  title. `golang.org/x/text/encoding/htmlindex` decodes whatever the declaration
  names now.
- **Chapter granularity.** One file per *top-level* section. Splitting nested
  ones out as well turns a book with sub-sections into hundreds of fragments.

Verified end to end on a real book: our own reader opens the result, the cover
comes out of it, chapter titles reach the table of contents, and the KEPUB
conversion after it has something ordinary to work with.

Calibre is now genuinely optional — needed only for Kindle and other formats,
and the interface says so instead of promising them.

### Covers for books that are not EPUB, and a random-sequence test

An FB2 arrived with no cover, and the reason was the same shape as before: the
cover was being taken out of the **original** file, and an FB2 is XML rather than
a zip. A book held as FB2, AZW3 or MOBI keeps its cover in a form only its own
reader understands — the converted EPUB is the first moment one can be taken out
at all, so that is where it happens now, as the prewarmer finishes each
conversion.

It refuses to touch a Calibre library. Those keep a cover.jpg beside the book
already, and writing into someone's library is the one thing this server does not
do — there is a test for that, not just a comment.

Uploaded files were already converted the moment they land; the uploads page just
never said so. It shows whether each one is ready, still converting, or cannot be
converted here at all.

### M20 — a test that writes its own scenarios

The converter work made the case for this concrete: every hand-written fixture
passed while the thing was broken on fifty-three of fifty-five real books.

`property_test.go` throws random sequences of everything that can happen — scans,
edits, a library removed and added back, books hidden, syncs cut off partway, a
device deleting a book, an operator resetting a device's sync state — at two
devices at once, and checks after **every step** that a device never loses a book
it was given and never holds one book under two identities.

What makes it usable rather than a curiosity:

- **The seed comes from the round, not the clock**, so a failing run is
  reproducible from its own output and replayable with `-seed`.
- **Every operation is logged**, so a failure reads as a story rather than a state
  dump.
- **It was proven to have teeth rather than assumed.** Breaking carry-forward in
  the snapshot query makes it fail in six steps with a readable trace. A companion
  test fails if the seeds ever stop reaching all eleven kinds of operation — a
  random test that quietly exercises three of them is a slower version of a
  smaller one.

### M19 — kepubify is gone

Ours is the converter now, and the dependency is out of `go.mod`. What made that safe was
running both over **fifty-seven books from a real library** rather than over more fixtures.

The first run matched **two of fifty-five**. That is what a hand-written fixture set is
worth: every one of mine passed while the thing was broken.

Three faults, each invisible to fixtures:

1. **XHTML was being parsed as HTML.** `<title/>` is self-closing in XHTML and opens an
   element in HTML, whose content is RCDATA — so an HTML parser swallows the rest of the
   chapter as the title's text. Every book from a converter that self-closes empty elements
   lost a whole chapter. Content documents are XML and are parsed as XML now, falling back
   to the HTML parser only for books that are not well-formed at all.
2. **The block counter was wrong**, and no amount of thinking would have got it right. It is
   not a property of the tree: it is a walking counter, incremented on entering `p`, `h1`–
   `h6`, `ul`, `ol` or `table` — but **deferred**, so an element holding no text never
   consumes a number. Text before any of them is `kobo.0.n`. Working this out by experiment
   got most of it; reading kepubify's own source got the rest, which is the right order —
   the experiments said what to look for.
3. **The non-breaking space.** kepubify's sentence splitter counts only ASCII whitespace,
   while its "is this only whitespace" check uses Unicode's — so a stray `&#160;` between
   paragraphs is not spanned. Matching one and missing the other put a span around every one
   of them and shifted every id after it. That was the last book of the fifty-five.

kepubify's output for every fixture is recorded under `testdata/golden`, so the gate still
runs with the dependency gone. Re-recording needs a real kepubify on PATH, deliberately:
the recording is the only remaining evidence that our ids match the ones already on people's
devices, and it must not come from us.

`KOBIBRI_KEPUBIFY_BIN` still runs an external kepubify, for anyone who wants to compare.

### M18 — a converter of our own

`internal/kepubconv/native.go` converts EPUB to KEPUB without kepubify: the wrappers, the
style hack, and a koboSpan over every run of text. It is selected with
`KOBIBRI_KEPUB_CONVERTER=kobibri`, and **kepubify is still the default** — it has converted
real books for years and ours has converted twenty-two fixtures.

The differential test is what makes the work possible at all, and it earned its keep
immediately. Ours split `«a quoted sentence.» Then another.` into two spans, which is the
better reading of the sentence — and wrong. kepubify does not split there, every book
converted so far has ids that follow its rule, and being cleverer would move every saved
reading position in every book already on a device. The splitter is deliberately naive now,
with a comment saying why, because that is the only thing it is allowed to be.

Two things it does that kepubify's output demanded:

- **Its own XHTML renderer.** `html.Render` writes HTML5, where `<img>` has nothing closing
  it. An EPUB content document is XHTML, and a book a strict reader refuses to open is
  worse than one without word-level progress. A test parses every converted chapter with a
  strict XML decoder.
- **`mimetype` first and uncompressed**, which the format requires and a checking reader
  enforces.

Changing the setting re-converts nothing by itself: the cache is keyed on the source file,
not on who converted it, so a book only changes when its file does.

The harness caught three faults of its own along the way — counting `<title>` as body text,
comparing whitespace that does not exist in the source, and treating the non-breaking space
as whitespace on one side of a comparison and not the other. That is what writing the
specification before the converter is for.

### Reading progress, shown

The device has been sending it all along — `PUT /v1/library/{uuid}/state` carries a
bookmark with `ContentSourceProgressPercent` — and it was stored and handed back to other
devices without anyone ever seeing it. The library now shows a bar across the foot of each
cover and a percentage beside the title, and the book page shows where each reader has got
to.

Two things it is careful about:

- **The whole-book figure, not the chapter one.** `ProgressPercent` is progress through the
  current chapter and `ContentSourceProgressPercent` is progress through the book. A reader
  44% through a book is not 12% through it, and devices do not always send both.
- **99.6% is not finished.** The percentage is floored and capped at 99 until the status
  says `Finished`, because telling someone they have finished a book they have not is worse
  than being a percent shy.

Progress is per person rather than per device — a position is shared across everything one
person reads on — so the listing takes a separate `ProgressFor`: an administrator sees every
book but only their own place in one. The library can also be filtered to what is being read
or has been finished.

### A specification for the KEPUB converter

The precondition for replacing kepubify is a differential test, and it now exists —
`spec_test.go` runs a converter over twelve fixtures aimed at what a converter can get
wrong, and pins the rules by measurement rather than by reading the source.

What it established:

- **Nothing a reader sees may change.** Every visible character survives, none is
  duplicated, and the spans between them cover all of it.
- **Ids are `kobo.<block>.<segment>`**, both counted from one, in reading order, unique
  within a chapter. This is the rule that matters most: ids are where a reading position is
  stored, so a converter that numbered them differently would move every saved position in
  every book.
- **`<pre>` is left unspanned** — deliberately. Every space in it is part of what is shown,
  and slicing it would change how it renders. The cost is that a position inside such a
  block falls back to the block.
- **A span the book already had is kept**, with the koboSpan nested inside it.
- **The wrappers** `div#book-columns` / `div#book-inner` and the `kobostylehacks` style are
  what Kobo's renderer expects.

The same helpers take any `Converter`, so the day a replacement exists it is one line to run
it against the same fixtures and compare span for span.

### Leftovers, found mechanically

After M24 the answer to "is anything left" was checked rather than recalled, in
three more ways:

- **Every route is reachable from the interface.** Nothing is registered that no
  page links to.
- **Four exported functions had outlived their callers** — `IsConvertible`,
  `SetConversionAvailable`, `ReadingProgress`, `SyncPointBookIDs` — each replaced
  by something that knows more, and none of them called by so much as a test.
  Removed. Dead code that looks like API is how the next person picks the wrong
  one.
- **Four settings the server reads were missing from the README**: the three
  cache budgets and the log level. Documented.

### M24 — what a reader was actually sent

Two tables were in the schema from the first milestone with nothing writing to
them. `source_acl` was one (M23). `sync_runs` was the other: read by nobody,
written by nobody, and quietly implying a feature that did not exist.

Both were found the same way — listing every table and counting which have a
writer and which have a reader. That is worth doing once in a while; a table with
readers and no writers is a feature someone believes in.

The device page now shows what each reader was told: how many books were new,
changed or archived, how much reading progress and how many collections, and
whether the sync finished. It is the only thing that answers "my book never
arrived", which `last_sync_at` cannot.

Syncs that sent nothing leave no trace. A reader checks in every few minutes, and
a history of empty check-ins is a history nobody can read. The janitor keeps the
last fifty per reader.

### M23 — the half of multi-user that was never built

`source_acl` was read by the sync snapshot and by the library listing from the
first milestone, and **nothing ever wrote to it**. Every library was created
shared with everyone, and there was no way to change that. So the multi-user
server — a family sharing one machine, which is the case it was asked for — had
accounts that all saw the same thing.

The Libraries page now says who can see each one. Restricting to nobody is
treated as sharing with everyone: it would otherwise hide a library from the
person who just pressed the button.

The interesting part was what it broke. The quiet-sync fingerprint from M22
counts books, reading states, collections, tombstones and enabled sources — and
granting someone access to a library changes **none** of those. A person given a
library would have waited for some unrelated change before receiving any of it.
Sharing is part of the fingerprint now, and `TestEveryChangeIsNoticed` has a case
for it that fails without the term — checked by removing it, not by reasoning
about it.

That is the second time that test has earned its place within a day of being
written, which is roughly the argument for writing it.

### M22 — measured on a library of the size it was designed for

Everything until now had been tested on a few dozen books, while the design was
written for tens of thousands. That claim had never been checked. `scale_test.go`
builds a library of any size and measures what an operator actually waits for.

At twenty thousand books, on a laptop:

| | |
|---|---|
| first scan | 9.9 s (0.5 ms per book) |
| rescan, nothing changed | 90 ms |
| first sync of a device | 2.3 s over 201 requests |
| **sync with nothing to say** | **268 ms → 10 ms** |
| database | 51 MB |

Everything scales linearly, which is what it should do. The last line is the one
that mattered: a device checks in every few minutes forever, and answering
"nothing" was costing a whole new snapshot — twenty thousand rows written and
twenty thousand deleted, every time, for every device. On a NAS with an SD card
in it that is not merely slow.

A sync point now records a **fingerprint** of everything a snapshot is built
from: counts and revision sums of visible books, of the user's reading states and
collections, of the device's tombstones, of enabled sources. A sync whose
fingerprint matches its own last completed snapshot answers an empty list and
writes nothing at all.

It is computed from the data rather than maintained as a counter, deliberately. A
counter has to be bumped from every place that writes, and the day one of those
is forgotten a device stops receiving updates and nobody notices — the worst
failure this system has. An aggregate cannot be forgotten.

And because being cheap is worthless if it is also wrong, `TestEveryChangeIsNoticed`
walks every kind of change — a book added, edited, hidden; a collection created;
progress reported on another device; a source switched off — and fails if any of
them fails to reach the device. Making the fingerprint constant makes five of its
six cases fail, which is how the test was shown to have teeth rather than assumed
to.

## Notes for novelkit

Found while integrating. On **v0.6.0** every one of them is fixed, and nothing here works
around the library any more.

1. ~~**`ParseSlug` does not handle `/ru/manga/<slug>`**~~ — fixed in v0.4.1. It now walks
   the path segments and takes the first that looks like a slug, which covers that shape
   and any future section name. Verified: the full `/ru/manga/…` link resolves and lists
   its translations.

2. ~~**`Registry.Resolve` reports two different failures the same way**~~ — fixed in
   v0.4.1 with `ErrBadReference` alongside `ErrUnsupported`. kobibri dropped its own
   workaround and maps the library's two errors instead.

3. On **v0.5.0** the library grew what this needed: `novel.ErrNotFound`, so a site saying
   "no such book" is now distinguishable from any other failure, and
   `ranobelib.WithToken`, which makes titles visible that the API hides from anyone not
   signed in. Both are used.

4. ~~**An empty edition and the explicit id of the same edition get different cache
   directories.**~~ — no longer true, and the note was stale rather than newly fixed:
   `job/job.go` is byte-identical from v0.4.1 through v0.6.0, and `Plan` already settled the
   translation before naming the directory. Measured rather than argued:
   `TestAnUnnamedTranslationReusesItsDownload` runs both imports and fails if a second job
   directory appears.

   Worth recording because it was checked for a different reason — a book that started
   downloading again after the upgrade. It was not the cache key: nothing that names a job
   directory changed between those versions.

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

- ~~**Dependence on an unmaintained kepubify.**~~ Gone: the conversion is ours, verified
  against kepubify on fifty-seven real books before the dependency was dropped, and its
  recorded output still gates every change. See M19.
- ~~**False merges on `titleauthor`.**~~ Still possible, but no longer a risk without a
  remedy: the duplicates report lists exactly the merges that rest on that key, and a wrong
  one can be split apart in a way a later scan will not undo. See M15.

## Backlog

### Formats, honestly

What this server converts **by itself**, with nothing installed:

| Format | Status |
|---|---|
| EPUB | native, nothing to convert |
| KEPUB | native, served as it is |
| **FB2** | **native** — `internal/fb2` |
| AZW3, MOBI, AZW | needs Calibre; compressed binary containers, a project each |
| LIT, PDB | needs Calibre; obsolete enough not to be worth writing |
| HTMLZ, RTF, DOCX, TXT | needs Calibre; would be easy, and nobody has asked |
| PDF, CBZ, CBR, DJVU | never — Kobo does not sync them at all |

The interface lists what **this machine** can do rather than the table above:
promising AZW3 with no Calibre installed is how someone uploads twelve files and
gets twelve rows that never convert.

Nothing further is planned here. The formats that need Calibre are covered by
Calibre when it is installed, and every one of them is rarer in practice than the
one that is now native.

### M15 — series as their own thing

Series were already reaching devices twice over — as the `Series` object on every book's
metadata, and, when `collections:mode` allows it, as shelves. Neither was visible in the
interface, so there was no way to see what series existed, no way to spot a book that had
fallen out of one, and no way to fix it short of opening Calibre.

`/series` lists them, `/series/{uuid}` is one series in reading order, and an administrator
can set a book's series and number from that page.

The part worth remembering is why editing needed a table of its own. A series is derived:
`Resolve` takes the winning source row whole and rewrites `books` from it. An edit written
into `books` would therefore survive exactly until Calibre next reported that book changed
— which looks like it works, right up until it silently does not. So the edit lives in
`book_series_overrides` and `apply` lays it over the top, the same shape as `books.hidden`.
`TestASeriesSetHereSurvivesAScan` is the test that would have caught the naive version.

Two smaller decisions inside it:

- The row's presence is the override, not its contents. An empty name means "in no series"
  and holds against a library that disagrees; no row means "whatever the library says".
- Nothing had to be added to tell a device. `servingFields` already covered the series, so
  an edit moves `serving_hash`, `metadata_rev` follows, and the next sync sends
  `ChangedProductMetadata` unprompted.

A book with no number sorts last on the series page rather than first: an unnumbered volume
is almost always a companion, and putting it ahead of book one is the worse mistake.

### Every reader was in the list twice

Reported from a real device: the readers page showed a Kobo Libra Colour and, above it, a
nameless row on the same token that had never synced and had no device id.

A device is keyed on `(token_hash, kobo_device_id)`, and the id comes from a header. The
header is not on every request: `/v1/auth/device`, `/v1/affiliate` and `/v1/initialization`
arrive without it, and it only starts appearing once the device reaches `/v1/user/profile`.
Those first requests were being filed under an id of `''` — a different key, hence a second
row. Nothing failed, which is why it survived this long; it is now a LANDMINE note in
`docs/kobo-protocol.md` §2.

`UpsertDevice` now names the nameless row as soon as the id turns up, and a request without
one attaches to the row the token already has instead of opening another. Migration 0008
clears out the duplicates already made — narrowly: only a row with no device id, that never
completed a sync, and whose token has a real row to keep. A token whose only row is nameless
is a reader that has not sent its id yet, and it is left alone.

Two readers sharing one token are still two readers; `TestTwoReadersOnOneTokenStayApart`
holds that line.

### One section, three views

The spine had grown a row per page, and three of those rows — the overview, the books and
the series — were the same section looked at from three angles. They now share one entry,
"Library", and separate along the top of the page instead.

`libraryNav` in the func map is what decides membership, asked by both the spine and the
strip, so the two cannot disagree about which pages belong together. The strip is an
underline rather than a filled pill on purpose: it is a division *inside* a section the
spine has already highlighted, and a second row of pills would read as competing with it.

`TestLibrarySectionSharesOneSpineEntry` holds both halves — the three pages show the strip
with the current one marked, and a page outside the section shows none.

### The race detector left CI

`go test -race ./...` roughly triples the job, and the test workflow gates every push. It
now runs the plain tests, and the race detector is `make race`.

This is a real trade-off rather than a tidy-up: the reason the detector was there is that a
scan rewrites the same rows a paginated sync is reading from, and that is precisely the
class of bug it catches. Run it before a release, and after anything touching the scanner,
the scheduler or the sync engine.

### A Makefile

`make build | run | test | race | vet | check | migrate | fmt | tidy | docker | clean`.

It has to work on Windows, where make runs recipes through cmd.exe rather than a shell.
Two things follow, and they are why the file looks odd: `VAR=value command` is shell syntax
cmd.exe does not have, so environment is set with make's own target-specific `export`; and
`rm` is not there, so the one recipe that needs it switches on `$(OS)`.

### The proxy stopped redirecting

`GET` to an endpoint kobibri does not implement used to be answered with a 307 to
storeapi.kobo.com. Cheap, and it worked, but it meant the device talked to the store
directly: nothing about that exchange passed through here, so the log could say what was
asked for and never what came back.

Every method is now relayed. Headers go **verbatim** in both directions and none of ours
are added — no `Via`, no `X-Forwarded-*` — so the store sees the reader rather than
something standing in front of it. The one thing still dropped is the hop-by-hop set, which
describes this connection rather than the next.

Two consequences worth knowing. A firmware image or a purchased book now comes through the
server, so the HTTP client lost its ten-second overall deadline and got a response-header
timeout instead — a blanket timeout would cut a large download off part-way. And
`content-encoding` is no longer stripped from the response: the device's `accept-encoding`
is forwarded now, so the store may answer gzipped and the header describing the body has to
travel with it.

## What was decided against

Kept here rather than deleted, because an empty backlog and a considered "no" look
identical a year later.

- **Ratings from the device.** Not part of the sync protocol — `ReadingState`
  carries status, bookmark and statistics and nothing else. The proxy logs every
  endpoint a device asks for and does not get, and nothing resembling a rating has
  appeared in one. Reopen if such a line ever shows up; do not go looking for it
  by guessing at request shapes.
- **A run against a physical Kobo.** Not wanted. Everything is verified against a
  simulated device, a random-sequence property test and a real library of books.
- **TXT, HTMLZ, DOCX and the rest.** See the table above: Calibre does them, and
  writing our own added nothing anyone asked for.
