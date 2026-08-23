-- Clear Calibre's "no date" sentinel out of stored publication dates.
--
-- Calibre writes UNDEFINED_DATE, datetime(101, 1, 1), into pubdate when a book
-- has none. kobibri parsed it as a real timestamp and sent devices
-- "PublicationDate":"0101-01-01T00:00:00Z" — a publication date in the year 101,
-- for most of a library. The reader is inside a sync response it either has to
-- parse or discard, and nothing said which it did.
--
-- parseTime now rejects it at the source. This clears what is already stored;
-- the books are re-resolved at the next start so the serving hash follows.
UPDATE source_books SET published_at = ''
 WHERE published_at LIKE '0101-%' OR published_at LIKE '0001-%';
