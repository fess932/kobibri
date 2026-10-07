-- A device row is keyed on its token, so once the token is revoked the row can
-- never be reached again: the same reader paired with a new token is a new row.
-- Revoking now removes them; this clears the ones left behind before it did.
delete from sync_runs where device_id in (
  select d.id from devices d
  join api_tokens t on t.token_hash = d.token_hash
  where t.revoked_at is not null);
delete from devices where token_hash in (
  select token_hash from api_tokens where revoked_at is not null);
