# kobibri — working notes

One file. Design, protocol and journal. Terse on purpose: append a few lines, never a page.

kobibri reads Calibre libraries off the filesystem and serves them to Kobo e-readers by
emulating the store sync API. **A sync is reconciliation between immutable snapshots, not a
delta over timestamps.**

## Principles

1. **The device is the fragile party.** Everything under `/kobo/` answers `200 {}`, never an
   error status. One 4xx from this host aborts the whole sync.
2. **Reconciliation, not deltas.** No timestamp watermarks anywhere in the sync path.
3. **Canonical book ids are forever.** The only identifier a device knows.
4. **Few dependencies, one static binary.** `CGO_ENABLED=0`.

## Layout

```
cmd/kobibri/     serve | migrate | source | ingest | convert | token | scan
internal/
  config/ store/ calibre/(calibretest/) ingest/ kobo/ kepubconv/ fb2/ covers/
  httpx/ web/ reader/ webimport/ upload/ ebookconv/ textindex/
```

Deps: stdlib `ServeMux` (Go 1.22 patterns), `modernc.org/sqlite` (cgo-free; DSN is
`_pragma=name(value)`, not mattn's form), hand-rolled migrations over `PRAGMA user_version`,
`google/uuid` (v3 over NAMESPACE_DNS for `Series.Id`), `x/image/draw`, `x/sync`,
`x/crypto/bcrypt`, `html/template`. kepubify was dropped in M19 — conversion is ours.

## Database

Two handles over one file: a **writer** capped at one connection (removes `SQLITE_BUSY` as a
class) and a reader pool. Timestamps are RFC3339 UTC text, so string order = time order.
Schema in `internal/store/migrations/`; an applied migration is never edited, only followed
by `<N+1>_<name>.sql`.

Tables: `sources`/`source_acl`, `source_books`/`source_book_files`/`source_book_columns`,
`books`, `book_identities`, `book_series_overrides`, `devices`/`device_tombstones`,
`sync_points`/`sync_point_books`/`sync_point_tags`, `sync_runs`, `reading_states`,
`reading_events`, `book_text_index`/`book_text_docs`/`book_text_blocks`, `tags`/`tag_books`,
`kepub_cache`/`kepub_failures`/`cover_cache`, `kv`, `sessions`.

## Invariants — breaking one breaks everything

- `books.id` issued once, **never** deleted or reissued. `DELETE FROM books` outside an
  explicit admin purge is forbidden. Ingest never deletes `source_books`; it sets `missing=1`.
- A book vanishing from the server does **not** remove it from a device. Removal is
  device-initiated (`DELETE /v1/library/{uuid}`) → a **per-device** tombstone, permanent,
  subtracted *after* the carry-forward union.
- A snapshot is immutable once created. That is what makes an interrupted sync resumable.
- **Carry-forward absorbs accidental disappearance; `hidden` is intentional removal.** A book
  gone from its source is carried over from the parent snapshot and produces nothing; a
  hidden book falls out and is retracted with `IsRemoved: true`.
- When a book becomes unavailable its serving metadata is **frozen**, not recomputed —
  otherwise `serving_hash` moves, `metadata_rev` moves, and a device hears about a book it
  was supposed to hear nothing about. Availability is deliberately not in the hash.
- A **derived** field cannot be edited by writing to `books`: `Resolve` rewrites them from
  the winning source row. It goes in an override table `apply` lays on top
  (`book_series_overrides`, `books.hidden`). **Presence of the row is the override**, not its
  contents — an empty series name means "in no series".
- A device is keyed on `(token_hash, kobo_device_id)`, and the id header is **absent** on
  `/v1/auth/device`, `/v1/affiliate`, `/v1/initialization`.
- Exactly **one** format is offered per book. Both KEPUB and EPUB lets the device pick EPUB
  and silently lose span-level progress.
- A fixed-layout book (`EPUB3FL`) is never converted; the `.kepub.epub` suffix is
  load-bearing all the way to `Content-Disposition`.
- A changed book goes out as `NewEntitlement` + `ChangedProductMetadata` +
  `ChangedReadingState`. `ChangedEntitlement` is for retraction only.
- A web source is **never scanned** (no `metadata.db`; a scan would mark every book missing).

## Ingest

Identity keys, strongest first: `calibre_uuid`, `isbn` (10→13, checksum validated — a bad
checksum shared by two books would merge them), `titleauthor` (normalised NFKD, always
present, the only one that can be wrong). `Attach`: no match → new canonical book; one →
attach; several → merge, survivor is oldest by `created_at`, ties by smallest id, losers keep
`merged_into` as a permanent alias.

`Resolve` takes the winner **whole** (no Frankenstein metadata); only empty fields fall back.
`metadata_rev` moves only when `serving_hash` changes. Any change to which source rows are
live must be followed by re-resolve — a scan will not do it (`Scanner.SetSourceEnabled` binds
them). A **vanish guard** rolls back a scan that would flag >20% or >25 books missing.

`calibre.Open` snapshot-copies `metadata.db` + `-wal`/`-shm` to a temp dir, verifies the
signature did not change mid-copy, runs `quick_check`. Never opens the user's DB writable.
Two phases: A reads `id, uuid, last_modified` for everything; B reads full records for the
changed set only, joining in Go (ordered `group_concat` needs SQLite ≥3.44). `ErrUnreachable`
changes **nothing** — an unmounted share is not a library that lost every book.

Undoing a bad merge: `SuspectMerges` lists merges resting on `titleauthor` alone; `Split`
keeps the original id and gives the leaving copy a new one, pinned via
`source_books.pinned_book_id` or the next scan merges them straight back.

## Sync engine

```
Snapshot = ( { syncable, visible books } ∪ Books(parent) ) \ Tombstones(device)
```

Materialised with one `INSERT … SELECT`. A snapshot also records a **fingerprint** —
counts and revision sums of visible books, reading states, collections, tombstones, enabled
sources — computed from the data rather than kept as a counter, because a forgotten bump
means a device silently stops receiving updates. Matching fingerprint → `[]`, nothing written.
`TestEveryChangeIsNoticed` walks each kind of change.

The diff is seven keyset-paginated queries over two snapshots, drained in a **fixed order**
without interleaving: new books, changed, removed, changed reading states, new/changed/deleted
tags. Budget counts books, not JSON objects (a changed book costs three); when it runs out the
cursor is saved and `x-kobo-sync: continue` is set. The token carries only references
(`{ongoing, last, raw}`, prefixed `KOBIBRI.`); a token without the prefix is the real store's
and is kept verbatim. The parent snapshot is deleted only when its child completes.

## Kobo protocol

No official docs. Verified against calibre-web, Calibre-Web-Automated, Komga, kobink,
kobo-book-downloader, and a Kobo Libra Colour on fw 4.45.23697.

Pointing a device: `.kobo/Kobo/Kobo eReader.conf`, `[OneStoreServices] api_endpoint=<root>`,
restart. Auth is an opaque secret in the URL path (`/kobo/<token>/v1/...`); DeviceId+UserKey
are irrevocable credentials and are ignored, `/v1/auth/*` returns random tokens never checked.

### `GET /v1/library/sync`

Flat JSON array of single-key objects, always an array. Keys: `NewEntitlement`,
`ChangedEntitlement`, `ChangedProductMetadata` (bare `BookMetadata`), `NewTag`/`ChangedTag`,
`DeletedTag` (`{Id, LastModified}` only), `ChangedReadingState`. Headers:
`x-kobo-synctoken` always, `x-kobo-sync: continue` when more.

Every uuid field (`Id`, `RevisionId`, `CrossRevisionId`, `EntitlementId`, `WorkId`,
`CoverImageId`) is the same per-book uuid. `Categories`/`Genre` =
`00000000-0000-0000-0000-000000000001`. `Accessibility: Full`, `Status: Active`,
`OriginCategory: Imported`. `Series.Id = uuid3(NAMESPACE_DNS, name)`. Timestamps
`%Y-%m-%dT%H:%M:%SZ`. `DownloadUrls`: `{Format, Size, Url, Platform, DrmType}`,
Format ∈ EPUB|EPUB3|EPUB3FL|KEPUB.

Removal: there is no `DeletedEntitlement` — re-send with `IsRemoved: true` (→ Archive; set
back to false to restore). Komga sends stub metadata for removed books.

### Reading state

`GET /v1/library/<uuid>/state` → **array of one**:

```json
{"EntitlementId":"…","Created":"…","LastModified":"…","PriorityTimestamp":"…",
 "StatusInfo":{"LastModified":"…","Status":"Reading","TimesStartedReading":1,"LastTimeStartedReading":"…"},
 "Statistics":{"LastModified":"…","SpentReadingMinutes":42,"RemainingTimeMinutes":180},
 "CurrentBookmark":{"LastModified":"…","ProgressPercent":37,"ContentSourceProgressPercent":61,
   "Location":{"Value":"kobo.12.3","Type":"KoboSpan","Source":"OEBPS/chapter05.xhtml"}}}
```

`PUT` body `{"ReadingStates":[{CurrentBookmark, Statistics, StatusInfo, LastModified}]}` →
`{"RequestResult":"Success","UpdateResults":[{EntitlementId, CurrentBookmarkResult,
StatisticsResult, StatusInfoResult, LastModified, PriorityTimestamp}]}`, each `Result` ∈
Success|Failure|Ignored. Status ∈ ReadyToRead|Reading|Finished.

Measured on fw 4.45.23697: the device sends `StatusInfo` with **only** `Status` and
`LastModified` — no `TimesStartedReading`. Each PUT carried all three sections in practice,
but the API is per-section by design, which is why each has its own `Result`: a section the
device did not send is answered `Ignored` and left as it was in storage. Writing null over a
bookmark because a status-only update arrived loses the reader's place.

`SpentReadingMinutes` is a cumulative **per-device** counter of real reading, not wall-clock:
28 minutes of elapsed time with 13 minutes on the counter is normal, and its delta is the only
honest denominator for a speed figure. `RemainingTimeMinutes` is the device's own estimate and
is close to useless on a long book — 4144 → 4139 across half an hour of reading.

`ProgressPercent` is a **whole number**, so it does not move at all inside a long book: four
reports across an evening all said 3. Anything measuring reading has to use `Location`.

### Tags / collections

`POST /v1/library/tags` `{Name, Items}` → 201 + bare uuid string. `PUT /v1/library/tags/<id>`
`{Name}`. Items bodies: `{"Items":[{"RevisionId":…,"Type":"ProductRevisionTagItem"}]}`.
`DELETE /v1/library/tags` answers **405** on purpose so it cannot shadow book deletion.

### Covers

Two templates, 5- and 6-segment, both served. `ImageId` = book uuid, with the cover's mtime
appended so a replaced cover arrives under a new id; the handler strips the suffix. Always
`image/jpeg`, pre-scaled into buckets by height, never upscaled, 3:4. No cover → placeholder
with **200**, `no-store`.

### LANDMINES — these fail silently

- **`/v1/initialization` is persisted forever.** The device writes every key of `Resources`
  into `[OneStoreServices]` permanently and uses those, not `api_endpoint`. A truncated
  response has wedged a device into never syncing again. Set `x-kobo-apitoken: e30=`.
- **A cached map outlives the server that set it.** A device moved between servers keeps the
  old `image_host`. Answering `/v1/initialization` does not reliably refresh it. Repair is
  hand-editing the file.
- **`image_host` is a prefix, not a hostname** — Kobo's own is `//cdn.kobo.com/book-images/`,
  the literal prefix of both templates. All three keys must name one place, with the token on
  it. calibre-web has them split; kobibri did too until 2026-08-24.
- **Placeholder casing:** `{ImageId}`, `{Width}`, `{Height}`, `{Quality}`, `{IsGreyscale}`.
  calibre-web emits lowercase and a literal `isGreyscale` — its bug, and devices follow it.
  The literal `false` in `image_url_template` is *not* a bug; Kobo hardcodes it too.
- **Not every request carries `x-kobo-deviceid`.** `/v1/auth/device`, `/v1/affiliate`,
  `/v1/initialization` arrive without it. Taking it at face value files them under `''` and
  every reader appears twice, the second nameless and unaddressable.
- **The device prefers `api_endpoint` over the map** for anything under the API (measured:
  served the native map, it still asked *this* server for `product_prices`). The map matters
  for hosts outside `api_endpoint` — in practice the image host. Overriding keys is a belt,
  not the trousers.
- **The store's map is unobtainable after first contact.** Once we answer `/v1/auth/device`
  the device holds our token; forwarding it upstream gets `400 Invalid token version`. Only
  `/v1/affiliate` and `/v1/initialization` validate it — `profile`, `wishlist`, `deals`,
  `loyalty/benefits`, `products/*/prices`, `products/*/nextread` all answer 200 with real data.
- **A 4xx from this host kills the whole sync.** The device posts
  `{"EventType":"FailedSync","reason":"WebRequestErr"}` and discards a perfectly good
  `/v1/library/sync` that arrives a second later. The same 4xx collected from storeapi via a
  307 is survivable — a 4xx on `api_endpoint` reads as the sync server failing. So: relay
  every method, copy back only <400, log and drop the rest, answer with our own stub.
- **A privacy decision in the proxy must also be made in `resourceOverrides`.** While
  `post_analytics_event` held Kobo's URL the device posted telemetry straight to storeapi and
  this server never saw it. Both analytics keys are claimed.
- **`Items` is capitalised** in `tag_items` (`…/tags/{TagId}/Items`) while `delete_tag_items`
  beside it is lowercase. `ServeMux` is case-sensitive; both spellings route here.
- **`ProgressPercent` ≠ `ContentSourceProgressPercent`.** The first is the whole book, the
  second the current spine file. Measured: 19 pages into 760 → `ProgressPercent: 2`,
  `ContentSourceProgressPercent: 42`. Show the first; fall back only when absent, and tell
  absent apart from zero (fields are pointers). Code and test held the same inverted belief
  for months, which is why a green test proved nothing.
- **A finished book sends the first resource as its position, not the last.** On `Finished`,
  override server-side.
- **`Location.Type: KoboSpan` only works for kepub.** Plain EPUB gives chapter granularity.
- **Calibre's `UNDEFINED_DATE`** (`datetime(101,1,1)`) reaches the wire as
  `"0101-01-01T00:00:00Z"`; 60 of 63 books in a real library carried it. `parseTime` returns
  zero for any year ≤101 and marshals as `null`.
- **Category order is fixed** and categories must not interleave.
- **`ChangedEntitlement` ignores a nested `ReadingState`.**
- **The `.kepub.epub` extension is load-bearing** — Nickel picks its renderer by filename.
- **The device caches covers by ImageId indefinitely.**
- **Kobo's TLS stack is old** — keep TLS 1.2, no HTTP/2 when serving TLS directly. Some
  firmwares fail hostname resolution where a raw IP works. Reverse proxies need enlarged
  buffers or sync 502s partway.
- **Devices send portless/bare-IPv6 `Host` headers** — repair before building absolute URLs.
- **`r.PathValue` is empty in middleware** (only populated after ServeMux matches); parse the
  token from `r.URL.Path`.
- **`PUT /v1/library/tags/{id}` and `PUT /v1/library/{uuid}/state` are the same path shape** —
  ServeMux refuses the ambiguity; one handler dispatches on the segments.

Ratings are **not** part of sync. Rating on a device talks to `/v1/products/…`. Every proxied
endpoint is logged (token stripped, book ids collapsed to `{id}`, credential-shaped query
values redacted) so an unimplemented call shows up as a line rather than a guess.

## Conversion

**Any format → EPUB** is Calibre's `ebook-convert` as a subprocess (searched in the macOS
bundle too — Calibre installed but not on PATH looked exactly like no Calibre). FB2 is native
(`internal/fb2`): one XML file, pictures inlined; decode whatever encoding the declaration
names (first real file was windows-1251), one EPUB chapter per **top-level** section only.
A book is advertised only when it can actually be delivered.

