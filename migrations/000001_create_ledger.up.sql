CREATE TABLE accounts (
    id         UUID PRIMARY KEY,
    name       TEXT        NOT NULL,
    currency   TEXT        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    kind       TEXT        NOT NULL CHECK (kind IN ('customer', 'external')),
    balance    BIGINT      NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    -- The application already rejects overdrafts; this is the last line of defence.
    CONSTRAINT accounts_balance_non_negative CHECK (kind = 'external' OR balance >= 0)
);

-- Exactly one external account (money outside the ledger) per currency.
CREATE UNIQUE INDEX accounts_external_currency_key ON accounts (currency) WHERE kind = 'external';

CREATE TABLE transactions (
    id         UUID PRIMARY KEY,
    type       TEXT        NOT NULL CHECK (type IN ('deposit', 'withdrawal', 'transfer')),
    amount     BIGINT      NOT NULL CHECK (amount > 0),
    currency   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

-- Append-only ledger entries. amount is signed from the account's point of view.
CREATE TABLE entries (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id UUID   NOT NULL REFERENCES transactions (id),
    account_id     UUID   NOT NULL REFERENCES accounts (id),
    amount         BIGINT NOT NULL CHECK (amount <> 0),
    balance_after  BIGINT NOT NULL
);

CREATE INDEX entries_account_id_id_idx ON entries (account_id, id);
CREATE INDEX entries_transaction_id_idx ON entries (transaction_id);

-- Double-entry invariant: the entries of a transaction must sum to zero. The
-- check is deferred to commit time so that all entries can be inserted first.
CREATE FUNCTION check_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT SUM(amount) FROM entries WHERE transaction_id = NEW.transaction_id) <> 0 THEN
        RAISE EXCEPTION 'transaction % is not balanced', NEW.transaction_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_transaction_balanced();

CREATE TABLE idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT        NOT NULL,
    resource_id  UUID        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE outbox_events (
    id           UUID PRIMARY KEY,
    -- Insertion order; the relay publishes in this order.
    seq          BIGINT GENERATED ALWAYS AS IDENTITY,
    event_type   TEXT        NOT NULL,
    payload      JSONB       NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ
);

CREATE INDEX outbox_events_pending_idx ON outbox_events (seq) WHERE published_at IS NULL;
