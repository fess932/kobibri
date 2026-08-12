-- Books that Calibre holds in some other format are converted to EPUB before
-- anything else happens to them, and the result is cached here.
--
-- Same shape as kepub_cache and for the same reason: it is a derived artefact,
-- rebuildable at any time, so the row exists to find and evict the file rather
-- than to hold anything of value.
CREATE TABLE epub_cache (
  book_id      TEXT NOT NULL,
  src_fp       TEXT NOT NULL,          -- sha1(absPath|size|mtimeNs)[:16]
  src_format   TEXT NOT NULL,          -- what it was converted from
  path         TEXT NOT NULL,
  size         INTEGER NOT NULL,
  created_at   TEXT NOT NULL,
  last_used_at TEXT NOT NULL,
  PRIMARY KEY (book_id, src_fp)
) WITHOUT ROWID;

CREATE TABLE epub_failures (
  book_id TEXT NOT NULL,
  src_fp  TEXT NOT NULL,
  err     TEXT NOT NULL,
  at      TEXT NOT NULL,
  PRIMARY KEY (book_id, src_fp)
) WITHOUT ROWID;

-- Which format the winning source row will be converted from, so the library
-- listing can say so without opening anything.
ALTER TABLE books ADD COLUMN convert_from TEXT NOT NULL DEFAULT '';
