-- Customer accounts belong to the API client that opened them; external
-- accounts belong to nobody. Customer accounts opened before this migration
-- have no owner (NULL): no client can reach them, only operators, until they
-- are assigned one with UPDATE accounts SET owner_id = ... .
ALTER TABLE accounts ADD COLUMN owner_id TEXT;
ALTER TABLE accounts ADD CONSTRAINT accounts_external_has_no_owner CHECK (kind = 'customer' OR owner_id IS NULL);
ALTER TABLE accounts ADD CONSTRAINT accounts_owner_id_not_blank CHECK (owner_id <> '');

-- Idempotency keys are unique per client, so one client can never claim or
-- replay another client's key. Existing keys get an empty client ID, which no
-- client can have; they stop matching and expire with the retention job.
ALTER TABLE idempotency_keys ADD COLUMN client_id TEXT NOT NULL DEFAULT '';
ALTER TABLE idempotency_keys ALTER COLUMN client_id DROP DEFAULT;
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys ADD PRIMARY KEY (client_id, key);
