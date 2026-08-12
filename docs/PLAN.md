# kobibri — сервер синхронизации Calibre → Kobo

## Context

Нужен self-hosted сервер, который:
1. Забирает книги из **нескольких Calibre-библиотек** (источников), читая `metadata.db` (SQLite) и файлы напрямую с ФС, и сливает их в одну каноническую библиотеку.
2. Отдаёт эту библиотеку читалкам **Kobo**, эмулируя store sync API (у устройства подменяется `api_endpoint`).
3. Конвертирует EPUB → KEPUB, проксирует неизвестные эндпоинты в `storeapi.kobo.com`, имеет веб-UI.

**Главное требование к надёжности** (в формулировке пользователя): засинкали из двух источников, потом книга пропала на сервере, а на Kobo осталась — ничего не должно сломаться; «всегда синкаются все книги с сервера». Существующие реализации (calibre-web) это ломают: их sync-токен — набор timestamp-вотермарок, который принципиально не умеет выражать удаления и теряет книги при прерванном синке. Поэтому здесь синк строится **на реконсиляции снапшотов** (модель SyncPoint из Komga), а не на дельтах по времени.

Каталог `/Users/fess932/git/kobibri` пуст — проект с нуля. Go 1.26.4 установлен, `kepubify`/`calibre` в PATH нет.

### Решения пользователя (не пересматривать)

