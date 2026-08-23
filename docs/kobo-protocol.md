# Kobo Store Sync API — reverse-engineered reference

There is no official documentation. Everything here was verified against the source
of four independent implementations plus one client of the real store:

- calibre-web — `cps/kobo.py`, `cps/kobo_auth.py`, `cps/services/SyncToken.py`, `cps/kobo_sync_status.py`
- Calibre-Web-Automated — `cps/kobo.py`, `cps/kobo_cover_cache.py`
- Komga — `KoboController.kt`, `KoboProxy.kt`, `KoboHeaders.kt`, `KomgaSyncTokenGenerator.kt`, `KepubConverter.kt`, `KoboMissingPortFilter.kt`, `dto/*`
- kobink — `src/kobo/`
- kobo-book-downloader — `kobodl/kobo.py` (a client of the *real* store)

Sources are listed at the end. Sections marked **LANDMINE** describe behaviour that
fails silently or wedges the device.

---

## 1. Pointing a device at a custom server

### `Kobo eReader.conf`

On the device's USB mass-storage partition at `.kobo/Kobo/Kobo eReader.conf`, an INI file:

```ini
[OneStoreServices]
api_endpoint=https://storeapi.kobo.com
```

Replace that line with the server root, e.g. `https://books.example.com/kobo/<auth_token>`.
Plain HTTP works. Restart the device after editing.

### `/v1/initialization` — **LANDMINE**

The device GETs `<api_endpoint>/v1/initialization` and expects:

```json
{ "Resources": { "<key>": "<url or bool-as-string>", ... } }
```

**The device persists every key of `Resources` into `[OneStoreServices]` in
`Kobo eReader.conf`, permanently.** All later traffic uses those cached values, not
`api_endpoint`. A partial or wrong response wedges sync until the user hand-edits the
file. A truncated response (2,658 bytes instead of 11,728) has been observed to stop
a device from ever syncing.

The native resource map is ~200 keys. Keys that must be overridden to point at
your server:

| Key | Kobo default | Set to |
|---|---|---|
| `library_sync` | `https://storeapi.kobo.com/v1/library/sync` | yours |
| `library_metadata` | `.../v1/library/{Ids}/metadata` | yours |
| `reading_state` | `.../v1/library/{Ids}/state` | yours |
| `library_book` | `.../v1/user/library/books/{LibraryItemId}` | yours |
| `image_host` | `//cdn.kobo.com/book-images/` | the covers prefix, trailing slash and all |
| `image_url_template` | `https://cdn.kobo.com/book-images/{ImageId}/{Width}/{Height}/false/image.jpg` | yours |
| `image_url_quality_template` | `.../{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}/image.jpg` | yours |
| `tags`, `tag_items`, `delete_tag`, `rename_tag`, `delete_tag_items` | storeapi | yours |
| `delete_entitlement` | `.../v1/library/{Ids}` | yours |

calibre-web and Komga only override the three image keys plus `library_sync` and rely
on `api_endpoint` routing the rest; overriding the full set is more robust across
firmwares.

**Measured against a live calibre-web (2026-08-24).** `GET /kobo/<token>/v1/initialization`
returns **147 keys**, of which **exactly four** name calibre-web — `image_host`,
`image_url_template`, `image_url_quality_template`, `library_sync`. Everything else,
`library_metadata`, `reading_state`, `delete_entitlement` and the whole `tag` family
included, is left pointing at `storeapi.kobo.com` and only works because the device derives
it from `api_endpoint`. 145 values are strings and 2 are objects (`blackstone_header`,
`free_books_page`) — a `map[string]string` will not decode this map.

**Getting a baseline map (verified 2026-08-12).** `GET https://storeapi.kobo.com/v1/initialization`
answers **401** without device credentials, so a server cannot fetch the native map on its
own. kobibri therefore does not ship a vendored copy. Instead:

1. When proxying is on, the map is fetched from the store using the credentials the device
   sends on its own `/v1/initialization` request, then cached (`kv` key
   `kobo:upstream_resources`). A response with fewer than 100 keys is treated as truncated
   and discarded rather than cached, so a bad upstream answer cannot poison later devices.
2. Otherwise the response contains **only the keys we override**. The reasoning was that
   omitted keys are left at whatever the device already has — Kobo's own endpoints — so the
   device is never left half-configured, and that the grimmory #1500 wedge involved a
   *truncated* (malformed) response rather than a small valid one.

   **Confirmed on hardware (Kobo Libra Colour, fw 4.45.23697).** A dump of
   `[OneStoreServices]` after kobibri had answered with 13 keys shows ~70 keys, the rest of
   them Kobo's own — `store_host`, `oauth_host`, `product_reviews` and so on, untouched. The
   device merges; a small valid map does not wedge it. Point 1 being unreachable after first
   contact (see §2) therefore does not matter as much as it looks.

   Keys the response omits are also simply absent from the file — `library_sync`,
   `library_metadata`, `reading_state` and the `tag` family were not in that dump at all —
   and the device derives them from `api_endpoint` instead. That is why sync reaches a
   server that never sent them.

   **The device prefers `api_endpoint` even when the map says otherwise — measured
   2026-08-24.** Serving the full native map puts `product_prices` and `product_nextread` at
   `storeapi.kobo.com`, and the device asked *this* server for
   `/v1/products/{id}/prices` and `/v1/products/{id}/nextread` anyway. So the map does not
   redirect API traffic away from `api_endpoint`; it is consulted for hosts that sit outside
   it, which in practice means the image host. Overriding the keys this server serves is
   still worth doing — it costs nothing and does not depend on that behaviour holding across
   firmwares — but it is a belt, not the trousers.

