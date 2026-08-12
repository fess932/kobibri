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

## The web interface

- English is the source language; the catalogue in `internal/web/i18n.go` carries English
  and Russian, and a missing phrase falls back to English rather than showing its key.
- The server itself — logs, errors, the sync API — stays English regardless of the
  interface language.
- A template that calls a function missing from the func map **compiles fine** and only
  fails when parsed at startup. `TestEveryPageRenders` is what catches it, along with a
  catalogue key leaking into the output.

## Commands

```
go build ./... && go vet ./... && go test ./...
go test -race ./...
go run ./cmd/kobibri migrate     # create or upgrade the database, then exit
go run ./cmd/kobibri serve       # KOBIBRI_DATA_DIR, KOBIBRI_LISTEN, KOBIBRI_BASE_URL
```

A background server started in one shell invocation does not survive into the next one —
start it and exercise it in the same command.

Schema lives in `internal/store/migrations/`. An applied migration is never edited, only
followed by the next one: `<N>_<name>.sql`, densely numbered from 1.

## Style

- Standard library first. A new dependency has to earn its place; the approved list is in
  `docs/ARCHITECTURE.md`.
- Comments in English, and about *why* — especially where the code looks strange because
  of a firmware quirk. Point those at the relevant section of `docs/kobo-protocol.md`.
- Every LANDMINE gets a test. Golden JSON for wire shapes, the fake device for scenarios.