| Вопрос | Решение |
|---|---|
| Язык | Go |
| Источник | Прямое чтение Calibre `metadata.db` + файлы |
| Книга пропала на сервере | **Остаётся на Kobo.** Сервер не шлёт удаление |
| Удаление с Kobo | Инициируется устройством (`DELETE /v1/library/{uuid}`) → tombstone, книга больше не предлагается **этому устройству** |
| Tombstone scope | Per-device (Kobo #2 книгу сохраняет) |
| Формат | KEPUB (kepubify), лениво при скачивании |
| Прокси | Да, на `storeapi.kobo.com` |
| Пользователи | Несколько (семья/друзья), с админкой |
| Объём | Весь путь M1–M10 |

### Протокол Kobo — ground truth

Официальной документации нет; всё ниже выверено по исходникам calibre-web (`cps/kobo.py`, `kobo_auth.py`, `services/SyncToken.py`), Calibre-Web-Automated, Komga (`KoboController.kt`, `KoboProxy.kt`, DTO), kobink и kobo-book-downloader. Ключевые факты вынесены в §9 (мины) — их нарушение ломает устройство молча.

---

## 1. Раскладка проекта и зависимости

Модуль: `github.com/fess932/kobibri`, `go 1.26`.

```
cmd/kobibri/            main.go, commands.go       # serve | scan | migrate | token | admin
internal/
  config/               config.go                  # env + flags
  store/                db.go migrate.go models.go
                        books.go sources.go devices.go tags.go
                        syncpoint.go readingstate.go users.go
                        migrations/*.sql           # embed.FS
  calibre/              open.go read.go model.go fixture.go
  ingest/               scanner.go scheduler.go identity.go merge.go epubprobe.go
  kobo/                 types.go jsontime.go syncitem.go
                        router.go middleware.go auth.go
                        initialization.go initialization_resources.json
                        sync.go build.go library.go tags.go download.go covers.go proxy.go
  kepubconv/            conv.go lib.go exec.go
  covers/               pipeline.go
  httpx/                url.go mw.go json.go
  web/                  routes.go handlers.go session.go templates/*.gohtml static/
testdata/               minimal.epub fixedlayout.epub cover.jpg golden/*.json
```

Зависимости — минимум, всё остальное stdlib:

| Задача | Выбор | Почему |
|---|---|---|
| Роутер | **stdlib `net/http.ServeMux`** (Go 1.22 patterns) | Приоритет литералов над wildcard сам решает конфликт `DELETE /v1/library/tags` vs `/v1/library/{uuid}`. Ноль зависимостей |
| SQLite | **`modernc.org/sqlite`** | cgo-free → `CGO_ENABLED=0`, статический бинарь для NAS/RPi. Один драйвер и для нашей БД, и для `metadata.db`. Fallback при проблемах: `github.com/ncruces/go-sqlite3` |
| Миграции | своя ~60 строк на `PRAGMA user_version` + `embed.FS` | goose/golang-migrate тянут дерево ради тривиальной задачи |
| UUID | `github.com/google/uuid` | нужен v3 над `NAMESPACE_DNS` для `Series.Id` — должен совпадать бит-в-бит с другими реализациями |
| kepub | `github.com/pgaskin/kepubify/v4/kepub` как **библиотека** | см. §6.1 |
| Resize | `golang.org/x/image/draw` | CatmullRom, официальный, маленький |
| Конкурентность | `golang.org/x/sync` (`singleflight`, `semaphore`) | ровно под кэш конверсии |
| Пароли | `golang.org/x/crypto/bcrypt` | веб-UI |
| Логи | `log/slog` | stdlib |
| UI | `html/template` + `embed` + вендоренные htmx и pico.css | без node-тулчейна и без дублирования API |

Конфиг-файла нет: bootstrap из env (`KOBIBRI_DATA_DIR`, `KOBIBRI_LISTEN`, `KOBIBRI_BASE_URL`, `KOBIBRI_ADMIN_PASSWORD`), всё остальное — в БД и правится через UI.

---

## 2. Схема БД (`<datadir>/kobibri.db`)

Открытие:
```
file:<datadir>/kobibri.db?_txlock=immediate&_pragma=journal_mode(WAL)
  &_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)
```
Два `*sql.DB`: **writer** с `SetMaxOpenConns(1)` (убирает `SQLITE_BUSY` как класс) и **reader** пул. Время — `TEXT` RFC3339 UTC с `Z`.

> Точный синтаксис DSN-прагм у `modernc.org/sqlite` отличается от `mattn` — проверить в M1 до того, как DSN разъедется по коду.

### 2.1 Источники и сырые строки

```sql
CREATE TABLE sources (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  library_path TEXT NOT NULL,                    -- каталог с metadata.db
  priority INTEGER NOT NULL DEFAULT 100,         -- МЕНЬШЕ = ВЫШЕ
  enabled INTEGER NOT NULL DEFAULT 1,
  share_all INTEGER NOT NULL DEFAULT 1,          -- виден всем пользователям
  scan_interval_sec INTEGER NOT NULL DEFAULT 900,
  last_scan_at TEXT, last_ok_scan_at TEXT,
  last_status TEXT NOT NULL DEFAULT 'never',     -- ok|unreachable|error|suspicious|running
  last_error TEXT NOT NULL DEFAULT '',
  consecutive_fails INTEGER NOT NULL DEFAULT 0,
  book_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE source_acl (                        -- если share_all=0
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (source_id, user_id)
) WITHOUT ROWID;

CREATE TABLE source_books (
  id INTEGER PRIMARY KEY,
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  calibre_id INTEGER NOT NULL,
  calibre_uuid TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL, sort_title TEXT NOT NULL DEFAULT '',
  authors_json TEXT NOT NULL DEFAULT '[]', author_sort TEXT NOT NULL DEFAULT '',
  series_name TEXT NOT NULL DEFAULT '', series_index REAL,
  description_html TEXT NOT NULL DEFAULT '',
  publisher TEXT NOT NULL DEFAULT '', published_at TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '', isbn13 TEXT NOT NULL DEFAULT '',
  identifiers_json TEXT NOT NULL DEFAULT '{}', tags_json TEXT NOT NULL DEFAULT '[]',
  rel_path TEXT NOT NULL,                        -- calibre books.path
  cover_rel_path TEXT NOT NULL DEFAULT '',
  cover_mtime INTEGER NOT NULL DEFAULT 0,
  calibre_last_modified TEXT NOT NULL DEFAULT '',
  meta_hash TEXT NOT NULL,
  book_id TEXT REFERENCES books(id),
  missing INTEGER NOT NULL DEFAULT 0,            -- пропала в источнике; строка НИКОГДА не удаляется
  first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL,
  UNIQUE(source_id, calibre_id)
);
CREATE INDEX idx_source_books_book ON source_books(book_id);

CREATE TABLE source_book_files (
  id INTEGER PRIMARY KEY,
  source_book_id INTEGER NOT NULL REFERENCES source_books(id) ON DELETE CASCADE,
  format TEXT NOT NULL,                          -- EPUB|KEPUB|PDF|AZW3...
  rel_path TEXT NOT NULL, size INTEGER NOT NULL, file_mtime INTEGER NOT NULL,
  layout TEXT NOT NULL DEFAULT '',               -- ''|reflowable|pre-paginated
  epub_version TEXT NOT NULL DEFAULT '',
  probed_mtime INTEGER NOT NULL DEFAULT 0,
  present INTEGER NOT NULL DEFAULT 1,            -- файл реально есть на диске
  UNIQUE(source_book_id, format)
);
```

### 2.2 Канонические книги — слой стабильного UUID

```sql
CREATE TABLE books (
  id TEXT PRIMARY KEY,                           -- server UUIDv4. НИКОГДА не удаляется и не перевыпускается
  merged_into TEXT REFERENCES books(id),         -- алиас после слияния; продолжает резолвиться
  title TEXT NOT NULL DEFAULT '', sort_title TEXT NOT NULL DEFAULT '',
  authors_json TEXT NOT NULL DEFAULT '[]', author_sort TEXT NOT NULL DEFAULT '',
  series_name TEXT NOT NULL DEFAULT '', series_index REAL,
  series_uuid TEXT NOT NULL DEFAULT '',          -- uuid3(NAMESPACE_DNS, series_name)
  description_html TEXT NOT NULL DEFAULT '',
  publisher TEXT NOT NULL DEFAULT '', published_at TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT 'en', isbn13 TEXT NOT NULL DEFAULT '',
  primary_source_book_id INTEGER,                -- победитель по метаданным/файлу (NULL если всё пропало)
  cover_source_book_id INTEGER,
  cover_image_id TEXT NOT NULL DEFAULT '',       -- "<books.id>-<cover_mtime>" — cache-buster
  download_format TEXT NOT NULL DEFAULT '',      -- KEPUB|EPUB3FL|'' (не синкается)
  download_size INTEGER NOT NULL DEFAULT 0,
  available INTEGER NOT NULL DEFAULT 0,          -- есть хотя бы один живой source_book
  hidden INTEGER NOT NULL DEFAULT 0,             -- админский «не предлагать» → уедет в Архив устройства
  syncable INTEGER NOT NULL DEFAULT 0,           -- available && !hidden && download_format<>''
  serving_hash TEXT NOT NULL DEFAULT '',         -- sha256 ровно по полям, видимым на проводе
  metadata_rev INTEGER NOT NULL DEFAULT 1,       -- ++ только если serving_hash изменился
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  last_available_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_books_syncable ON books(syncable) WHERE merged_into IS NULL;

CREATE TABLE book_identities (                   -- много ключей → одна книга
  kind TEXT NOT NULL,                            -- calibre_uuid | isbn | titleauthor
  key  TEXT NOT NULL,
  book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  PRIMARY KEY (kind, key)
) WITHOUT ROWID;
```

### 2.3 Пользователи, токены, устройства, tombstones

```sql
CREATE TABLE users (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL, is_admin INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL
);
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL
);

CREATE TABLE api_tokens (                        -- непрозрачный токен в api_endpoint
  token_hash TEXT PRIMARY KEY,                   -- sha256 hex; сырой показывается один раз
  token_hint TEXT NOT NULL,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, last_used_at TEXT, revoked_at TEXT
);

CREATE TABLE devices (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL REFERENCES api_tokens(token_hash) ON DELETE CASCADE,
  kobo_device_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '', serial TEXT NOT NULL DEFAULT '',
  firmware TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '',
  first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL,
  last_sync_at TEXT, last_sync_status TEXT NOT NULL DEFAULT '',
  UNIQUE(token_hash, kobo_device_id)
);

-- удаления, инициированные устройством. PER-DEVICE. Постоянные, не чистятся автоматически.
CREATE TABLE device_tombstones (
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  book_id TEXT NOT NULL,                         -- канонический id ПОСЛЕ резолва алиасов
  created_at TEXT NOT NULL,
  PRIMARY KEY (device_id, book_id)
) WITHOUT ROWID;
```

### 2.4 Sync points — неизменяемые снапшоты

```sql
CREATE TABLE sync_points (
  id TEXT PRIMARY KEY,
  device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  parent_id TEXT,                                -- снапшот 'from', относительно которого diff
  state TEXT NOT NULL,                           -- ongoing | completed | abandoned
  cursor_cat INTEGER NOT NULL DEFAULT 0,
  cursor_key TEXT NOT NULL DEFAULT '',
  raw_kobo_token TEXT NOT NULL DEFAULT '',
  items_sent INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, completed_at TEXT
);
CREATE INDEX idx_sync_points_device ON sync_points(device_id, state);

CREATE TABLE sync_point_books (
  sync_point_id TEXT NOT NULL REFERENCES sync_points(id) ON DELETE CASCADE,
  book_id TEXT NOT NULL,
  metadata_rev INTEGER NOT NULL, reading_state_rev INTEGER NOT NULL,
  PRIMARY KEY (sync_point_id, book_id)
) WITHOUT ROWID;

CREATE TABLE sync_point_tags (
  sync_point_id TEXT NOT NULL REFERENCES sync_points(id) ON DELETE CASCADE,
  tag_id TEXT NOT NULL, tag_rev INTEGER NOT NULL,
  PRIMARY KEY (sync_point_id, tag_id)
) WITHOUT ROWID;
```

### 2.5 Прогресс чтения, коллекции, кэши, журналы

```sql
CREATE TABLE reading_states (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  book_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'ReadyToRead',    -- ReadyToRead|Reading|Finished
  bookmark_json TEXT NOT NULL DEFAULT 'null',
  statistics_json TEXT NOT NULL DEFAULT 'null',
  rev INTEGER NOT NULL DEFAULT 0,
  last_writer_device_id INTEGER,                 -- подавление эха
  last_modified TEXT NOT NULL, priority_ts TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, book_id)
) WITHOUT ROWID;

CREATE TABLE tags (
  id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, origin TEXT NOT NULL DEFAULT 'device',
  rev INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL, last_modified TEXT NOT NULL,
  deleted_at TEXT,                               -- soft delete: нужен для DeletedTag
  UNIQUE(user_id, name)
);
CREATE TABLE tag_books (
  tag_id TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  book_id TEXT NOT NULL, PRIMARY KEY(tag_id, book_id)
) WITHOUT ROWID;

CREATE TABLE kepub_cache (
  book_id TEXT NOT NULL, src_fp TEXT NOT NULL,   -- sha1(absPath|size|mtimeNs)[:16]
  path TEXT NOT NULL, size INTEGER NOT NULL,
  created_at TEXT NOT NULL, last_used_at TEXT NOT NULL,
  PRIMARY KEY (book_id, src_fp)
) WITHOUT ROWID;
CREATE TABLE kepub_failures (book_id TEXT NOT NULL, src_fp TEXT NOT NULL,
  err TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY(book_id, src_fp)) WITHOUT ROWID;
CREATE TABLE cover_cache (
  image_id TEXT NOT NULL, bucket TEXT NOT NULL,  -- small|medium|large
  path TEXT NOT NULL, width INTEGER, height INTEGER, size INTEGER,
  created_at TEXT NOT NULL, last_used_at TEXT NOT NULL,
  PRIMARY KEY (image_id, bucket)
) WITHOUT ROWID;

CREATE TABLE sync_runs (id INTEGER PRIMARY KEY, device_id INTEGER NOT NULL,
  sync_point_id TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT,
  requests INTEGER NOT NULL DEFAULT 0, new_books INTEGER, changed_books INTEGER,
  removed_books INTEGER, reading_states INTEGER, tags INTEGER,
  status TEXT NOT NULL DEFAULT 'running');
CREATE TABLE scan_runs (id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL,
  started_at TEXT NOT NULL, finished_at TEXT, status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '', seen INTEGER, added INTEGER, updated INTEGER,
  vanished INTEGER);
CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL);
```

**Инвариант:** ingest никогда не делает `DELETE FROM books` и `DELETE FROM source_books`. Только явное админское «purge source» удаляет `source_books`, и даже оно оставляет `books` нетронутыми.

---

## 3. Идентичность и слияние источников (`internal/ingest`)

```go
type IdentityKey struct{ Kind, Key string }
func Keys(sb *store.SourceBook) []IdentityKey            // identity.go, сильнейший первым
func (m *Merger) Attach(tx *store.Tx, sb *store.SourceBook) (bookID string, err error) // merge.go
```

Ключи по убыванию силы:
1. `calibre_uuid:<lower(books.uuid)>` — клоны/бэкапы одной библиотеки, доминирующий реальный кейс.
2. `isbn:<isbn13>` — из `identifiers` где `type='isbn'`; нормализация, ISBN-10→13 с пересчётом контрольной цифры, невалидные отбрасываем (мусорных ISBN в Calibre много).
3. `titleauthor:<normTitle>|<normAuthor>` — есть всегда, поэтому у книги минимум один ключ.

`norm(s)`: NFKD → снять комбинирующие → lower → `&`→`and` → выкинуть всё кроме `[a-z0-9 ]` → схлопнуть пробелы → снять ведущий артикль (`a|an|the|le|la|les|el|los|der|die|das|il|lo|de|het`). Для title дополнительно убрать хвостовые `(...)` и ` - <series> #n`. Автор — `norm(author_sort)` первого автора.

**Хеш содержимого файла для матчинга не используем** — это O(размер библиотеки) IO на каждый скан, и идентичность начинает зависеть от того, какой конвертер сделал EPUB. Вместо этого — админский отчёт «найти дубликаты» в UI (v2).

`Attach`:
- 0 совпадений → `INSERT INTO books(id=uuid.NewString())` + все ключи.
- 1 совпадение → прикрепить; `INSERT OR IGNORE` ключей, которых ещё не было (источник с ISBN обогащает индекс для следующих).
- \>1 → слияние. Выживший = **минимальный `created_at`, тай-брейк по лексикографически меньшему `id`** (детерминированно и не зависит от порядка скана; это тот UUID, который устройства скорее всего уже держат). У проигравших: перевесить `source_books`, `tag_books`, `book_identities`; свернуть `reading_states` (берём больший `rev`) и `device_tombstones` (`INSERT OR IGNORE`); проставить `merged_into=survivor`. **Строки проигравших остаются навсегда** — чтобы `/state`, download и `DELETE` по старому UUID с устройства продолжали резолвиться. `resolveBookID()` идёт по `merged_into` с защитой от циклов и глубиной ≤8.

Выбор победителя (`ingest.Resolve(bookID)`, перезапускается при изменении любого вкладчика):

```sql
SELECT sb.* FROM source_books sb JOIN sources s ON s.id = sb.source_id
WHERE sb.book_id = ? AND sb.missing = 0 AND s.enabled = 1
ORDER BY (EXISTS(SELECT 1 FROM source_book_files f
                 WHERE f.source_book_id=sb.id AND f.format='EPUB' AND f.present=1)) DESC,
         s.priority ASC, s.id ASC, sb.calibre_id ASC
LIMIT 1;
```
Победитель берётся целиком (без франкенштейн-метаданных), но пустые поля (`description_html`, `series_*`, `publisher`, `published_at`, `isbn13`) добираются из следующего по рангу. Обложка выбирается независимо тем же порядком среди строк с непустым `cover_rel_path`.

Дальше: `cover_image_id = books.id + "-" + cover_mtime`, `serving_hash = sha256(канонический JSON ровно тех полей, что уходят на провод)`, `metadata_rev++` **только если** `serving_hash` изменился (иначе каждый скан порождал бы шторм `ChangedProductMetadata`).

**Гарантия стабильности:** `books.id` выдаётся один раз, все ключи идентичности выводятся из содержимого. Удалить источник и добавить обратно → те же ключи → та же строка `books` → устройство не видит вообще никаких изменений.

---

## 4. Чтение Calibre (`internal/calibre`)

### 4.1 Открытие через snapshot-copy — всегда

Calibre держит `metadata.db` в WAL и может быть запущен. Открывать его read-only на месте враждебно: SQLite захочет создать `-shm`, что падает на read-only монтированиях и глючит по SMB/NFS.

`calibre.Open(libraryPath, workDir string) (*calibre.DB, error)`:
1. `stat(libraryPath+"/metadata.db")`; нет файла или EACCES → `ErrUnreachable` (отдельный тип ошибки, планировщик на него не трогает БД).
2. Скопировать `metadata.db`, `-wal`, `-shm` (ENOENT у последних двух игнорируем) в `<datadir>/tmp/scan-<id>-<rand>/`. WAL копировать **после** основного файла и проверить, что размер/mtime основного не изменились; до 3 ретраев.
3. Открыть **копию** read-write локально (чтобы SQLite проиграл WAL): `file:<tmp>/metadata.db?_txlock=deferred&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)`.
4. `PRAGMA quick_check`; при неудаче — одна повторная копия, потом `ErrCorrupt`.
5. `defer db.Close()`, `defer os.RemoveAll(tmpdir)`.

Стоимость ограничена (~50 МБ на 30k книг) и покупает иммунитет к живому Calibre, сетевым ФС, read-only монтированиям и мутациям посреди скана. **Реальную БД пользователя мы никогда не открываем на запись.** Быстрая предпроверка: если mtime+size `metadata.db` не изменились с прошлого скана — копию не делаем вовсе.

### 4.2 Запросы

Фаза A (каждый скан, дёшево, ловит изменения и пропажи):
```sql
SELECT id, uuid, last_modified FROM books ORDER BY id;
```
Сверка с `source_books` источника: изменившийся `last_modified` → changed; отсутствующий id → new; строка `source_books`, чей `calibre_id` не встретился → **vanished** → `missing=1` (не удаляем).

Фаза B (только для new+changed, батчами `IN (...)` по 500): `books`, `books_authors_link`+`authors`, `books_series_link`+`series`, `books_publishers_link`+`publishers`, `books_languages_link`+`languages` (с `item_order`), `books_tags_link`+`tags`, `comments`, `identifiers`, `data`. Джойн в Go через `map[int64][]…`.

> **Не использовать `group_concat` с внутренним `ORDER BY`** — порядок внутри агрегата гарантирован только с SQLite 3.44, а версия в modernc плавает.

`custom_columns` в v1 читаем только чтобы показать список доступных колонок в UI; маппинг кастомной колонки в `Genre`/автоколлекции — v2.

### 4.3 Файлы

- Книга: `filepath.Join(libraryPath, books.path, data.name+"."+lower(data.format))`, `os.Stat` → size/mtime/present. Если строка в `data` есть, а файла нет → `present=0` (Calibre регулярно врёт после ручных перемещений), лог один раз на путь.
- Обложка: `cover.jpg` в каталоге книги при `has_cover=1`; mtime идёт в cache-buster.
- Пути из Calibre через `/` → `filepath.FromSlash`. Любой путь, вылезающий за корень библиотеки (`filepath.Clean` + префикс-проверка), отвергается — защита от повреждённой/враждебной `metadata.db`.
- EPUB-проба (§6.4) выполняется здесь при `probed_mtime != file_mtime`.

### 4.4 Планировщик и защита от массовых пропаж

`ingest.Scheduler`: горутина на источник, тикер `scan_interval_sec` ±10% джиттера, канал `Trigger(sourceID)` для кнопки «Сканировать сейчас», глобальный семафор в 1 скан.

- `ErrUnreachable` → `last_status='unreachable'`, экспоненциальный бэкофф до 6 часов, **в БД не меняется ничего** (отвалившийся NAS не должен пометить 8000 книг пропавшими).
- **Suspicious-vanish guard**: если скан собирается пометить `missing` больше чем `max(20% книг источника, 25)` — транзакция откатывается, `last_status='suspicious'`, в UI требуется явное «Подтвердить». Даже подтверждение ничего не шлёт на устройства (удаление только device-initiated); guard защищает выбор победителя и счётчики.

---

## 5. Движок синхронизации — ядро

### 5.1 Желаемое множество

Для устройства *D* пользователя *U* в момент *t*:

```
Snapshot(D,t) = ( { b : b.merged_into IS NULL ∧ b.syncable ∧ visible(b,U) }
                ∪ Books(parent(D)) )       -- carry-forward, монотонность
                \ Tombstones(D)            -- удаления с этого устройства
```

Объединение с родителем — **это и есть механизм**, делающий требование пользователя истинным по построению:

- Книга пропала на сервере (источник отмонтирован, файл удалён, запись в Calibre снесена) → она всё ещё в `Books(parent)` → она в новом снапшоте → diff по ней даёт **ровно ноль элементов**. Ни `IsRemoved`, ни повторного добавления, ни ошибки. Устройство спокойно держит свой файл.
- Вернулась → `books.id` тот же (§3), `metadata_rev` скорее всего не менялся → снова ничего. Если файл изменился — обычное обновление метаданных.
- Tombstone вычитается **после** объединения → книга не может вернуться никогда, даже при полном ресинке от `parent = NULL`.

### 5.2 Материализация снапшота

`store.CreateSyncPoint(ctx, dev, userID, parentID, rawToken)` — одна пишущая транзакция:

```sql
INSERT INTO sync_points(id, device_id, parent_id, state, cursor_cat, cursor_key,
                        raw_kobo_token, created_at, updated_at)
VALUES (:new, :dev, :parent, 'ongoing', 0, '', :raw, :now, :now);

INSERT INTO sync_point_books(sync_point_id, book_id, metadata_rev, reading_state_rev)
SELECT :new, b.id, b.metadata_rev, COALESCE(rs.rev, 0)
FROM books b
LEFT JOIN reading_states rs ON rs.book_id = b.id AND rs.user_id = :uid
WHERE b.merged_into IS NULL
  AND (   ( b.syncable = 1
            AND EXISTS (SELECT 1 FROM source_books sb
                        JOIN sources s ON s.id = sb.source_id
                        LEFT JOIN source_acl a ON a.source_id = s.id AND a.user_id = :uid
                        WHERE sb.book_id = b.id AND sb.missing = 0 AND s.enabled = 1
                          AND (s.share_all = 1 OR a.user_id IS NOT NULL)) )
       OR b.id IN (SELECT book_id FROM sync_point_books WHERE sync_point_id = :parent) )
  AND b.id NOT IN (SELECT book_id FROM device_tombstones WHERE device_id = :dev);

INSERT INTO sync_point_tags(sync_point_id, tag_id, tag_rev)
SELECT :new, t.id, t.rev FROM tags t WHERE t.user_id = :uid AND t.deleted_at IS NULL;
```

Дальше снапшот **неизменяем**. Он не обновляется посреди синка — именно это делает возобновление по `x-kobo-sync: continue` нетеряющим, даже если параллельный скан переписывает `books`.

### 5.3 Diff — семь keyset-пагинированных запросов

```go
type syncCat int
const (
    catNewBooks syncCat = iota  // 0
    catChangedBooks             // 1
    catRemovedBooks             // 2
    catReadingStates            // 3
    catNewTags                  // 4
    catChangedTags              // 5
    catDeletedTags              // 6
    catDone                     // 7
)
```
Каждый запрос — `... AND <key> > :cursor ORDER BY <key> LIMIT :n` по двум неизменяемым снапшотам: детерминированно и возобновляемо. `:from` может быть `''` (полный синк) — `NOT EXISTS` вырождается корректно.

```sql
-- catNewBooks
SELECT t.book_id FROM sync_point_books t
WHERE t.sync_point_id = :to AND t.book_id > :cur
  AND NOT EXISTS (SELECT 1 FROM sync_point_books f
                  WHERE f.sync_point_id = :from AND f.book_id = t.book_id)
ORDER BY t.book_id LIMIT :n;

-- catChangedBooks
SELECT t.book_id FROM sync_point_books t
JOIN sync_point_books f ON f.sync_point_id = :from AND f.book_id = t.book_id
WHERE t.sync_point_id = :to AND t.book_id > :cur AND t.metadata_rev <> f.metadata_rev
ORDER BY t.book_id LIMIT :n;

-- catRemovedBooks
SELECT f.book_id FROM sync_point_books f
WHERE f.sync_point_id = :from AND f.book_id > :cur
  AND NOT EXISTS (SELECT 1 FROM sync_point_books t
                  WHERE t.sync_point_id = :to AND t.book_id = f.book_id)
ORDER BY f.book_id LIMIT :n;

-- catReadingStates: книга в обоих снапшотах, rev прогресса изменился, писало НЕ это устройство
SELECT t.book_id FROM sync_point_books t
JOIN sync_point_books f ON f.sync_point_id = :from AND f.book_id = t.book_id
LEFT JOIN reading_states rs ON rs.user_id = :uid AND rs.book_id = t.book_id
WHERE t.sync_point_id = :to AND t.book_id > :cur
  AND t.reading_state_rev <> f.reading_state_rev
  AND COALESCE(rs.last_writer_device_id, -1) <> :dev
ORDER BY t.book_id LIMIT :n;

-- catNewTags / catChangedTags / catDeletedTags — те же формы по sync_point_tags
```

Правила эмиссии (`kobo/sync.go`), с учётом задокументированных багов прошивки:

| Категория | Что уходит на провод, в этом порядке |
|---|---|
| New | `{"NewEntitlement": {BookEntitlement, BookMetadata, ReadingState}}` |
| Changed | `{"NewEntitlement": {...}}`, затем `{"ChangedProductMetadata": BookMetadata}`, затем `{"ChangedReadingState": {ReadingState}}` — **никогда** не полагаться на ReadingState внутри `ChangedEntitlement`, устройство его игнорирует |
| Removed | `{"ChangedEntitlement": {BookEntitlement с IsRemoved:true}}` — `DeletedEntitlement` не существует |
| ReadingState | `{"ChangedReadingState": {ReadingState}}` |
| New/ChangedTag | `{"NewTag": {Tag}}` / `{"ChangedTag": {Tag}}` |
| DeletedTag | `{"DeletedTag": {Tag}}` |

`catRemovedBooks` на практике срабатывает только (а) как безобидное эхо удаления, которое устройство само только что сделало, и (б) при админском `hidden=1` — это и есть предусмотренный способ отправить книгу в Архив устройства.

### 5.4 Машина состояний `continue`

```go
func (h *Handler) Sync(w http.ResponseWriter, r *http.Request) {
    dev := deviceFrom(r.Context())
    unlock := h.syncLocks.Lock(dev.ID)   // мьютекс на устройство, таймаут 30с → пустой [] + тот же токен
    defer unlock()

    tok := ParseSyncToken(r.Header.Get("x-kobo-synctoken"))
    sp := h.resolveSyncPoint(r.Context(), dev, tok)      // resume-or-create
    items, nextCat, nextKey, more := h.drain(r.Context(), sp, 100)
    if more { h.saveCursor(sp, nextCat, nextKey) } else { h.commit(sp) }

    w.Header().Set("x-kobo-synctoken", newToken.String())
    if more { w.Header().Set("x-kobo-sync", "continue") }
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    json.NewEncoder(w).Encode(items)   // ВСЕГДА массив; "[]" когда пусто, никогда null
}
```

`drain`: начать с `sp.cursor_cat`; выполнить запрос категории с `LIMIT batch+1`; выдать до `batch` книг; если вернулось `<= batch` строк и все выданы — `cursor_cat++`, `cursor_key=""` и перейти к следующей категории **внутри того же ответа**, пока не исчерпан бюджет (обычный «ничего не поменялось» синк — один пустой ответ, а не семь round-trip'ов). Никогда не переходить к категории *k+1*, пока в *k* остались строки. `catDone` ⇒ `more=false`.

Бюджет считается **в книгах (100)**, не в JSON-объектах (изменённая книга стоит трёх), плюс потолок ~4 МБ на тело ответа (описания бывают огромными) — что наступит раньше.

`resolveSyncPoint`:
- Токен пустой / нечитаемый / без префикса / указывает на чужое устройство ⇒ **полный синк**: `parent = lastCompleted(dev)` если есть, иначе `NULL`. Даже при `NULL` это безопасно — повторный `NewEntitlement` для книги, которая уже есть, идемпотентен (устройство ключуется по `Id`).
- В токене есть `Ongoing`, он `state='ongoing'` и принадлежит этому устройству ⇒ **resume** с сохранённого курсора.
- Только `Last` ⇒ новый sync point с `parent = Last`.
- Есть `ongoing`, но токен на него не ссылается (устройство перезагрузилось посреди синка) ⇒ пометить `abandoned`, создать новый от `lastCompleted`. **Родитель не удаляется, пока ребёнок не завершился** — ничего не теряется.

`commit` (одна транзакция):
```sql
UPDATE sync_points SET state='completed', completed_at=:now, cursor_cat=7 WHERE id=:to;
DELETE FROM sync_points WHERE id = :parent;   -- каскадом чистит sync_point_books/tags
UPDATE devices SET last_sync_at=:now, last_sync_status='ok' WHERE id=:dev;
UPDATE sync_runs SET finished_at=:now, status='ok' WHERE sync_point_id=:to;
```

### 5.5 Токен

```go
type SyncToken struct {
    V       int    `json:"v"`             // 1
    Ongoing string `json:"o,omitempty"`
    Last    string `json:"l,omitempty"`
    Raw     string `json:"r,omitempty"`   // токен настоящего стора, дословно
}
const tokenPrefix = "KOBIBRI."           // + base64.RawURLEncoding(json)
```
`ParseSyncToken`: снять префикс → base64 → JSON. Если префикса нет, а строка непустая — это токен настоящего Kobo-стора (форма `b64.b64` или просто непрозрачный) → вернуть `SyncToken{V:1, Raw:s}` и начать локально с нуля, сохранив `Raw` для проксируемых синков.

### 5.6 Уборка

Ежечасный janitor: `sync_points` в состоянии `ongoing`/`abandoned` старше 7 дней — удалить (никогда единственный `completed` у устройства). Осиротевшие `books` (нет `source_books`, не были ни в одном снапшоте, нет tombstone) — только по явному действию админа.

---

## 6. HTTP-слой

### 6.1 Таблица маршрутов

Префикс `/kobo/{token}/`, stdlib `ServeMux`:

```
POST   /kobo/{token}/v1/auth/device
POST   /kobo/{token}/v1/auth/refresh
GET    /kobo/{token}/v1/initialization
GET    /kobo/{token}/v1/library/sync
GET    /kobo/{token}/v1/library/{uuid}/metadata          -> массив из ОДНОГО BookMetadata
GET    /kobo/{token}/v1/library/{uuid}/state             -> массив из ОДНОГО ReadingState
PUT    /kobo/{token}/v1/library/{uuid}/state
DELETE /kobo/{token}/v1/library/{uuid}                   -> 204 + tombstone
POST   /kobo/{token}/v1/library/tags                     -> 201 + голая JSON-строка uuid
DELETE /kobo/{token}/v1/library/tags                     -> 405 (иначе затеняет удаление книги)
PUT    /kobo/{token}/v1/library/tags/{id}                -> 200
DELETE /kobo/{token}/v1/library/tags/{id}                -> 200
POST   /kobo/{token}/v1/library/tags/{id}/items          -> 201
POST   /kobo/{token}/v1/library/tags/{id}/items/delete   -> 200
GET    /kobo/{token}/v1/analytics/gettests               -> {"Result":"Success","TestKey":"","Tests":{}}
POST   /kobo/{token}/v1/analytics/event                  -> {"Result":"Success"}
GET    /kobo/{token}/download/{uuid}/{format}
GET    /kobo/{token}/covers/{imageId}/{w}/{h}/{greyscale}/image.jpg
GET    /kobo/{token}/covers/{imageId}/{w}/{h}/{quality}/{greyscale}/image.jpg
       /kobo/{token}/                                    -> catch-all: прокси, иначе 200 {}
```

Веб-UI — отдельный mux со своей аутентификацией: `/`, `/login`, `/sources`, `/library`, `/books/{id}`, `/devices`, `/users`, `/settings`, `/static/`.

### 6.2 Цепочка middleware

`recoverer → requestID → hostRepair → accessLog → koboHeaders → tokenAuth → deviceResolve`

- **recoverer**: паника ⇒ лог со стеком и `200 {}` (не 500) для путей `/kobo/`.
- **hostRepair** — аналог Komga `KoboMissingPortFilter`: Kobo шлёт `Host` без порта. `X-Forwarded-Host` при `trustProxy` выигрывает; иначе `net.SplitHostPort(r.Host)` и при ошибке дописать порт слушателя. Отдельно обработать хвостовое `:` и голые IPv6-литералы.
- **accessLog**: сегмент с токеном заменяется на хинт — токены не попадают в логи.
- **koboHeaders**: `x-kobo-apitoken: e30=` на все ответы под `/kobo/`.
- **tokenAuth**: `sha256(r.PathValue("token"))` → `api_tokens` (LRU-кэш 60с), отсечь revoked. Неудача: `200 {}` для API-путей (не ломать устройство), `404` для download/covers.
- **deviceResolve**: устройство = `(token_hash, x-kobo-deviceid)`, upsert модели/прошивки/UA. Без `x-kobo-deviceid` — запись с пустым id, привязанная к токену.

### 6.3 Построение абсолютных URL

```go
type URLBuilder struct{ Base *url.URL; ListenPort string; TrustProxy bool }
func (b URLBuilder) Abs(r *http.Request, elem ...string) string
```
`KOBIBRI_BASE_URL`, если задан, выигрывает безусловно (и это рекомендуемая конфигурация за любым reverse proxy). Иначе схема из `X-Forwarded-Proto`/`r.TLS`, хост — из уже починенного `r.Host`. **Все** URL в `DownloadUrls`, `Resources` и шаблонах обложек идут через эту одну функцию — должно быть ровно одно место, способное ошибиться.

### 6.4 `/v1/initialization` — самый опасный ответ в системе

Устройство **навсегда кэширует каждый ключ `Resources` в `Kobo eReader.conf`**. Частичный или неправильный ответ выводит синк из строя до ручной правки файла пользователем.

```go
res := h.baseResources(ctx)   // upstream, 24ч кэш в kv; при ЛЮБОЙ ошибке — встроенный JSON
h.overrideResources(res, r)   // полный набор URL-ключей
httpx.WriteJSON(w, 200, map[string]any{"Resources": res})
```
Переопределяем **весь** набор, а не подмножество: `library_sync`, `library_metadata`, `library_book`, `reading_state`, `image_host`, `image_url_template`, `image_url_quality_template`, `tags`, `tag_items`, `delete_tag`, `rename_tag`, `delete_tag_items`, `delete_entitlement`.

```
image_url_template         = {base}/kobo/{tok}/covers/{ImageId}/{Width}/{Height}/{IsGreyscale}/image.jpg
image_url_quality_template = {base}/kobo/{tok}/covers/{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}/image.jpg
image_host                 = {base}
```
Плейсхолдеры — **точно в этом регистре**. Не повторять баг calibre-web (`{width}`/`{height}` строчными и литеральное `isGreyscale` вместо `{IsGreyscale}`). Upstream-запрос с таймаутом 5с и правилом: если после слияния ключей меньше ~150 — выбросить результат и отдать встроенный baseline. Golden-тест на точный набор override'ов и на нижнюю границу числа ключей — обязателен.

### 6.5 Прокси и catch-all

1. Апстрим-путь: срезать `/kobo/{token}`, приклеить `https://storeapi.kobo.com`, сохранить `RawQuery`.
2. Прокси выключен или путь в denylist (`/v1/initialization` — вслепую не проксируется никогда) ⇒ `200 {}`.
3. `GET` ⇒ `http.Redirect(..., 307)`.
4. Не-GET ⇒ настоящее проксирование (устройство на редиректе понижает метод до GET). Наружу передаём только `Authorization`, `User-Agent`, `Accept`, `Accept-Language`, `Content-Type` и все `x-kobo-*` **кроме** `x-kobo-synctoken`. Срезать hop-by-hop (`Connection`, `Keep-Alive`, `Transfer-Encoding`, `Content-Encoding`, `Content-Length`, `Upgrade`, `Proxy-*`) и `Host`. Тело ≤8 МБ, таймаут 10с.
5. Любая ошибка транспорта/таймаут/DNS ⇒ **`200 {}`**, лог на debug. Никогда 502.

---

## 7. kepub и обложки

### 7.1 Библиотека vs subprocess — берём библиотеку с аварийным люком

Импортируем `github.com/pgaskin/kepubify/v4/kepub` напрямую: это Go-модуль с публичным пакетом (он для того и отделён от CLI). Выигрыш — никакой зависимости от PATH и упаковки (весь смысл единого статического бинаря), нет fork+exec на книгу, настоящая отмена по `context` вместо эвристического таймаута. Риск — слом API при обновлении.

```go
// internal/kepubconv
type Converter interface {
    Convert(ctx context.Context, srcPath, dstPath string) error
    Name() string
}
type libConverter struct{ /* lib.go — единственный файл, импортирующий kepubify */ }
type execConverter struct{ bin string }  // exec.go, KOBIBRI_KEPUBIFY_BIN
```
Первым делом в M6 — `go doc github.com/pgaskin/kepubify/v4/kepub`, точная сигнатура в этой сессии не проверялась (нет сети). Раз зависимость трогает ровно один файл, слом API — правка на десять строк, а `KOBIBRI_KEPUBIFY_BIN=/usr/bin/kepubify` — мгновенный обход в рантайме.

### 7.2 Ленивый кэш конверсии

```go
func (c *Cache) KepubPath(ctx context.Context, b *store.Book) (path string, size int64, err error)
```
- Ключ: `book_id + src_fp`, `src_fp = sha1hex(absPath|size|mtimeUnixNano)[:16]` — ловит и подмену файла с сохранённым mtime.
- Путь: `<datadir>/cache/kepub/<id[0:2]>/<id>.<src_fp>.kepub.epub`. **Суффикс `.kepub.epub` несущий** — он обязан пройти через путь кэша, аргумент назначения конвертера и `Content-Disposition`.
- Промах: `sem.Acquire` → конверсия во `<dst>.tmp-<rand>` → `Sync()` → `os.Rename` → строка в `kepub_cache`. `singleflight` по ключу: 20 одновременных запросов конвертируют один раз.
- Таймаут 120с, потолок входного файла 300 МБ (сверх — отдаём сырой EPUB как `EPUB3`).
- Ошибка конверсии ⇒ лог, строка в `kepub_failures`, отдаём оригинальный EPUB. Устройство, получившее 500 на скачивании, ретраит весь синк.
- Вытеснение: ежечасный janitor, LRU по `last_used_at` до `KOBIBRI_KEPUB_CACHE_BYTES` (по умолчанию 4 ГиБ), плюс снос строк с протухшим `src_fp`.

### 7.3 Отдача файла

- Резолв алиаса (`merged_into`); tombstone'нутая для этого устройства книга ⇒ 404.
- `KEPUB` → путь из кэша; `EPUB3FL`/`EPUB`/`EPUB3` → исходный файл.
- `Content-Type: application/epub+zip`; `Content-Disposition: attachment; filename="....kepub.epub"` (+ `filename*=UTF-8''…`); отдача через `http.ServeContent` — **range-запросы и докачка обязательны**, Kobo докачивает прерванное.
- Дедлайн записи на запрос через `http.ResponseController.SetWriteDeadline` при `WriteTimeout=0`, иначе 300 МБ по Wi-Fi не доедут.

`DownloadUrls` (`kobo/build.go`) — ровно **одна** запись:
```go
switch {
case f.layout == "pre-paginated": // Format:"EPUB3FL", БЕЗ конверсии
case f.format == "EPUB":          // Format:"KEPUB"
default:                          // syncable=0
}
// {Format, Size, Url, Platform:"Generic", DrmType:"None"}
```
Предлагать одновременно KEPUB и EPUB нельзя — устройство выберет EPUB и потеряет пословный прогресс. `Size` для KEPUB — размер исходного EPUB (устройство трактует его как справочный; `Content-Length` при скачивании всегда точный). Книги только в PDF/CBZ/MOBI/AZW3 получают `syncable=0` и в синк не попадают.

### 7.4 Детект fixed-layout (`ingest/epubprobe.go`)

`zip.OpenReader`, читаем два маленьких файла: `META-INF/container.xml` → OPF; потоковый `encoding/xml` до `</spine>`. Признаки pre-paginated: `<meta property="rendition:layout">pre-paginated</meta>`, легаси `<meta name="fixed-layout" content="true"/>`, `original-resolution` / `com.apple.ibooks.display-options`, либо ≥80% `<itemref>` с `rendition:layout-pre-paginated` в `properties`. Результат — в `source_book_files.layout` + `probed_mtime`, перепроба только при смене mtime. Гоняется во время скана, чтобы уже первый синк отдал верный `Format`.

### 7.5 Обложки

- Нормализация `imageId`: отрезать хвост `-<digits>`, проверить что префикс — валидный UUID, затем резолв алиаса.
- Бакеты по запрошенной высоте: `>1000 → large (900×1200)`, `>500 → medium (540×720)`, иначе `small (270×360)`. Экраны Kobo 3:4, вписывать с сохранением пропорций.
- Декод jpeg/png, ресайз `x/image/draw.CatmullRom`, JPEG q=85, **всегда `Content-Type: image/jpeg`**. `IsGreyscale` принимаем и игнорируем.
- Кэш `<datadir>/cache/covers/<bucket>/<aa>/<imageId>.jpg`, `ETag`, `Cache-Control: public, max-age=31536000, immutable`.
- Нет обложки / ошибка декода ⇒ встроенная заглушка, **200, а не 404** (устройство долбит падающие URL обложек).
- `CoverImageId` содержит mtime обложки, поэтому замена обложки даёт новый ImageId и пробивает вечный кэш устройства; хендлер срезает суффикс, поэтому старые ImageId продолжают работать.

---

## 8. Веб-UI (v1)

`html/template` + htmx, cookie-сессии (`HttpOnly`, `SameSite=Lax`, 32 байта случайности в таблице `sessions`), bcrypt, CSRF-токен на все мутирующие формы. Первый админ — из `KOBIBRI_ADMIN_PASSWORD` либо мастер первого запуска.

1. **Dashboard** — счётчики, последний скан/синк, предупреждения (недоступный источник, suspicious-скан, упавшие конверсии).
2. **Sources** — CRUD (имя, путь, приоритет, интервал, `share_all` + ACL), «Сканировать сейчас», живой статус через htmx-поллинг, подтверждение suspicious-скана.
3. **Library** — поиск/пагинация; карточка книги: слитые метаданные, все вкладчики `source_books` с пометкой кто победил и почему, форматы + layout + размер, превью обложки с её ImageId, состояние kepub-кэша + «Пересобрать», тумблер `hidden`, статус по каждому устройству (в снапшоте / tombstone / не отправлялась).
4. **Devices & tokens** — выпуск/отзыв токенов (сырой показывается один раз), модель/серийник/прошивка/последний синк, история `sync_runs`, список tombstone'ов с **«Забыть tombstone»** (отмена случайного удаления на читалке) и **«Сбросить состояние синка»** (снос sync points → следующий синк полный). Эти две кнопки — операционный аварийный люк на все случаи жизни.
5. **Users** — CRUD пользователей, флаг админа, привязка источников.
6. **Setup helper** — готовая строка `api_endpoint=` с абсолютным base URL и токеном, кнопка копирования и предупреждения: сначала сделать бэкап `Kobo eReader.conf`; при проблемах с резолвом имени использовать IP; TLS не ниже 1.2.
7. **Settings** — base URL, прокси вкл/выкл, бюджеты кэшей, размер батча, уровень логов.

Вне v1: OPDS, редактирование коллекций из веба, редактирование метаданных, автомаппинг тегов Calibre → коллекции Kobo.

---

## 9. Мины, которые нельзя задеть

**Протокол**
1. `/v1/initialization` кэшируется в `Kobo eReader.conf` навсегда. Частичный ответ = сломанный синк до ручной правки файла. Обязательны встроенный baseline, нижняя граница числа ключей и golden-тест.
2. Плейсхолдеры строго `{ImageId}/{Width}/{Height}/{Quality}/{IsGreyscale}`.
3. `DeletedEntitlement` не существует. Удаление = `ChangedEntitlement` + `IsRemoved: true`.
4. Устройство **игнорирует ReadingState внутри `ChangedEntitlement`** — всегда `NewEntitlement` + `ChangedProductMetadata` + отдельный `ChangedReadingState`.
5. `Content-Type: application/json; charset=utf-8` на синке, тело — всегда массив (`[]`, не `null`).
6. Порядок слива категорий фиксирован, чередовать нельзя.
7. `DELETE /v1/library/tags` обязан отдавать 405, иначе затеняет удаление книги.
8. `.kepub.epub` и в имени файла кэша, и в `Content-Disposition`, иначе пословный прогресс тихо деградирует.
9. Fixed-layout EPUB не кепубифицировать (`EPUB3FL`, как есть).
10. При `Status=Finished` устройство присылает **первый** ресурс вместо последнего — не сохранять его значение дословно, чинить на сервере.
11. Ошибка на любом второстепенном эндпоинте рушит весь синк ⇒ `200 {}` везде, включая паники и сбои прокси.
12. Проксирование GET — только 307; не-GET проксировать по-настоящему.
13. Полноразмерные обложки подвешивают устройство — всегда предмасштабировать.
14. `Host` без порта; старый TLS-стек (минимум TLS 1.2, HTTP/2 на слушателе отключить); часть прошивок не резолвит имена, но работает по IP; reverse proxy нужны увеличенные буферы (`proxy_buffers 4 512k`, `proxy_buffer_size 1024k`, `proxy_busy_buffers_size 1024k`).

**Дизайн**
15. Стабильность `books.id` — несущая. Любой путь кода, удаляющий строку `books` или перевыпускающий id, порождает дубликаты книг на всех устройствах. Защита: никаких `DELETE FROM books` вне явного админского purge + тест на стабильность id при удалении и повторном добавлении источника.
16. Ложные слияния по `titleauthor` (разные переводы, «Избранное»). Смягчение: это слабейший ключ; UI показывает вкладчиков; в v2 — ручное «разделить». Если на тестах ложные срабатывания пойдут — сделать `titleauthor` опциональным на пару источников.
17. `sync_point_books` растёт монотонно из-за carry-forward: 50k книг × 2 снапшота × 2 устройства ≈ 200k крошечных `WITHOUT ROWID` строк. Не проблема, задокументировать.
18. Синтаксис DSN-прагм `modernc.org/sqlite` отличается от `mattn`; проверить в M1. Snapshot-copy уже снимает жёсткую зависимость от read-only открытия.
19. API `kepubify` не проверен (не было сети) — M6 начинается с `go doc`, изоляция в одном файле держит радиус поражения.
20. Токен живёт в URL и в конфиге устройства открытым текстом навсегда. Хранить только хеш, вырезать из логов, выдавать токен на устройство, чтобы отзывать по одному.

---

## 10. Этапы

| M | Содержание | Как проверить |
|---|---|---|
| **M1** | Скелет: `go.mod`, `cmd/kobibri`, конфиг из env, `store.Open` + миграции + `0001_init.sql`, slog, graceful shutdown, `/healthz` | `kobibri migrate` создаёт БД, `PRAGMA user_version=1`, `go vet ./...` чист |
| **M2** | Чтение Calibre: snapshot-copy, фазы A/B, stat файлов и обложек, `kobibri scan --dry-run --path=…` | Прогон на сгенерированной фикстуре и вручную на реальной библиотеке |
| **M3** | Ingest: `source_books`, файлы, ключи идентичности, `Attach`, выбор победителя, `serving_hash`/`metadata_rev`, обработка пропаж + suspicious guard, планировщик | Две фикстуры с пересекающейся книгой дают одну строку `books`; удаление источника не меняет id; `metadata_rev` растёт только на видимых изменениях |
| **M4** | HTTP-скелет Kobo: роутер, middleware, tokenAuth, deviceResolve, hostRepair, `URLBuilder`, `/v1/auth/*`, `/v1/initialization`, прокси + catch-all | Golden-тест на initialization; реальное устройство перенаправляется и проходит (пустой) синк, не ломаясь |
| **M5** | Движок синка, только полный: типы, конверт `SyncItem`, `KoboTime`, create/diff/commit снапшотов, `NewEntitlement`, `/v1/library/{uuid}/metadata`. Батч временно огромный | Fake-device тест: первый синк отдаёт N книг, второй — `[]`; golden JSON на пару entitlement/metadata |
| **M6** | Скачивание, kepub, обложки: `kepubconv` (сначала `go doc`), кэш + singleflight + вытеснение, `epubprobe`, пайплайн обложек и оба маршрута | Реальное устройство скачивает и открывает книгу; на устройстве файл `.kepub.epub`; fixed-layout приходит без конверсии; обложки быстрые |
| **M7** | Пагинация, прогресс, удаление: машина `continue` с курсором, `GET/PUT /state` с починкой Finished, `DELETE /v1/library/{uuid}` → tombstone + 204 | 500 книг синкаются за 5 запросов; убийство сервера посреди синка ничего не теряет; удалённая книга не возвращается за 3 последующих синка, в т.ч. после «сброса состояния синка» |
| **M8** | Коллекции: пять tag-эндпоинтов + три категории diff'а, `DELETE /v1/library/tags` → 405 | Создать коллекцию на устройстве, добавить книги, переименовать, удалить; второе устройство получает её |
| **M9** | Веб-UI: все семь страниц, многопользовательский режим, ACL источников | Полная настройка с нуля только через UI; второй пользователь видит только свои источники |
| **M10** | Харднинг и упаковка: TLS 1.2 минимум, HTTP/2 off, таймауты, janitor'ы (kepub/обложки/sync points), Dockerfile, systemd-юнит, README с настройками reverse proxy и процедурой правки `Kobo eReader.conf` | Soak: 2 источника, 2 устройства, 5k книг, источник отмонтирован посреди прогона — ноль ошибок и ноль потерянных книг |

---

## 11. Проверка

1. **Golden JSON** — `testdata/golden/{new_entitlement,changed_metadata,reading_state,tag,initialization,auth_device}.json`, хелпер `assertGolden(t, name, got)` с флагом `-update`. Для синк-ответов проверяется **порядок** элементов (он протокольно значим).
2. **Фейковая библиотека Calibre** — `calibre.NewFixture(t, opts)` создаёт настоящий `metadata.db` с подмножеством оригинального DDL, дерево каталогов, крошечные валидные EPUB через `archive/zip` (reflowable, pre-paginated, EPUB2, битый zip) и `cover.jpg`. Опции: общие uuid между двумя фикстурами, пересечение только по ISBN, только по title+author, грязный WAL, строка в `data` без файла.
3. **Фейковое устройство** (`kobo/device_test.go`) — гоняет реальный диалог по `httptest.Server`: `auth/device` → `initialization` (проверить каждый override и что ответ записывается в map-конфиг) → цикл `library/sync` пока `x-kobo-sync: continue` с переносом токена → скачивание каждого URL (`Content-Disposition` кончается на `.kepub.epub`, байты — валидный zip с koboSpan-разметкой) → обложки → `PUT state` → `DELETE` одной книги → снова синк. Держит модель «библиотеки устройства» и проверяет инварианты:
   - каждая syncable-книга присутствует ровно один раз;
   - удалённая книга не возвращается никогда;
   - **после отключения/удаления источника следующий синк — `[]`, а библиотека устройства не изменилась** (главное требование, оформленное как ассерт);
   - после правки метаданных — ровно один `NewEntitlement` + один `ChangedProductMetadata` + один `ChangedReadingState`;
   - синк, прерванный в каждой возможной точке курсора (таблично по категориям × смещениям), всё равно сходится.
4. **Property-тест** — случайные последовательности {скан, правка книги, добавление/удаление источника, синк, удаление с устройства, `PUT state`, убийство посреди синка} против модели ожидаемого состояния устройства; проверка сходимости после финального чистого синка. Самый ценный тест в проекте.
5. **Конкурентность** — `-race`: скан переписывает `books`, пока пагинированный синк сливается; эмитированное множество обязано точно равняться снапшоту (доказательство неизменяемости).
6. **Реальное железо** (`docs/hardware-testing.md`): бэкап `Kobo eReader.conf`; смена `api_endpoint`; синк по plain HTTP и по TLS 1.2; `Host` без порта (подключение по IP и на нестандартном порту); прогресс чтения round-trip на уровне span (`CurrentBookmark.Location.Type == "KoboSpan"`); поведение Архива при удалении на устройстве; свежее устройство после factory reset.
