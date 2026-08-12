-- Users, sessions and the opaque tokens that appear in a device's api_endpoint.

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  is_admin      INTEGER NOT NULL DEFAULT 0,
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL
);

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
  token_hash   TEXT PRIMARY KEY,             -- sha256 hex; the raw token is shown once
  token_hint   TEXT NOT NULL,                -- first chars, for the UI
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  label        TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  last_used_at TEXT,
  revoked_at   TEXT
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

-- Sources: Calibre libraries read directly off the filesystem.

CREATE TABLE sources (
  id                INTEGER PRIMARY KEY,
  name              TEXT NOT NULL UNIQUE,
  library_path      TEXT NOT NULL,                  -- directory containing metadata.db
  priority          INTEGER NOT NULL DEFAULT 100,   -- LOWER WINS
  enabled           INTEGER NOT NULL DEFAULT 1,
  share_all         INTEGER NOT NULL DEFAULT 1,     -- visible to every user
  scan_interval_sec INTEGER NOT NULL DEFAULT 900,
  last_scan_at      TEXT,
  last_ok_scan_at   TEXT,
  last_status       TEXT NOT NULL DEFAULT 'never',  -- never|running|ok|unreachable|error|suspicious
  last_error        TEXT NOT NULL DEFAULT '',
  consecutive_fails INTEGER NOT NULL DEFAULT 0,
  book_count        INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL
);

CREATE TABLE source_acl (                           -- consulted only when share_all = 0
  source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (source_id, user_id)
) WITHOUT ROWID;

-- Canonical books. books.id is the only identifier a device ever sees, so it is
-- issued once and never deleted or reissued. A book that vanishes from every
-- source keeps its row (available = 0); a merged-away book keeps its row too and
-- stays resolvable through merged_into.

