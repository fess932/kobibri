-- Undoing a merge that should not have happened.
--
-- Two different books can share a title and an author — different translations,
-- two anthologies called "Selected Poems", a reissue with new content. When
-- neither side carries a uuid or an ISBN, title-and-author is the only key
-- there is, and it will sometimes join books that are not the same book.
--
-- Splitting them apart is not enough on its own: the next scan recomputes the
-- same keys and merges them straight back. This column pins a source row to the
-- book it was moved to, and Attach honours it instead of looking the keys up.
-- It is only ever set by an explicit decision in the interface.
ALTER TABLE source_books ADD COLUMN pinned_book_id TEXT;

CREATE INDEX idx_source_books_pinned ON source_books(pinned_book_id)
  WHERE pinned_book_id IS NOT NULL;
