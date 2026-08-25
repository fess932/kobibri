---
name: kobibri
description: Working on kobibri, a Calibre → Kobo sync server. Use for ANY task in this repository - before writing code, when behaviour changes, and when something new is learned about the Kobo protocol. Triggers - kobo, calibre, sync, entitlement, kepub, metadata.db, sync point, tombstone, reading state.
---

# kobibri

A Calibre → Kobo sync server. **A sync is reconciliation between immutable snapshots, not a
delta over timestamps.**

`docs/NOTES.md` is the only document: design, protocol, invariants, open items. Read the
relevant part before writing code — the LANDMINE list in it describes behaviour that breaks a
device silently rather than failing. Do not reconstruct it from memory.

## Writing to NOTES.md

Only when something is **true now and not derivable from the code**: a protocol fact measured
on hardware, a decision whose reasoning would otherwise be lost, an invariant, a considered no.

- A landmine → the LANDMINE list, one bullet, with what was measured and on what.
- A behaviour change contradicting the file → fix that line in place. Do not append a
  correction beside a wrong statement.
- Anything else worth keeping → one or two lines at the end of `## Log`.

Not a journal. No milestone entries, no "what it cost", no narrating a change git already
shows. If it takes a paragraph, it is probably not worth writing. Keep the file's tone: short
sentences, the non-obvious why, no preamble.

## Rules

- **Do not write comments in Go** (owner's instruction, 2026-08-23). Name things so the code
  says it; the why goes in `docs/NOTES.md`. Exception: a line that looks wrong on purpose
  because of a firmware quirk gets one line pointing at the landmine.
- Standard library first. A new dependency has to earn its place.
- Every landmine gets a test. Golden JSON for wire shapes, the fake device for scenarios.
- An applied migration is never edited, only followed by `<N+1>_<name>.sql` in
  `internal/store/migrations/`.
- Scripted whitespace-sensitive edits on Go source silently fail — `gofmt` realigns struct
  fields and map keys, so the anchor stops matching. Edit declarations directly.
- Adding a route means adding it to `internal/kobo/openapi.json`, or the coverage test fails.
- A background server started in one shell invocation does not survive into the next.

## Commands

```
make check    # go vet ./... && go test ./...
make lint     # golangci-lint, stock set, no config file; keep it at zero. Not in CI
make race     # not in CI; run after touching the scanner, scheduler or sync engine
make migrate
make run DATA=./data BASE_URL=http://192.168.1.10:8078
```
