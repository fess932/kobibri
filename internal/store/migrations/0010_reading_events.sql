-- Reading history, and the word index that makes it mean something.
--
-- reading_states holds one row per (user, book) and every PUT overwrites it, so
-- the only thing anyone could ever say was where a reader is right now. Every
-- progress report before this one was thrown away as it arrived.
--
-- reading_events is append-only and written in the same transaction as the
-- state update. Three or four rows an hour per reader.
--
-- What makes speed computable is not in the events. The device sends whole
-- numbers: a reader can spend half an hour in a long book and send
-- ProgressPercent: 3 every single time. The position that does move is
-- Location — a spine file and a koboSpan id, kobo.<block>.<segment>, and we
-- generate those spans ourselves. book_text_blocks records how many words come
-- before each block, so a location resolves to a word offset and a delta
-- between two events is a real number of words read.
--
-- spent_delta is the honest denominator. SpentReadingMinutes is the device's
-- own cumulative counter of time actually spent reading, which is not the same
-- as the wall clock between two reports: 28 minutes apart, 13 minutes read.
-- It is per device and can go backwards when a book is re-added, so the delta
-- is computed against the previous event from the same device and clamped.

CREATE TABLE reading_events (
  id                INTEGER PRIMARY KEY,
  user_id           INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  book_id           TEXT NOT NULL,
  device_id         INTEGER,
  at                TEXT NOT NULL,
  device_at         TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT '',
  percent           REAL,
  source            TEXT NOT NULL DEFAULT '',
  block             INTEGER,
  span              TEXT NOT NULL DEFAULT '',
  spent_minutes     INTEGER,
  spent_delta       INTEGER NOT NULL DEFAULT 0,
  remaining_minutes INTEGER
);

CREATE INDEX reading_events_book ON reading_events(user_id, book_id, id);
CREATE INDEX reading_events_when ON reading_events(user_id, at);

-- The word index, built from the file the device actually receives. Rebuilt
-- when the fingerprint moves, which is what happens when a serial gains
-- chapters: the offsets change, and every past event maps onto the new ones.
CREATE TABLE book_text_index (
  book_id     TEXT PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL,
  words       INTEGER NOT NULL,
  documents   INTEGER NOT NULL,
  spanned     INTEGER NOT NULL DEFAULT 0,
  built_at    TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE book_text_docs (
  book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  source  TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  title   TEXT NOT NULL DEFAULT '',
  words   INTEGER NOT NULL,
  before  INTEGER NOT NULL,
  PRIMARY KEY (book_id, source)
) WITHOUT ROWID;

CREATE TABLE book_text_blocks (
  book_id TEXT NOT NULL REFERENCES books(id) ON DELETE CASCADE,
  source  TEXT NOT NULL,
  block   INTEGER NOT NULL,
  before  INTEGER NOT NULL,
  PRIMARY KEY (book_id, source, block)
) WITHOUT ROWID;

-- StatusInfo carries these two and they were parsed and dropped, so every
-- answer said the book had been started zero times.
ALTER TABLE reading_states ADD COLUMN times_started INTEGER NOT NULL DEFAULT 0;
ALTER TABLE reading_states ADD COLUMN last_started TEXT NOT NULL DEFAULT '';
