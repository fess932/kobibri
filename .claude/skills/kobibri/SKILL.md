---
name: kobibri
description: Working on kobibri, a Calibre → Kobo sync server. Use for ANY task in this repository - before writing code (which docs to read), when behaviour changes (which docs to update), when something new is learned about the Kobo protocol, and when a milestone is finished. Triggers - kobo, calibre, sync, entitlement, kepub, metadata.db, sync point, tombstone, milestone M1..M10.
---

# kobibri

A server that reads several Calibre libraries off the filesystem and serves them to Kobo
e-readers by emulating the store sync API. The governing idea: **a sync is reconciliation
between immutable snapshots, not a delta over timestamps.**

## The documents are the source of truth

| File | What is in it | When to read it |
|---|---|---|
| `docs/ARCHITECTURE.md` | The design and the reasoning behind it: schema, sync engine, routing, conversion | Before any task — find the relevant section |
| `docs/kobo-protocol.md` | Reverse-engineered Kobo protocol: exact JSON shapes, headers, firmware quirks | Before any code that touches the wire (`internal/kobo/**`) |
| `docs/PROGRESS.md` | Milestone status, decisions, closed and open risks, backlog | At the start and end of a session |

Read them rather than reconstructing from memory. Sections marked **LANDMINE** in
`kobo-protocol.md` are places where a mistake does not fail — it quietly breaks the device.

## Before starting

1. Open `docs/PROGRESS.md` — which milestone is current, which risks are open.
2. Open the relevant part of `docs/ARCHITECTURE.md`.
3. If the task touches Kobo requests or responses, check the shapes against
   `docs/kobo-protocol.md`. Field names, placeholder casing and the presence of a header
   are all load-bearing there.

## After changing anything — update the documents

This is not optional. Context does not survive between sessions; documents do.

- **Finished a milestone** → update the status in `docs/PROGRESS.md` and record in a
  paragraph what was actually built and how it was verified.
- **Departed from the design** → fix the relevant part of `docs/ARCHITECTURE.md` and say
  why. It is a living document; a description that disagrees with the code is worse than
  no description.
- **Learned something about the protocol** (device behaviour, a response shape, a firmware
  bug) → add it to `docs/kobo-protocol.md`, mark it `LANDMINE` if it fails silently, and
  cite the source.
- **Closed a risk** (verified in practice something that was previously assumed) → move it
  in `docs/PROGRESS.md` with the actual measured result.
- **Had an idea outside the current milestone** → the backlog in `docs/PROGRESS.md`, not
  the code.
- **Changed the package layout, the build commands or the documents themselves** → update
  this SKILL.md.

## Invariants — breaking one breaks everything

- `books.id` is issued once and **never** deleted or reissued. It is the only identifier
  the device knows. `DELETE FROM books` outside an explicit administrative purge is
  forbidden.
- Ingest **never** deletes `source_books` rows; it sets `missing = 1`.
- A book disappearing from the server does **not** remove it from the device. Removal is
  device-initiated (`DELETE /v1/library/{uuid}`) and produces a **per-device** tombstone.
- A snapshot (`sync_points` + `sync_point_books`) is immutable once created. That is what
  makes an interrupted sync resumable without loss.
- A **derived** field cannot be edited by writing to `books`: `Resolve` rewrites them all
  from the winning source row, so the edit survives only until the next scan touches that
  book. It goes in an override table that `apply` lays over the top — see
  `book_series_overrides`. Presence of the row is the override, not its contents.
- A device is keyed on `(token_hash, kobo_device_id)`, and **the id header is absent** on
  `/v1/auth/device`, `/v1/affiliate` and `/v1/initialization`. Taking it at face value
  files those under `''` and gives every reader a second, nameless row. Nothing fails.
- **Carry-forward absorbs accidental disappearance; `hidden` is intentional removal.** A
  book gone from its source is carried over from the parent snapshot and produces nothing;
  a book hidden by an operator falls out of the snapshot and is retracted with
  `IsRemoved: true`. Do not conflate the two.
- When a book becomes unavailable its serving metadata is **frozen**, not recomputed:
  otherwise `serving_hash` changes, `metadata_rev` moves, and the device is told about a
  book it was supposed to hear nothing about.
- A changed book goes out as `NewEntitlement` + `ChangedProductMetadata` +
  `ChangedReadingState`. `ChangedEntitlement` is for retraction only.
- Exactly **one** format is offered to the device. Offering both KEPUB and EPUB lets it
  pick EPUB and silently lose span-level progress.
- A fixed-layout book (`EPUB3FL`) is never converted: it already has one page per chapter,
  and conversion breaks full-screen rendering.
- The `.kepub.epub` suffix is load-bearing and must survive through the cache path and the
  `Content-Disposition` filename. Kobo picks its renderer by filename.
- Conversion runs on a background queue (`kepubconv.Prewarmer`), never inside a scan: a
  synchronous conversion would hold the single SQLite writer connection.
- The protocol reuses the path shape `PUT /v1/library/X/Y` for renaming a collection and
  for reading progress. ServeMux cannot disambiguate it — `handleLibraryPut` does. Do not
  try to separate them with routes.
