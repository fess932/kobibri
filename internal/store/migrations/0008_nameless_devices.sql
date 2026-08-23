-- Clear out the nameless duplicate every reader made of itself.
--
-- A device is keyed by (token_hash, kobo_device_id), but the first requests of
-- a session — /v1/auth/device, /v1/affiliate, /v1/initialization — arrive
-- before the reader starts sending x-kobo-deviceid. Those were filed under an
-- id of '', which is a different key, so every reader appeared twice: once as
-- itself, and once as a row with no device id that had never synced.
--
-- UpsertDevice no longer makes them. This clears out the ones already there.
--
-- Deliberately narrow. A row only goes if all three hold:
--   * it has no device id, so it is not a reader we can address;
--   * the same token has another row, so a real one exists to keep;
--   * it never completed a sync, so no snapshot or tombstone hangs off it.
-- A token whose only row is nameless is left alone: that is a reader which has
-- simply not sent its id yet, and the next request that does will name it.
DELETE FROM devices
 WHERE kobo_device_id = ''
   AND last_sync_at IS NULL
   AND EXISTS (SELECT 1 FROM devices other
                WHERE other.token_hash = devices.token_hash
                  AND other.kobo_device_id <> '');
