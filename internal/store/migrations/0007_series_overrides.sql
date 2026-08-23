-- A series set here rather than in Calibre.
--
-- Series reach a book the same way every other field does: Resolve takes the
-- winning source row whole and writes the result into books. That makes any
-- edit written straight into books last exactly until the next scan touches
-- the book, which is not an edit at all.
--
-- So the edit lives beside the derived value instead, and Resolve lays it over
-- the top. The same shape as books.hidden: a server-side decision that survives
-- a scan because a scan never writes it.
--
-- The row's presence is the override, not its contents. An empty series_name
-- means "this book is in no series", which is a real thing to want and cannot
-- be said by leaving the row out — that means "whatever Calibre says".
CREATE TABLE book_series_overrides (
  book_id      TEXT PRIMARY KEY REFERENCES books(id) ON DELETE CASCADE,
  series_name  TEXT NOT NULL DEFAULT '',
  series_index REAL,
  updated_at   TEXT NOT NULL
) WITHOUT ROWID;