- Collections are soft-deleted (`deleted_at`): without the row the diff cannot send
  `DeletedTag` and the collection sticks on every device forever. Same principle as books
  — you can only delete what you still remember.
- Any endpoint under `/kobo/` answers `200 {}` on error, never 4xx or 5xx: an error on an
  incidental endpoint aborts the whole sync.
- `r.PathValue` is **empty in middleware** — it is only populated after ServeMux matches.
  The token is parsed from `r.URL.Path` (`pathToken`) and carried in the context. Getting
  this wrong once made the server silently reject every request.
- Every absolute URL handed to a device is built through `httpx.URLBuilder`. One place
  that can get it wrong — and a mistake in `/v1/initialization` is irreversible.
- `metadata_rev` moves **only** when `serving_hash` changes, or every scan becomes an
  update storm on every device.
- Any change to which source rows are live (enable, disable, priority, removal) must be
  followed by re-resolving the affected books: a scan will not do it, because nothing
  changed in Calibre and the books never enter the changed set. See
  `ingest.Scanner.SetSourceEnabled`.

## Importing from a link

- Downloading, chapter caching and EPUB assembly belong to `github.com/fess932/novelkit`.
  Adding a site is a change **there**, not here — it already has the provider abstraction.
- `job.Store.Plan` is idempotent and additive: it derives the cache directory from the
  book, keeps what is downloaded and appends newly published chapters. Always plan; do not
  try to reuse a job by hand.
- An imported book is identified by its link (`weburl:`), because it has neither a Calibre
  uuid nor an ISBN.
- A web source is **never scanned**: it has no `metadata.db`, and a scan would mark every
  imported book as vanished. `Scanner.Scan` returns early on `store.SourceKindWeb`.

## The web interface

- English is the source language; the catalogue in `internal/web/i18n.go` carries English
  and Russian, and a missing phrase falls back to English rather than showing its key.
- The server itself — logs, errors, the sync API — stays English regardless of the
  interface language.
- **Never build a sentence by concatenating a translated fragment with a Go string.** That
  is how "как есть в as it is in Main shelf" reached a page. A phrase that names something
  gets a `%s` in the catalogue and is built with `Msg(key, arg)`, which `T` unpacks — that
  is also what carries a flash message through a redirect.
- Anything a person reads in the browser belongs in the catalogue, including text assembled
  in a handler: dashboard warnings, flash messages, `http.Error` bodies.
- A template that calls a function missing from the func map **compiles fine** and only
  fails when parsed at startup. `TestEveryPageRenders` is what catches it, along with a
  catalogue key or an unpacked `Msg` argument leaking into the output.

## Finding what was never finished

Two features turned out to be half-built, and both were found mechanically rather
than by remembering. Worth repeating occasionally:

- **A table with readers and no writers is a feature someone believes in.** List
  every table in the migrations and count which have an `INSERT`/`UPDATE` and
  which only a `SELECT`. That found `source_acl` (multi-user sharing was never
  wired up) and `sync_runs` (the sync history the plan asked for).
- **An exported function nothing calls is the same smell one layer up.** Four had
  outlived their callers.
- **Settings drift.** Compare `KOBIBRI_*` read in the code against the README
  table, both ways.

## Commands

```
make check                       # go vet ./... then go test ./...
make race                        # go test -race ./... -- NOT run by CI; run it by hand
make migrate                     # create or upgrade the database, then exit
make run DATA=./data BASE_URL=http://192.168.1.10:8078
make help                        # every target
```

The plain commands still work and are what CI runs:

```
go build ./... && go vet ./... && go test ./...
go test -race ./...
go run ./cmd/kobibri migrate
go run ./cmd/kobibri serve       # KOBIBRI_DATA_DIR, KOBIBRI_LISTEN, KOBIBRI_BASE_URL
```

The Makefile has to run under cmd.exe as well as a shell, so it sets environment with
make's target-specific `export` rather than a `VAR=value` prefix, and switches the one
recipe needing `rm` on `$(OS)`. Keep it that way.

A background server started in one shell invocation does not survive into the next one —
start it and exercise it in the same command.

Schema lives in `internal/store/migrations/`. An applied migration is never edited, only
followed by the next one: `<N>_<name>.sql`, densely numbered from 1.

## A trap worth remembering

Scripted, whitespace-sensitive substitutions on Go source are unreliable: `gofmt` realigns
struct fields and map keys, so an anchor that matched a moment ago silently stops matching
and the edit does nothing. It has cost a debugging session twice — once a func map missing
its entries, once a struct field that never got added. Edit Go declarations directly.

## Style

- Standard library first. A new dependency has to earn its place; the approved list is in
  `docs/ARCHITECTURE.md`.
- Comments in English, and about *why* — especially where the code looks strange because
  of a firmware quirk. Point those at the relevant section of `docs/kobo-protocol.md`.
- Every LANDMINE gets a test. Golden JSON for wire shapes, the fake device for scenarios.
