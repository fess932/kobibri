-- Which part of a title an import covers, as positions in the translation's
-- chapter list starting at 1. Zero means no bound.
--
-- It has to be stored: every later check plans the download again, and a plan
-- with no range widens the book back to its first chapter.
alter table web_imports add column from_chapter integer not null default 0;
alter table web_imports add column to_chapter   integer not null default 0;