**Where kobibri's base map comes from**, in order:

1. `<data>/kobo_resources.json`, if it holds at least 20 keys. Either JSON — bare or still
   in its `{"Resources": …}` envelope — or the `[OneStoreServices]` section copied straight
   out of a device's `Kobo eReader.conf`, which is parsed as INI. `api_endpoint` is dropped
   from either: it is the bootstrap, not a resource.
2. The store, asked once on a device's own initialization using that device's credentials —
   the only request that can ever succeed. Whatever it hands over is written to the file
   above and used from then on. A refusal is remembered for a day.
3. `nativeResources` in `internal/kobo/native_resources.go`: 144 keys captured from a real
   `/v1/initialization` response rather than typed out, and written out as a Go map so
   nothing is parsed at startup. The three hosts in it that no
   longer resolve — `social_host`, `social_authorization_host`, `discovery_host`, all
   `*.kobobooks.com`, checked 2026-08-24 — are removed, so the device keeps whatever it has
   for those rather than being handed a corpse.

The overrides go on top of whichever base won, always, so a stale map cannot point a device
away from this server for the keys that matter.

The floors differ on purpose. A map from the store is only believed at 100 keys or more,
because a short answer there is a truncated or error response. A map from the file is
believed at 20, because the best source of all is a dump of `[OneStoreServices]` off a
working device and one of those holds around seventy keys, not a hundred and fifty.

Prefer that dump over the embedded copy where you can: it is real, it matches that
firmware, and it is correct for that region.

`api_endpoint` is the JSON API root; `image_host`/`image_url_*` are a separate CDN-style
host. Changing only `api_endpoint` leaves covers pointed at `cdn.kobo.com`, which 404s
for your ImageIds. This is the one part of the map that cannot be derived: every other
key the device rebuilds from `api_endpoint` when it is absent, covers it cannot.

**`image_host` is a prefix, not a hostname — LANDMINE.** Kobo's own value is
`//cdn.kobo.com/book-images/`: scheme-relative, carrying a path, ending in a slash, and
the literal prefix of both templates. Setting it to a bare server root while the
templates point somewhere deeper names two different places in three keys, and a
firmware that builds a cover URL out of `image_host` rather than a template lands on the
one that is not served — without the auth token, at that. calibre-web has exactly that
split (`image_host` = its root, templates = `/kobo/<token>/…`) and kobibri copied it
until 2026-08-24. All three now read `<base>/kobo/<token>/covers/`, so the token travels
with a cover request whichever key the device reached for.

Also set the response header `x-kobo-apitoken: e30=` (base64 of `{}`).

**A cached map outlives the server that set it — LANDMINE.** The same dump still held
`image_host=http://192.168.0.200:8083` and `image_url_template=…/{ImageId}/{width}/{height}/false/image.jpg`
from a **calibre-web** instance the device had been pointed at months earlier — lowercase
placeholders and the literal `false`, calibre-web's own bug, preserved verbatim. `api_endpoint`
had been repointed at kobibri and every `/v1/initialization` since had carried the correct
`image_*` values, and the device kept the old ones regardless.

So the persisted map is not reliably refreshed by answering `/v1/initialization`. Moving a
device between servers leaves it with a mixture: the new API root, the old image host. Covers
then resolve against a machine that may not even exist any more, and nothing in either log
says so. The repair is to hand-edit `[OneStoreServices]`.

**Placeholder casing — LANDMINE.** Kobo's native templates use `{ImageId}`, `{Width}`,
`{Height}`, `{Quality}`, `{IsGreyscale}`. calibre-web emits lowercase `{width}`/`{height}`
and, as a genuine bug, the literal string `isGreyscale` instead of `{IsGreyscale}`; device
logs confirm it then requests that literal path. Use Kobo's exact capitalisation.

Read off a live calibre-web, 2026-08-24, both bugs in one line:

```
image_url_quality_template = http://…/kobo/<token>/{ImageId}/{width}/{height}/{Quality}/isGreyscale/image.jpg
```

The `false` in `image_url_template` where `{IsGreyscale}` might be expected is *not* a bug —
Kobo's own native template hardcodes it the same way.

### TLS and networking

- Plain HTTP works; HTTPS with a publicly trusted cert is recommended (the sync protocol
  carries sensitive information).
- Self-signed: install a DER cert at `.kobo/certificates/<name>.cer`
  (`openssl x509 -in cert.crt -outform DER -out …`) and restart. Flaky on some firmwares.
- **Kobo's TLS stack is old — keep TLS 1.2 enabled.** TLS-1.3-only proxy profiles are
  rejected at handshake.
- Some firmwares (4.38.x) fail hostname resolution where a raw IP works.
- Reverse proxies need enlarged buffers or sync 502s:
  `proxy_busy_buffers_size 1024k; proxy_buffers 4 512k; proxy_buffer_size 1024k;`
- **Devices send malformed/portless `Host` headers.** Komga ships a dedicated
  `KoboMissingPortFilter`; calibre-web has `config_external_port`. Repair `Host` before
  building any absolute URL.

