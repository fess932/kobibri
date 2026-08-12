-- Books imported from a web serial, rather than read out of a Calibre library.
--
-- They join the library through the same path as everything else: a source_books
-- row, an identity key, a canonical book. Only where the file comes from differs,
-- so nothing downstream — merging, sync, conversion — needs to know.

ALTER TABLE sources ADD COLUMN kind TEXT NOT NULL DEFAULT 'calibre'; -- calibre | web

-- The canonical link a book was imported from. It is also its identity key,
-- since such a book has neither a Calibre uuid nor an ISBN.
ALTER TABLE source_books ADD COLUMN web_url TEXT NOT NULL DEFAULT '';

-- Where novelkit keeps its download cache for this book, so fetching newly
-- published chapters resumes rather than starting over.
CREATE TABLE web_imports (
  source_book_id INTEGER PRIMARY KEY REFERENCES source_books(id) ON DELETE CASCADE,
  url            TEXT NOT NULL,
  provider       TEXT NOT NULL,          -- novelkit source id, e.g. "ranobelib"
  remote_book_id TEXT NOT NULL,
  edition_id     TEXT NOT NULL DEFAULT '',
  job_dir        TEXT NOT NULL,          -- relative to the imports root
  chapters_total INTEGER NOT NULL DEFAULT 0,
  chapters_done  INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE INDEX idx_web_imports_url ON web_imports(url);