CREATE TABLE books (
  id                     TEXT PRIMARY KEY,
  merged_into            TEXT REFERENCES books(id),
  title                  TEXT NOT NULL DEFAULT '',
  sort_title             TEXT NOT NULL DEFAULT '',
  authors_json           TEXT NOT NULL DEFAULT '[]',
  author_sort            TEXT NOT NULL DEFAULT '',
  series_name            TEXT NOT NULL DEFAULT '',
  series_index           REAL,
  series_uuid            TEXT NOT NULL DEFAULT '',   -- uuid3(NAMESPACE_DNS, series_name)
  description_html       TEXT NOT NULL DEFAULT '',
  publisher              TEXT NOT NULL DEFAULT '',
  published_at           TEXT NOT NULL DEFAULT '',
  language               TEXT NOT NULL DEFAULT 'en',
  isbn13                 TEXT NOT NULL DEFAULT '',
  primary_source_book_id INTEGER,                    -- metadata + file winner
  cover_source_book_id   INTEGER,
  cover_image_id         TEXT NOT NULL DEFAULT '',   -- "<books.id>-<cover_mtime>" cache buster
  download_format        TEXT NOT NULL DEFAULT '',   -- KEPUB | EPUB3FL | '' (not servable)
  download_size          INTEGER NOT NULL DEFAULT 0,
  available              INTEGER NOT NULL DEFAULT 0, -- at least one live source_book
  hidden                 INTEGER NOT NULL DEFAULT 0, -- admin "do not offer" -> device Archive
  syncable               INTEGER NOT NULL DEFAULT 0, -- available AND NOT hidden AND download_format <> ''
  serving_hash           TEXT NOT NULL DEFAULT '',   -- sha256 over exactly the wire-visible fields
  metadata_rev           INTEGER NOT NULL DEFAULT 1, -- bumped only when serving_hash changes
  created_at             TEXT NOT NULL,
  updated_at             TEXT NOT NULL,
  last_available_at      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_books_syncable ON books(syncable) WHERE merged_into IS NULL;
CREATE INDEX idx_books_merged_into ON books(merged_into) WHERE merged_into IS NOT NULL;

CREATE TABLE book_identities (
  kind    TEXT NOT NULL,        -- calibre_uuid | isbn | titleauthor
  key     TEXT NOT NULL,
  book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  PRIMARY KEY (kind, key)
) WITHOUT ROWID;
CREATE INDEX idx_book_identities_book ON book_identities(book_id);

-- Raw per-source rows. Never deleted by ingest; a book that disappears from the
-- library is flagged missing so that history and canonical ids survive.

CREATE TABLE source_books (
  id                    INTEGER PRIMARY KEY,
  source_id             INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
  calibre_id            INTEGER NOT NULL,
  calibre_uuid          TEXT NOT NULL DEFAULT '',
  title                 TEXT NOT NULL,
  sort_title            TEXT NOT NULL DEFAULT '',
  authors_json          TEXT NOT NULL DEFAULT '[]',
  author_sort           TEXT NOT NULL DEFAULT '',
  series_name           TEXT NOT NULL DEFAULT '',
  series_index          REAL,
  description_html      TEXT NOT NULL DEFAULT '',
  publisher             TEXT NOT NULL DEFAULT '',
  published_at          TEXT NOT NULL DEFAULT '',
  language              TEXT NOT NULL DEFAULT '',
  isbn13                TEXT NOT NULL DEFAULT '',
  identifiers_json      TEXT NOT NULL DEFAULT '{}',
  tags_json             TEXT NOT NULL DEFAULT '[]',
  rel_path              TEXT NOT NULL,               -- calibre books.path
  cover_rel_path        TEXT NOT NULL DEFAULT '',
  cover_mtime           INTEGER NOT NULL DEFAULT 0,
  calibre_last_modified TEXT NOT NULL DEFAULT '',
  meta_hash             TEXT NOT NULL,
  book_id               TEXT REFERENCES books(id),
  missing               INTEGER NOT NULL DEFAULT 0,
  first_seen_at         TEXT NOT NULL,
  last_seen_at          TEXT NOT NULL,
  UNIQUE(source_id, calibre_id)
);
CREATE INDEX idx_source_books_book ON source_books(book_id);

CREATE TABLE source_book_files (
  id             INTEGER PRIMARY KEY,
  source_book_id INTEGER NOT NULL REFERENCES source_books(id) ON DELETE CASCADE,
  format         TEXT NOT NULL,                -- EPUB | KEPUB | PDF | AZW3 | ...
  rel_path       TEXT NOT NULL,
  size           INTEGER NOT NULL,
  file_mtime     INTEGER NOT NULL,
  layout         TEXT NOT NULL DEFAULT '',     -- '' | reflowable | pre-paginated
  epub_version   TEXT NOT NULL DEFAULT '',
  probed_mtime   INTEGER NOT NULL DEFAULT 0,
  present        INTEGER NOT NULL DEFAULT 1,   -- the file actually exists on disk
  UNIQUE(source_book_id, format)
);

-- Devices and the per-device tombstones that record on-device deletions.
-- Tombstones are permanent and scoped to one device: deleting a book on Kobo #1
-- must not remove it from Kobo #2.

CREATE TABLE devices (
  id               INTEGER PRIMARY KEY,
  user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash       TEXT NOT NULL REFERENCES api_tokens(token_hash) ON DELETE CASCADE,
  kobo_device_id   TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  serial           TEXT NOT NULL DEFAULT '',
  firmware         TEXT NOT NULL DEFAULT '',
  user_agent       TEXT NOT NULL DEFAULT '',
  first_seen_at    TEXT NOT NULL,
  last_seen_at     TEXT NOT NULL,
  last_sync_at     TEXT,
  last_sync_status TEXT NOT NULL DEFAULT '',
  UNIQUE(token_hash, kobo_device_id)
);
CREATE INDEX idx_devices_user ON devices(user_id);

CREATE TABLE device_tombstones (
  device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  book_id    TEXT NOT NULL,                    -- canonical id AFTER alias resolution
  created_at TEXT NOT NULL,
  PRIMARY KEY (device_id, book_id)
) WITHOUT ROWID;

-- Sync points: immutable snapshots of what a device should hold. A sync is a
-- diff between two snapshots, which is what makes removals expressible and
-- interrupted syncs resumable without losing books.

CREATE TABLE sync_points (
  id             TEXT PRIMARY KEY,
  device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  parent_id      TEXT,                         -- the 'from' snapshot; NULL means a full sync
  state          TEXT NOT NULL,                -- ongoing | completed | abandoned
  cursor_cat     INTEGER NOT NULL DEFAULT 0,
  cursor_key     TEXT NOT NULL DEFAULT '',
  raw_kobo_token TEXT NOT NULL DEFAULT '',     -- the real store's token, verbatim
  items_sent     INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  completed_at   TEXT
);
CREATE INDEX idx_sync_points_device ON sync_points(device_id, state);

CREATE TABLE sync_point_books (
  sync_point_id     TEXT NOT NULL REFERENCES sync_points(id) ON DELETE CASCADE,
  book_id           TEXT NOT NULL,
  metadata_rev      INTEGER NOT NULL,
  reading_state_rev INTEGER NOT NULL,
  PRIMARY KEY (sync_point_id, book_id)
) WITHOUT ROWID;

CREATE TABLE sync_point_tags (
  sync_point_id TEXT NOT NULL REFERENCES sync_points(id) ON DELETE CASCADE,
  tag_id        TEXT NOT NULL,
  tag_rev       INTEGER NOT NULL,
  PRIMARY KEY (sync_point_id, tag_id)
) WITHOUT ROWID;

-- Reading progress and collections.

CREATE TABLE reading_states (
  user_id               INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  book_id               TEXT NOT NULL,
  status                TEXT NOT NULL DEFAULT 'ReadyToRead',  -- ReadyToRead|Reading|Finished
  bookmark_json         TEXT NOT NULL DEFAULT 'null',
  statistics_json       TEXT NOT NULL DEFAULT 'null',
  rev                   INTEGER NOT NULL DEFAULT 0,
  last_writer_device_id INTEGER,                               -- echo suppression
  last_modified         TEXT NOT NULL,
  priority_ts           TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, book_id)
) WITHOUT ROWID;