---

## 2. Authentication

### What the real store does

On first sign-in the device is redirected to `https://auth.kobobooks.com/CrossDomainSignIn`
which serves a `kobo://UserAuthenticated?userId=…&userKey=…` deep link, inserting a
userKey into the device's User table. From calibre-web's research notes:

> Together, the device's DeviceId and UserKey act as an **irrevocable** authentication
> token to most (if not all) Kobo APIs. In fact, in most cases only the UserKey is
> required to authorize the API call. Changing Kobo password *does not* invalidate user keys!

- Most endpoints (sync, metadata, tags): userKey in the `x-kobo-userkey` header.
- Some (AnnotationService): Bearer token from `POST /v1/auth/device`.

  **Annotations are dialled directly and fail — and that is survivable.** The service
  lives on `reading_services_host` (`https://readingservices.kobo.com`), its own key in the
  resource map. The device goes there itself, so the request never passes through the
  server and cannot be answered or even logged, and the Bearer token it presents is the one
  *we* minted, which that host rejects exactly as storeapi does. A Kobo Libra Colour on
  fw 4.45.23697 reports it as a failure to load annotations.

  Overriding the key to point at the server was tried and reverted: the device does not
  refresh its stored map (see §1), so the override never reached it, and calibre-web — which
  mints an equally fake token and never touches this key — syncs regardless. The annotation
  failure is noise, not the cause of a failed sync.

`POST /v1/auth/device` request body (observed from a real store client):

```json
{ "AffiliateName": "Kobo", "AppVersion": "4.38.23171", "ClientKey": "<base64 of PlatformId>",
  "DeviceId": "<64 hex>", "PlatformId": "00000000-0000-0000-0000-000000000373",
  "SerialNumber": "<32 hex>", "UserKey": "<only on first activation>" }
```

Response (same shape for `/v1/auth/refresh`, which additionally takes `RefreshToken`):

```json
{ "AccessToken": "...", "RefreshToken": "...", "TokenType": "Bearer",
  "TrackingId": "<uuid>", "UserKey": "<echoed back>" }
```

### What emulators do

Ignore all of it and return random tokens. calibre-web:

```python
AccessToken  = base64.b64encode(os.urandom(24)).decode('utf-8')
RefreshToken = base64.b64encode(os.urandom(24)).decode('utf-8')
# "CalibreWeb doesn't make practical use of this auth/device API call for
#  authentication (nor for authorization). We return a dummy response."
```

Authorisation is instead an **opaque token in the URL path**:
`/kobo/<auth_token>/v1/...` (calibre-web uses `hexlify(urandom(16))`; Komga uses an API key).

### Device request headers (Kobo Clara 2E, fw 4.37/4.38)

```
Authorization: Bearer <token>
Accept: application/json
Accept-Encoding: gzip
User-Agent: Mozilla/5.0 (Linux; U; Android 2.0; en-us;) AppleWebKit/538.1 (KHTML, like Gecko)
            Version/4.0 Mobile Safari/538.1 (Kobo Touch 0373/4.38.23171)
x-kobo-affiliatename: Kobo
x-kobo-appversion: 4.37.21586
x-kobo-deviceid: <device_id>
x-kobo-devicemodel: Kobo Clara 2E
x-kobo-deviceos: 4.1.15
x-kobo-platformid: <platform_id>
x-kobo-synctoken: <sync_token>
```

**Not every request carries these — LANDMINE.** The headers above are what a *sync*
request looks like. The calls that open a session send far fewer, and in particular send
no `x-kobo-deviceid`: observed on firmware 4.45.23697, `/v1/auth/device`, `/v1/affiliate`
and `/v1/initialization` all arrive without it, and the id only starts appearing once the
device moves on to `/v1/user/profile` and the sync itself.

Nothing fails when this is missed, which is what makes it a landmine. Keying a device on
`(token, x-kobo-deviceid)` and taking the header at face value files those first requests
under an id of `""` — a different key — so every reader quietly appears twice in the
interface: once as itself, and once as a nameless row that has never synced and can never
be addressed. See `store.UpsertDevice`, which names the nameless row as soon as the id
turns up rather than opening a second one.

**The store's map is unobtainable after first contact — LANDMINE.** The plan of fetching
the real resource map from `storeapi.kobo.com` using the credentials the device sends is
sound exactly once. After we answer `/v1/auth/device` the device holds *our* access token,
sends it back in `Authorization`, and forwarding that upstream gets:

```json
{"ResponseStatus":{"ErrorCode":"ArgumentException","Message":"Invalid token version."}}
```

400, not 401 — observed on firmware 4.45.23697. The device also sends **no query string**
on `/v1/initialization`, so `PlatformID` and `SerialNumber` are not available to identify
it another way. There is no path back: the fetch will fail for as long as that device is
registered. kobibri remembers the refusal for a day rather than paying a few hundred
milliseconds of the device's time on every initialization to be told the same thing.

The consequence is that the fallback — sending only the keys we override — is not a rare
degraded mode but the normal one, and it has to be good enough on its own.

**The same token refusal reaches every other proxied endpoint.** `/v1/initialization` is
just where it was noticed first: `GET /v1/affiliate` got the identical 400 on every sync,
and relaying it to the device aborted the sync outright. That is what took proxying out
of kobibri altogether — §5, proxy strategy.