**EPUB → KEPUB** is ours (`internal/kepubconv/native.go`). Lazy on download, prewarmed in
background, never inside a scan; cache keyed on book id + fingerprint(path, size, mtime),
`singleflight`, temp name then rename. Format order: pre-paginated EPUB → the library's own
KEPUB → reflowable EPUB → whatever the converter can produce. A library's KEPUB is served
untouched (`convert_from = KEPUB`); re-converting would nest koboSpans and throw positions away.

Span rules, pinned by `spec_test.go` and golden kepubify output in `testdata/golden`:

- Ids are `kobo.<block>.<segment>`, both from one, reading order, unique per chapter. **This
  is where reading positions live** — different numbering moves every saved position.
- The block counter is a **walking, deferred** counter: incremented on entering `p`, `h1`–`h6`,
  `ul`, `ol`, `table`, but an element holding no text never consumes a number. Text before any
  of them is `kobo.0.n`.
- Content documents are parsed as **XML**, not HTML (`<title/>` self-closes in XHTML; an HTML
  parser swallows the rest of the chapter as RCDATA — a whole chapter lost per book).
- The sentence splitter counts only ASCII whitespace, while "is this whitespace" uses
  Unicode's — so a stray `&#160;` is not spanned. Matching one and missing the other shifts
  every id after it.
