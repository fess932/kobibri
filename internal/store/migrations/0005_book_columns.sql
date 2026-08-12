-- Values of a library's own custom columns, for the books that have them.
--
-- They are kept apart from tags on purpose. A tag is the library's own word for
-- a book and belongs in tags_json; a custom column is the library owner's
-- private taxonomy — "Read status", "Shelf", "Mood" — and conflating the two
-- would make a shelf out of every one of them and lose which is which.
--
-- Rows are replaced wholesale for a source book whenever it is read, the same
-- way its formats are.
CREATE TABLE source_book_columns (
  source_book_id INTEGER NOT NULL REFERENCES source_books(id) ON DELETE CASCADE,
  label          TEXT NOT NULL,   -- the column's label, without the leading '#'
  value          TEXT NOT NULL,
  PRIMARY KEY (source_book_id, label, value)
) WITHOUT ROWID;