---

## 3. `GET /v1/library/sync`

### Request

```
GET <library_sync>?Filter=ALL&DownloadUrlFilter=Generic,Android&PrioritizeRecentReads=true
x-kobo-synctoken: <opaque, absent on first sync>
```

Every implementation surveyed ignores `Filter`, `DownloadUrlFilter` and
`PrioritizeRecentReads`. There is no `per_page`; pagination is server-driven.

### Response

- 200, `Content-Type: application/json; charset=utf-8` — **LANDMINE**: calibre-web
  deliberately bypasses Flask's `jsonify` because "jsonify decodes the Unicode string
  different to what kobo expects".
- Body is a **flat JSON array of single-key objects**. Always an array — `[]`, never `null`.

| Key | Payload |
|---|---|
| `NewEntitlement` | `{BookEntitlement, BookMetadata, ReadingState?}` |
| `ChangedEntitlement` | same container |
| `ChangedProductMetadata` | a bare `BookMetadata` |
| `NewTag` / `ChangedTag` | `{Tag: {...}}` |
| `DeletedTag` | `{Tag: {Id, LastModified}}` |
| `ChangedReadingState` | `{ReadingState: {...}}` |

**There is no `DeletedEntitlement`.** Removal is a `ChangedEntitlement` with
`BookEntitlement.IsRemoved = true` (see §4).

Real-store responses also contain `AudiobookEntitlement`, `AudiobookMetadata` and
`BookSubscriptionEntitlement` keys, which a self-hosted server can ignore.

### Example item

```json
{
  "NewEntitlement": {
    "BookEntitlement": {
      "Accessibility": "Full",
      "ActivePeriod": { "From": "2026-08-12T09:00:00Z" },
      "Created": "2024-01-02T10:11:12Z",
      "CrossRevisionId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "Id": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "IsRemoved": false,
      "IsHiddenFromArchive": false,
      "IsLocked": false,
      "LastModified": "2026-08-01T12:00:00Z",
      "OriginCategory": "Imported",
      "RevisionId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "Status": "Active"
    },
    "BookMetadata": {
      "Categories": ["00000000-0000-0000-0000-000000000001"],
      "Contributors": ["Jane Author"],
      "ContributorRoles": [{ "Name": "Jane Author" }],
      "CoverImageId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "CrossRevisionId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "CurrentDisplayPrice": { "CurrencyCode": "USD", "TotalAmount": 0 },
      "CurrentLoveDisplayPrice": { "TotalAmount": 0 },
      "Description": "<p>Blurb.</p>",
      "DownloadUrls": [
        { "Format": "KEPUB", "Size": 1234567,
          "Url": "https://library.example/kobo/<token>/download/<uuid>/KEPUB",
          "Platform": "Generic", "DrmType": "None" }
      ],
      "EntitlementId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "ExternalIds": [],
      "Genre": "00000000-0000-0000-0000-000000000001",
      "IsEligibleForKoboLove": false,
      "IsInternetArchive": false,
      "IsPreOrder": false,
      "IsSocialEnabled": true,
      "Language": "en",
      "PhoneticPronunciations": {},
      "PublicationDate": "2020-05-01T00:00:00Z",
      "Publisher": { "Imprint": "", "Name": "Some Press" },
      "RevisionId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a",
      "Series": { "Name": "The Series", "Number": 2.0, "NumberFloat": 2.0,
                  "Id": "<uuid3(NAMESPACE_DNS, seriesName)>" },
      "Title": "A Book",
      "WorkId": "6b6a2f9c-6a4d-4f3e-9a1b-1f2e3d4c5b6a"
    },
    "ReadingState": { "...": "see §5" }
  }
}
```

Field notes:

- Every UUID field (`Id`, `RevisionId`, `CrossRevisionId`, `EntitlementId`, `WorkId`,
  `CoverImageId`) is set to the *same* per-book UUID by every emulator. The device keys
  on `RevisionId`/`EntitlementId`.
- Timestamps: `%Y-%m-%dT%H:%M:%SZ`. The device is lenient (kobink sends raw epochs) but
  ISO-8601-Z is what the real store emits.
- `Categories`/`Genre`: the dummy `00000000-0000-0000-0000-000000000001`.
- `Accessibility`: `Full` (the store also uses `Preview` for samples).
  `Status`: `Active`. `OriginCategory`: `Imported`. `IsLocked`: always false.
- `Series.Number` is int-ish, `NumberFloat` a float, `Series.Id = uuid3(NAMESPACE_DNS, name)`.
- `Slug` is optional (store deep links only). `ContentKeys` is **not** part of sync — it
  belongs to `/v1/products/books/{id}/access` and only exists for DRM'd store books.
- `ISBN` and `SubTitle` are optional.
- The real store sometimes spells them `DRMType`/`DownloadUrl`; emit the canonical
  `DrmType`/`Url`.

### Continuation

```
x-kobo-synctoken: <always set; opaque; echoed back next request>
x-kobo-sync: continue        <- "I have more, call me again immediately"
x-kobo-sync-mode, x-kobo-recent-reads   <- pass-through when proxying only
```

Omit `x-kobo-sync` to signal completion. Batch sizes: calibre-web `SYNC_ITEM_LIMIT = 100`.