- Deliberately naive: `«a quoted sentence.» Then another.` is **not** split, because kepubify
  does not split there and every device already holds those ids.
- `<pre>` is left unspanned; an existing span is kept with the koboSpan nested inside.
- Wrappers `div#book-columns`/`div#book-inner` + the kobostylehacks style; own XHTML renderer
  (`html.Render` writes HTML5 and would leave `<img>` unclosed); `mimetype` first and stored.

Fifty-seven real books, not fixtures, is what made dropping kepubify safe — the first run
matched two of fifty-five while every hand-written fixture passed. Re-recording the golden
files needs a real kepubify on PATH, deliberately: it is the only remaining evidence our ids
match the ones on people's devices.

## Reading statistics

`reading_states` keeps one row per (user, book) and every PUT overwrites it, so before
migration 0010 every progress report was destroyed by the next one. `reading_events` is
append-only, written in the same transaction as the state update, a few rows an hour per
reader. A report identical to the last one from that device is dropped — a woken device
resends its position, and a history full of those turns every average into a lie.

What makes a position mean something is `internal/textindex`: it opens **the file the device
actually receives** — the converted KEPUB, or the library's own KEPUB where that is what is
served — and records how many words stand before every `kobo.<block>` id, per content
document, book-wide. Two reported positions then subtract into words read. The index is
rebuilt when the file's fingerprint moves, so a serial that gained chapters re-measures and
old events land on the new offsets. It is built off the request goroutine when a progress
report arrives for an unmeasured book, and swept for books read before any of this existed.