CREATE TABLE tags (
  id            TEXT PRIMARY KEY,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  origin        TEXT NOT NULL DEFAULT 'device',   -- device | calibre | server
  rev           INTEGER NOT NULL DEFAULT 1,       -- bumped on rename or membership change
  created_at    TEXT NOT NULL,
  last_modified TEXT NOT NULL,
  deleted_at    TEXT,                             -- soft delete: needed to emit DeletedTag
  UNIQUE(user_id, name)
);

CREATE TABLE tag_books (
  tag_id  TEXT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  book_id TEXT NOT NULL,
  PRIMARY KEY (tag_id, book_id)
) WITHOUT ROWID;

-- Derived-artefact caches. Rows point at files under <datadir>/cache.

CREATE TABLE kepub_cache (
  book_id      TEXT NOT NULL,
  src_fp       TEXT NOT NULL,          -- sha1(absPath|size|mtimeNs)[:16]
  path         TEXT NOT NULL,
  size         INTEGER NOT NULL,
  created_at   TEXT NOT NULL,
  last_used_at TEXT NOT NULL,
  PRIMARY KEY (book_id, src_fp)
) WITHOUT ROWID;

CREATE TABLE kepub_failures (
  book_id TEXT NOT NULL,
  src_fp  TEXT NOT NULL,
  err     TEXT NOT NULL,
  at      TEXT NOT NULL,
  PRIMARY KEY (book_id, src_fp)
) WITHOUT ROWID;

CREATE TABLE cover_cache (
  image_id     TEXT NOT NULL,
  bucket       TEXT NOT NULL,          -- small | medium | large
  path         TEXT NOT NULL,
  width        INTEGER NOT NULL DEFAULT 0,
  height       INTEGER NOT NULL DEFAULT 0,
  size         INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,
  last_used_at TEXT NOT NULL,
  PRIMARY KEY (image_id, bucket)
) WITHOUT ROWID;

-- Operational journals surfaced in the web UI.

CREATE TABLE sync_runs (
  id             INTEGER PRIMARY KEY,
  device_id      INTEGER NOT NULL,
  sync_point_id  TEXT NOT NULL,
  started_at     TEXT NOT NULL,
  finished_at    TEXT,
  requests       INTEGER NOT NULL DEFAULT 0,
  new_books      INTEGER NOT NULL DEFAULT 0,
  changed_books  INTEGER NOT NULL DEFAULT 0,
  removed_books  INTEGER NOT NULL DEFAULT 0,
  reading_states INTEGER NOT NULL DEFAULT 0,
  tags           INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT 'running'
);
CREATE INDEX idx_sync_runs_device ON sync_runs(device_id, started_at DESC);

CREATE TABLE scan_runs (
  id          INTEGER PRIMARY KEY,
  source_id   INTEGER NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  status      TEXT NOT NULL,
  error       TEXT NOT NULL DEFAULT '',
  seen        INTEGER NOT NULL DEFAULT 0,
  added       INTEGER NOT NULL DEFAULT 0,
  updated     INTEGER NOT NULL DEFAULT 0,
  vanished    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_scan_runs_source ON scan_runs(source_id, started_at DESC);

CREATE TABLE kv (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
