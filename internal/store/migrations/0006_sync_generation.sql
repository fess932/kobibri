-- What the library looked like when a snapshot was taken.
--
-- A device checks in every few minutes and almost always has nothing to be told.
-- Answering that used to cost a whole new snapshot — every book written into
-- sync_point_books and the previous one deleted — which on a large library is
-- tens of thousands of rows written every few minutes, per device, forever.
--
-- With the fingerprint recorded here, a sync that finds it unchanged answers an
-- empty list and writes nothing at all.
ALTER TABLE sync_points ADD COLUMN generation TEXT NOT NULL DEFAULT '';