Word counting treats a Han, Hiragana or Katakana character as a word of its own: those
scripts are written without spaces, and `strings.Fields` over a Japanese paragraph would put
a reading speed at three words a minute.

Two rules the numbers depend on:

- **The denominator is measured minutes, not all minutes.** Minutes whose interval has no
  known position at both ends — the first report of a book, a status-only report, a second
  device's counter arriving mid-book — are real reading time and are shown as such, but
  putting them under the words read drags every speed down. Both are tracked.
- **Deltas are per (book, device).** `SpentReadingMinutes` belongs to one device; subtracting
  one device's counter from another's invents or destroys hours. A negative delta is a reset
  or a re-added book and clamps to zero.

Sittings are split by an hour's gap — wider than the fifteen-to-thirty minutes between
reports, narrow enough to separate an evening from the next morning. `/stats` is the year
calendar, the hour-of-day histogram and the book table; a book's own page carries its
sittings, its pace per chapter and a finishing date projected from minutes on the days it was
actually opened. Days and hours are bucketed in the **server's** time zone: it is read by the
person the server belongs to, and UTC would put an evening's reading on the wrong square.

## The rest of the server

- **Collections** are per user, rebuilt after every scan, idempotent (compare membership
  before writing, or every shelf is re-announced). A shelf a reader deleted stays deleted —
  origin distinguishes it from a tag that left Calibre and came back. Library tags, series and
  chosen `#custom` columns can be mirrored; off by default (200 tags = 200 shelves). Calibre
  stores a column in one of two shapes and says which via `normalized`; only text/enum/series
  can name a shelf; choosing one forces a full re-read of that library.
  `collections:loose` names a catch-all shelf for books in no series, empty for none. It only
  applies while series become shelves, and it is **a shelf, not a series**: writing a series
  name into those books would put "Standalones #1" under the title on the device and move the
  serving hash of most of the library over one setting.
