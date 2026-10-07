-- Where a source's book files are read from, as opposed to where its catalogue
-- is. For a Calibre source the two differ once the library has been copied
-- here: library_path stays the folder holding metadata.db, files_path becomes
-- this server's own copy, and the books outlive the folder.
alter table sources add column files_path text not null default '';
update sources set files_path = library_path;
