ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
-- Keys were only unique per client. Keep one row per key so that the global
-- primary key can be restored.
DELETE FROM idempotency_keys a USING idempotency_keys b
WHERE a.key = b.key AND a.client_id > b.client_id;
ALTER TABLE idempotency_keys ADD PRIMARY KEY (key);
ALTER TABLE idempotency_keys DROP COLUMN client_id;

ALTER TABLE accounts DROP CONSTRAINT accounts_owner_id_not_blank;
ALTER TABLE accounts DROP CONSTRAINT accounts_external_has_no_owner;
ALTER TABLE accounts DROP COLUMN owner_id;