**Calibre's undefined date reaches the wire — LANDMINE.** Calibre writes
`UNDEFINED_DATE`, `datetime(101, 1, 1)`, into `pubdate` when a book has none, and most of a
library has none. Parsed as an ordinary timestamp it becomes
`"PublicationDate":"0101-01-01T00:00:00Z"` — a publication date in the year 101, inside a
sync response the device either parses or discards, with nothing said either way.

Observed on a real library: 60 of 63 books carried it. `calibre.parseTime` now returns the
zero time for any year at or below 101, and `MarshalJSON` renders that as `null`.

**Category order — LANDMINE.** From Komga's implementation, hard-won against real
hardware: drain in a fixed order — books added → books changed → books removed →
changed reading states → readlists added → changed → removed — and only start the next
category when the previous one is exhausted.

**ChangedEntitlement ignores ReadingState — LANDMINE.** Verbatim from Komga:

```kotlin
// changed books are also passed as changed reading state because Kobo does not process
// ChangedEntitlement even if it contains a ReadingState
```

So for a changed book emit `NewEntitlement` + `ChangedProductMetadata` +
a separate `ChangedReadingState`.

---

## 4. Removal

There is no `DeletedEntitlement`. To make a book disappear, re-send the entitlement with
`IsRemoved: true`. Komga sends a *stub* metadata object for removed books (all ids =
book id, `title = bookId`, no `DownloadUrls`) — real metadata is not needed to retract.

`IsRemoved: true` moves the item to the device's **Archive** (local file removed, tile
gone from My Books, still listed under Archived). `IsHiddenFromArchive: true` additionally
hides it from the Archive view; all emulators hard-code `false`. Setting `IsRemoved` back
to `false` and re-sending restores it.

### Device-initiated deletion

`DELETE /v1/library/{uuid}` → **204 No Content**. Sent when the user deletes a book on
the device.

### If the server forgets a book the device still has

**Nothing happens — the book stays on the device forever.** The protocol is a pure delta
stream; the device never reconciles. If the row disappears from the server before an
`IsRemoved: true` entitlement is emitted, the ability to retract it is permanently lost.

calibre-web states this as a known design gap in `SyncToken.py`:

```python
# This Schema doesn't contain enough information to detect and propagate book deletions
# from Calibre to the device. A potential solution might be to keep a list of all known
# book uuids in the token, and look for any missing from the db.
```

and in its wiki: *"Deleting a book from Calibre/Calibre-Web will not cause it to be
removed from the device. In order to trigger deletions, users must archive books and
then sync their devices."*

**Implication:** the server needs a persistent record of what it already sent, independent
of its book table. calibre-web uses `KoboSyncedBooks` + `ArchivedBook` side tables. Komga
uses immutable **SyncPoints** and diffs `from → to`, deleting the old sync point only once
the sync completes. kobibri copies the Komga model — see the sync engine in `docs/ARCHITECTURE.md`.

---

## 5. Other endpoints

calibre-web's route table, all under `/kobo/<auth_token>`:

| Route | Methods | Behaviour |
|---|---|---|
| `""` | GET | `{}` |
| `/v1/initialization` | GET | Resources + `x-kobo-apitoken: e30=` |
| `/v1/auth/device`, `/v1/auth/refresh` | POST | dummy Bearer tokens |
| `/v1/library/sync` | GET | the sync stream |
| `/v1/library/<uuid>/metadata` | GET | `[BookMetadata]` — **an array of one** |
| `/v1/library/<uuid>/state` | GET, PUT | reading state |
| `/v1/library/<uuid>` | DELETE | archive book, 204 |
| `/v1/library/tags` | POST | create collection → 201 + bare JSON string uuid |
| `/v1/library/tags` | DELETE | **405** — guard so it does not shadow book delete |
| `/v1/library/tags/<id>` | PUT, DELETE | rename / delete → 200 |
| `/v1/library/tags/<id>/items` | POST | add items → 201 |
| `/v1/library/tags/<id>/items/delete` | POST | remove items → 200 |
| `/<uuid>/<w>/<h>/<isGreyscale>/image.jpg` | GET | cover |
| `/<uuid>/<w>/<h>/<Quality>/<isGreyscale>/image.jpg` | GET | cover |
| `/download/<id>/<format>` | GET | file |
| `/v1/user/loyalty/benefits` | GET | `{"Benefits": {}}` |
| `/v1/analytics/gettests` | GET/POST | `{"Result":"Success","TestKey":<userkey>,"Tests":{}}` |
| `/v1/analytics/*`, `/v1/user/*`, `/v1/products/**`, `/v1/affiliate`, `/v1/deals`, `/v1/assets` | | proxy or `{}` |

### Tags / collections

Server → device:

```json
{ "Tag": { "Created": "2026-01-01T00:00:00Z", "Id": "<uuid>",
           "Items": [{ "RevisionId": "<book uuid>", "Type": "ProductRevisionTagItem" }],
           "LastModified": "2026-01-01T00:00:00Z", "Name": "My Shelf", "Type": "UserTag" } }
```

`DeletedTag` needs only `{"Tag": {"Id": …, "LastModified": …}}`.

Device → server: `POST /v1/library/tags` body `{"Name": "...", "Items": [...]}` → 201 with
the shelf UUID as a bare JSON string. `PUT /v1/library/tags/<id>` body `{"Name": "..."}`.
Item add/remove bodies are `{"Items": [{"RevisionId": …, "Type": "ProductRevisionTagItem"}]}`.