- **Series** reach a device as the `Series` object unconditionally and as shelves optionally.
  `/series` and `/series/{uuid}` (reading order; no number sorts last). A series is its name —
  `series_uuid` is derived from it — so creating one is naming it on a book. The editor lives
  on the **book's own page**, because a book in no series appears on no series page and could
  otherwise never be given one; the series page carries a search for books to add, so filling
  a series is not a matter of visiting ten book pages.
- **Uploads** land in one source of their own at priority 0. Metadata from the EPUB, so a file
  exported from Calibre merges with the library copy. Removing marks the row missing.
- **Web import** (`internal/webimport` + `fess932/novelkit`, v0.6.0, no workarounds left):
  identity is `weburl:<link>`; re-importing is the update path and keeps earlier chapters
  byte-identical, so reading positions survive (two tests hold that). Choosing a translation
  is a first-class step and part of the identity key. One download per book at a time;
  periodic chapter check on `KOBIBRI_IMPORT_CHECK_EVERY`.
- **Covers** for non-Calibre books come out of the file (EPUB3 manifest property → EPUB2
  metadata pointer → an image merely named like one) and are written beside it as
  `cover.<ext>`. For FB2/AZW3/MOBI that happens after conversion, on the EPUB. Never written
  into a Calibre library — there is a test, not a comment. `BackfillCovers` is a one-off
  keyed in `kv`.
