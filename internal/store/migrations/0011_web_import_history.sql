-- A check that finds nothing new must leave the book exactly as it was.
--
-- Reassembling the EPUB rewrites the file and the cover beside it, and both are
-- observable from a device: the cover's mtime is part of CoverImageId, so
-- serving_hash moves, metadata_rev is bumped and every device re-downloads a
-- book whose text did not change. The kepub cache is keyed by the file's mtime
-- too, so the conversion is thrown away with it.
--
-- build_sig is what the assembled file was built from — chapters, their state
-- and the metadata that goes into the file. Same signature and the file still
-- on disk means there is nothing to do.
ALTER TABLE web_imports ADD COLUMN build_sig TEXT NOT NULL DEFAULT '';

-- When the site was last asked, as opposed to when the book last changed.
-- Without it a book that is checked every ten hours and updated twice a year
-- looks abandoned.
ALTER TABLE web_imports ADD COLUMN checked_at TEXT NOT NULL DEFAULT '';

-- What each check actually changed. Nothing downstream reads it; it exists so
-- that "why did this book update?" has an answer other than the file's mtime.
CREATE TABLE web_import_events (
  id              INTEGER PRIMARY KEY,
  source_book_id  INTEGER NOT NULL REFERENCES source_books(id) ON DELETE CASCADE,
  at              TEXT NOT NULL,
  kind            TEXT NOT NULL,          -- imported | chapters | metadata | error
  chapters_before INTEGER NOT NULL DEFAULT 0,
  chapters_after  INTEGER NOT NULL DEFAULT 0,
  detail          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_web_import_events_book ON web_import_events(source_book_id, id DESC);