**`Items` is capitalised in Kobo's own map — LANDMINE.** The native `tag_items` value is
`https://storeapi.kobo.com/v1/library/tags/{TagId}/Items`, capital I, while its sibling
`delete_tag_items` is lowercase `…/items/delete`. Confirmed on a live calibre-web
2026-08-24, which does not override the key at all, so a device that derives the path from
`api_endpoint` sends the capitalised one. Go's `ServeMux` is case-sensitive: without an
alias that POST falls through to the unknown-endpoint handler, gets `200 {}`, and the book
silently never joins the shelf. kobibri registers both spellings on `handleAddTagItems`.
calibre-web and Komga register only the lowercase form.

### Reading state

`GET /v1/library/<uuid>/state` → an **array of one** ReadingState:

```json
{ "EntitlementId": "<uuid>", "Created": "...", "LastModified": "...", "PriorityTimestamp": "...",
  "StatusInfo": { "LastModified": "...", "Status": "Reading",
                  "TimesStartedReading": 1, "LastTimeStartedReading": "..." },
  "Statistics": { "LastModified": "...", "SpentReadingMinutes": 42, "RemainingTimeMinutes": 180 },
  "CurrentBookmark": { "LastModified": "...", "ProgressPercent": 37,
                       "ContentSourceProgressPercent": 61,
                       "Location": { "Value": "kobo.12.3", "Type": "KoboSpan",
                                     "Source": "OEBPS/chapter05.xhtml" } } }
```

`PUT` body `{"ReadingStates": [ {CurrentBookmark, Statistics, StatusInfo, LastModified} ]}` →

```json
{ "RequestResult": "Success",
  "UpdateResults": [{ "EntitlementId": "<uuid>",
                      "CurrentBookmarkResult": { "Result": "Success" },
                      "StatisticsResult": { "Result": "Success" },
                      "StatusInfoResult": { "Result": "Success" },
                      "LastModified": "...", "PriorityTimestamp": "..." }] }
```

`Result` ∈ {Success, Failure, Ignored}. `Status` ∈ {ReadyToRead, Reading, Finished}.
`ProgressPercent` and `ContentSourceProgressPercent` are 0–100.

**They are not two names for the same number — LANDMINE.** `ProgressPercent` is progress
through the **whole book**; `ContentSourceProgressPercent` is progress through the **current
spine file** only. Verified on a Kobo Libra Colour, fw 4.45.23697: a reader 19 pages into a
760-page book sent `ProgressPercent: 2` alongside `ContentSourceProgressPercent: 42`, being
42% of the way through one chapter file. The same reader on another book sent
`ProgressPercent: 70` with `ContentSourceProgressPercent: 0`, having just entered a new file.

Show `ProgressPercent`. Fall back to the other only when it is absent, and tell absent apart
from zero — a reader on page one sends `ProgressPercent: 0`, and treating that as unset puts
the chapter figure on screen. kobibri had this inverted in both `percentOf` and its test,
which is why it survived: the library showed 42% for a book barely opened.

Two device quirks — **LANDMINE**:

- `Location.Type: "KoboSpan"` only works for kepub. Plain EPUB gives chapter-level
  granularity only.
- *"If the book is finished, Kobo sends the first resource instead of the last, so we
  can't trust what Kobo sent"* — on `Finished`, override with the last position server-side.

### Proxy strategy

**kobibri proxies, but a refusal never leaves this host — LANDMINE, learned the hard way.**

An endpoint kobibri cannot answer out of its own library is offered to the store first and
**relayed**, every method, never redirected: a 307 sends the device to storeapi directly and
the answer never passes through here to be logged. Whatever comes back with a status under
400 is copied to the device verbatim. Anything from 400 up is logged with its body and
**discarded** — the device is then answered by kobibri, with a captured store shape where
there is one (`profile`, `wishlist`, `deals`, `affiliate`, `loyalty/benefits`,
`analytics/gettests`) and `200 {}` where there is not. `KOBIBRI_PROXY_UPSTREAM=off` keeps
every request here.

The reason for the split, rather than for proxying or not proxying at all:

1. **Some of it does work.** `profile`, `wishlist`, `deals` and `loyalty/benefits` come
   back 200 with the account's real data, and that is worth having over a stub.
2. **Two endpoints cannot.** `POST /v1/auth/device` is answered *here*, with a token
   invented on the spot — every implementation does this, calibre-web included. The device
   sends that invented token upstream in `Authorization`, and two endpoints check its
   version and refuse:
   `400 {"ErrorCode":"ArgumentException","Message":"Invalid token version."}`. Those two are
   `GET /v1/affiliate` and `GET /v1/initialization`, and they refuse for as long as the
   reader is registered here.

   **Only those two — measured 2026-08-24.** It is tempting to write this down as "the store
   rejects our token", and that is wrong. With the same invented token,
   `/v1/user/profile`, `/v1/user/wishlist`, `/v1/deals`, `/v1/user/loyalty/benefits`,
   `/v1/products/{id}/prices` and `/v1/products/{id}/nextread` all answer **200 with real
   data**. Most of the store works; two endpoints validate the token version and the rest do
   not look.
