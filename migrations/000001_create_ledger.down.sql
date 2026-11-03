DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS entries;
DROP FUNCTION IF EXISTS check_transaction_balanced();
DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS accounts;