- **`/opds`** is OPDS 1.2, HTTP Basic (a reading app has no login form; a successful pair is
  cached a minute), same per-user visibility as the sync snapshot, EPUB link first.
- **`internal/reader`** opens the file that syncs, one chapter at a time, so a conversion can
  be checked without a Kobo. Untrusted: `sandbox=""`, restrictive CSP naming this server's
  address outright — `'self'` matches nothing in an opaque origin and refused every image.
- **Web UI**: server-rendered, embedded assets, bcrypt, CSRF on every mutating form. E-ink
  palette. Navigation is two levels; `libraryNav` decides membership and both levels ask it.
  English is the source language, catalogue in `internal/web/i18n.go` carries English and
  Russian, missing phrase falls back to English. The server itself stays English.
  **Never concatenate a translated fragment with a Go string** — a phrase that names something
  takes `%s` and is built with `Msg(key, arg)`. A template calling a missing func compiles
  fine and fails at parse time; `TestEveryPageRenders` catches it.
- **`Content-Disposition`** is RFC 5987 percent-encoded UTF-8 (not `mime.QEncoding`, which is
  for mail headers), with a transliterated ASCII fallback. One builder in `httpx`.
- **Library order**: newest first by default, by title, or by what was read last
  (`COALESCE(NULLIF(rs.last_modified,''), b.created_at) DESC`). The device orders its own home
  screen from `PriorityTimestamp` and asks for it with `PrioritizeRecentReads=true`.
- **`internal/kobo/openapi.json`** at `/api/kobo.json`, drawn at `/api`. Written half = what we
  serve (a missing path fails `TestOpenAPIParsesAndCoversWhatWeServe`); derived half is walked
  out of `nativeResources` at serve time, no schemas — an unobserved response is an invented
  one. `x-kobibri` says served/relayed/proxyable; `x-kobibri-evidence` says
  observed/implemented/unproven. Parsed once in `web.New` so a broken document stops the server.
- **Janitor** hourly: trims rebuildable caches, removes abandoned snapshots and expired
  sessions. A device's single completed snapshot is never touched.