3. **The refusal is what is fatal, and only from this host.** Relaying that 400 to the
   device aborts the *whole* sync: a Kobo Libra Colour on fw 4.45.23697 posts
   `{"EventType":"FailedSync","Attributes":{"reason":"WebRequestErr"}}` to analytics and
   discards a `/v1/library/sync` response that arrived perfectly well a second later. An
   endpoint with nothing to do with books is enough to do it. Measured 2026-08-24: a
   calibre-web with proxying on answers the same endpoint with a 307, the device follows it
   and collects the identical 400 from storeapi — and syncs. A 4xx on `api_endpoint` is the
   sync server failing; the same 4xx from the store's own host is the shop being away.

Analytics is not offered upstream at all: `POST /v1/analytics/event` and
`GET /v1/analytics/gettests` are answered here. The events carry shelf sizes, reading
minutes, device storage and the serial number, and there is no reason to hand that to a shop
the account cannot even sign in to.

**Not forwarding is only half of it — LANDMINE.** `post_analytics_event` and
`get_tests_request` are keys in the resource map, and while they hold Kobo's own values the
reader posts its telemetry to `storeapi.kobo.com` directly: this server never sees the
request, let alone declines to forward it. A privacy decision taken in the proxy and not
also taken in `resourceOverrides` does nothing at all. Both keys are claimed.

calibre-web has no catch-all: a path outside its route table answers a plain Flask **404**
(`GET /kobo/<token>/v1/something/unknown`, measured 2026-08-24). The never-404 rule is
kobibri's own discipline and stricter than anything upstream enforces.

Kept for reference, this is what the others do:

```python
def redirect_or_proxy_request(auth=False):
    if config.config_kobo_proxy:
        if request.method == "GET":
            return redirect(get_store_url_for_current_request(), 307)
        ...
    return make_response(jsonify({}))
```

Komga answers GET with a 307 and really proxies everything else — *"The Kobo device turns
other request types into GET requests on redirects"*. calibre-web additionally proxies
`/v1/auth/device` when proxying is on, which is the one arrangement where the device holds
a **real** store token and the store answers it.

Their timeouts, if you ever put one back: calibre-web `(2, 10)`; Komga 1 minute.

**Whatever answers an unknown endpoint, it answers 200 — LANDMINE.** Never 404, never a
relayed 4xx. This is the rule the proxy broke, and the reason it is worth stating apart from
the removal: anything put back on that path — a cache, a partial proxy, a stub for a single
endpoint — inherits it.

---

## 6. calibre-web's SyncToken, and why not to copy it

```python
data_schema_v1 = { "type": "object", "properties": {
    "raw_kobo_store_token": {...}, "books_last_modified": {...}, "books_last_created": {...},
    "archive_last_modified": {...}, "reading_state_last_modified": {...},
    "tags_last_modified": {...} } }
```

Serialised as `base64(json)` with float epoch seconds; unsigned, unencrypted, opaque to
the device.

Token-shape detection worth copying:

```python
# On the first sync from a Kobo device, we may receive the SyncToken from the official
# Kobo store. That token is of the form [b64encoded blob].[b64encoded blob 2]
if "." in sync_token_header:
    return SyncToken(raw_kobo_store_token=sync_token_header)
```

Komga reads three variants: `base64.base64` (real store), a single base64 string
(calibre-web), and `KOMGA.`-prefixed base64 (its own).

### Known limitations (all real)

1. **Cannot express deletions** — the schema's own comment says so; two side tables paper over it.
2. **Timestamp high-water marks lose data.** Anything skipped (permission filter, format
   filter, error) whose `last_modified` is below the watermark is never re-sent. Clock skew,
   timezone-naive comparisons, sub-second truncation all bite.
3. **Resume is lossy.** During `continue`, `books_last_created` is frozen but
   `books_last_modified` advances, so an interrupted paged sync can permanently skip books.
4. **Not per-device** — state is `(user, book)`; two Kobos on one account interfere.
5. `archive_last_modified` is computed but never read — vestigial.
6. `datetime.utcfromtimestamp` raises on Windows for out-of-range values → silent full resync.
7. Zero rows in `KoboSyncedBooks` forcibly resets the token — a blunt instrument.
8. Large libraries time out on first sync.

---

## 7. kepub vs epub

kepub is **not required** — plain EPUB syncs and reads. What is lost: mid-chapter progress
(`Location.Type: "KoboSpan"` exists only in kepub) and Kobo's own renderer features.

`DownloadUrls` entry: `{Format, Size, Url, Platform, DrmType}`.
`Format` ∈ {`EPUB`, `EPUB3`, `EPUB3FL`, `KEPUB`}; `Platform` ∈ {`Generic`, `Android`};
`DrmType` `"None"` (store values: `KDRM`, `AdobeDrm`, `SocialDrm`, `None`).

- `EPUB3FL` = **fixed layout**. Komga: *"for fixed layout we always send EPUB3FL, so the
  Kobo can display in full screen; no conversion to Kepub is necessary, as there is already
  1 chapter per page, which is sufficient for progress tracking."*
- **PDF does not sync at all.**
- Offering both KEPUB and EPUB invites the device to pick EPUB and lose span-level progress.

### kepubify

kobibri converts EPUB to KEPUB itself (`internal/kepubconv/native.go`), following the rules
below exactly because span ids are where reading positions live. It was verified against
kepubify on fifty-seven real books before that library was dropped; see docs/PROGRESS.md.

[`pgaskin/kepubify`](https://github.com/pgaskin/kepubify) wraps text segments in
`<span class="koboSpan" id="kobo.N.M">` elements — which is precisely what makes
`Location.Type: "KoboSpan"` progress possible — plus Kobo's `div#book-columns`/`div#book-inner`
wrappers and CSS. It is a Go module; `github.com/pgaskin/kepubify/v4/kepub` can be imported
directly instead of shelling out. CLI form: `kepubify <in.epub> -o <out.kepub.epub>`.

**The `.kepub.epub` extension is load-bearing — LANDMINE.** Nickel only uses the KePub
renderer for files named `*.kepub.epub`, and kepubify only converts when the destination
has that extension. Komga forces both:

```kotlin
val destinationPath = dir.resolve(epub.nameWithoutExtension + ".kepub.epub")
contentDisposition = ContentDisposition.builder("attachment")
    .filename("${book.path.nameWithoutExtension}.kepub.epub", UTF_8).build()
```

Conversion timing: calibre-web converts eagerly (which is what makes its first sync slow);
Komga converts **lazily on download** with a 5-minute cache keyed on `id + fileLastModified`
and a 10s subprocess timeout. Lazy + cached is the better design.

`obok.py` (DeDRM) is unrelated to sync — it extracts KDRM keys from the Kobo desktop app's
local database.

---

## 8. Covers

Two templates, both in `Resources`, so the server must serve both a 5-segment and a
6-segment path:

```
image_url_template         = {base}/{ImageId}/{Width}/{Height}/{IsGreyscale}/image.jpg
image_url_quality_template = {base}/{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}/image.jpg
```

`ImageId` = `BookMetadata.CoverImageId`; every emulator sets it to the book UUID.

**The device caches covers by ImageId indefinitely — LANDMINE.** A changed cover is never
refetched. Calibre-Web-Automated's fix, worth copying:

```python
def build_cover_image_id(base_id, *, last_modified, cover_path):
    cover_mtime = int(os.path.getmtime(cover_path))
    return f"{base_id}-{cover_mtime}"

def normalize_cover_uuid(image_id):
    # "<uuid>-<digits>" -> "<uuid>"; valid UUIDs pass through unchanged
```

Serving:

- **Always return `image/jpeg`** — the path ends in `image.jpg` and the device expects JPEG.
- **Pre-scale and cache.** Serving full-resolution JPEGs visibly stalls library browsing
  (~5s for 5 covers). calibre-web buckets by requested height: `>1000` large, `>500` medium,
  else small.
- Kobo screens are 3:4, so `width = 0.75 × height`.
- `isGreyscale` is `true`/`false`; all implementations ignore it (the device dithers locally).
- Unknown ImageId → 307 to Kobo's CDN if proxying, else a placeholder with **200, not 404**
  (the device hammers failing cover URLs).
- The cover endpoint sits under the auth-token prefix and is authenticated by it.

---

## Sources

Code:
- https://raw.githubusercontent.com/janeczku/calibre-web/master/cps/kobo.py
- https://raw.githubusercontent.com/janeczku/calibre-web/master/cps/kobo_auth.py
- https://raw.githubusercontent.com/janeczku/calibre-web/master/cps/services/SyncToken.py
- https://raw.githubusercontent.com/janeczku/calibre-web/master/cps/kobo_sync_status.py
- https://raw.githubusercontent.com/crocodilestick/Calibre-Web-Automated/main/cps/kobo.py
- https://raw.githubusercontent.com/crocodilestick/Calibre-Web-Automated/main/cps/kobo_cover_cache.py
- https://github.com/gotson/komga — `interfaces/api/kobo/` and `infrastructure/kobo/`
- https://github.com/potatoeggy/kobink — `src/kobo/`
- https://github.com/subdavis/kobo-book-downloader — `kobodl/kobo.py`

Docs and issue threads:
- [calibre-web wiki: Kobo Integration](https://github.com/janeczku/calibre-web/wiki/Kobo-Integration)
- [Komga: Read with Kobo](https://komga.org/docs/guides/kobo/)
- [kepubify](https://github.com/pgaskin/kepubify)
- calibre-web #2867 (device header dump, sync query params), #2690 (OneStoreServices port
  appending), #1850 (restoring archived books), #1347 (cover image performance)
- [MobileRead: "Documentation for the Kobo Sync API?"](https://www.mobileread.com/forums/showthread.php?t=344656)
  — confirms no official docs exist

## Ratings and reviews — not part of sync

`ReadingState` carries status, bookmark and statistics. There is no rating in it, and no
sync category for one. Rating a book on a Kobo talks to the store separately, under
`/v1/products/…`, which kobibri does not implement — those calls fall through to the proxy,
or to `200 {}` when proxying is off.

So a rating given on the device is not stored here, and nothing kobibri knows could be
shown as one. What it does now is **write down everything that leaves the server**. Every request the
proxy handles is logged with its endpoint, the query, the upstream status and how long it
took, plus one extra line the first time an endpoint is seen at all. The token is never in
it, book ids are collapsed, and any query parameter that looks like a credential is
redacted. Rate a book on a real device and the line that appears says exactly which call to
implement.

That is deliberately where this stops. Guessing at a request shape for an undocumented API
is how you build something that silently does nothing.