- **Deployment**: one static binary; `deploy/` has systemd, an env file and an nginx fragment.

## Testing

`calibretest` builds real Calibre libraries on disk. A **fake device** replays the sync
conversation and keeps what a real Kobo would end up holding. `property_test.go` throws random
sequences of eleven kinds of operation at two devices and checks after every step that a device
never loses a book and never holds one book under two identities — seed comes from the round,
every operation is logged, and breaking carry-forward makes it fail in six steps. A soak test
covers edits, deletion, a source toggled, a self-deletion and interrupted syncs. Every LANDMINE
has a test. The race detector is **not** in CI (`make race`) — run it after touching the
scanner, the scheduler or the sync engine.

```
make check   # vet + test          make race    # not in CI
make migrate                       make run DATA=./data BASE_URL=http://192.168.1.10:8078
```

The Makefile must run under cmd.exe: target-specific `export`, not `VAR=value`, and `rm`
switched on `$(OS)`. A background server started in one shell invocation does not survive into
the next — start it and exercise it in the same command.

## Style

- Standard library first; a new dependency has to earn its place.
- **Do not write comments** (owner's instruction, 2026-08-23). Name things so the code says
  it; the *why* goes here, where it survives. The rare exception is a line that looks wrong on
  purpose because of a firmware quirk — one short line pointing at the landmine.
- Scripted whitespace-sensitive edits on Go source are unreliable — `gofmt` realigns struct
  fields and map keys, so an anchor silently stops matching and the edit does nothing. It has
  cost two debugging sessions. Edit Go declarations directly.
- **`golangci-lint` on its stock set, with no `.golangci.yml`.** An error dropped on purpose is
  written `_ = f()` at the call rather than excluded in config. An exclusion list for the
  `Close` family would be shorter and would also blind the linter to the cases that matter — a
  `Close` on a file being written, where a failed flush loses the tail while the copy reports
  success. Every such site here checks `Sync()` first, which is why the discards are safe.
- **golangci-lint's defaults hide most of what it found.** `max-issues-per-linter` is 50 and
  `max-same-issues` is 3, so this tree reported 50 errcheck findings when it had 268. Pass
  `--max-issues-per-linter=0 --max-same-issues=0` before believing a count.
- **A table with readers and no writers is a feature someone believes in.** Listing tables and
  counting INSERT/UPDATE vs SELECT found two half-built features (`source_acl`, `sync_runs`).
  An exported function nothing calls is the same smell one layer up.

## Open

- **Word counts are approximate for CJK and for `<pre>`.** An ideograph counts as a word,
  which is a convention rather than a measurement, and an unspanned `<pre>` block resolves
  only to the document it is in.
- **A book with no koboSpans** — a plain EPUB served to a device — resolves positions only to
  the start of a content document, so its speed is coarse. That is the same limitation the
  device itself has.
- Formats: AZW3/MOBI/LIT/PDB/HTMLZ/RTF/DOCX/TXT need Calibre and nothing further is planned;
  PDF/CBZ/CBR/DJVU never (Kobo does not sync them). The interface lists what **this machine**
  can do, not the table.

Decided against, kept so a considered no does not look like an empty backlog: ratings from the
device (not in the protocol — reopen only if a proxy log line shows one); a run against
physical hardware as a gate; writing our own TXT/DOCX converters.

## Log

Newest last. One or two lines each — what changed and the non-obvious why.

- 2026-08-25 — Three docs (ARCHITECTURE, kobo-protocol, PROGRESS; 2658 lines) collapsed into
  this file. The milestone-by-milestone journal was dropped: it recorded what it cost, not what
  is true now, and git holds it.
- 2026-08-25 — Series editing where a person would look for it: on the book page, with the
  names in use offered as suggestions, plus an add-a-book search on the series page and an
  optional catch-all shelf for books in no series.
- 2026-08-25 — Reading history and statistics. Migration 0010, `internal/textindex`, `/stats`,
  a panel on the book page. Two bugs fixed on the way: a status-only PUT wiped the stored
  bookmark and statistics while answering `Success`, and `TimesStartedReading` was parsed and
  dropped so every answer said zero.
