-- Let the retention job find expired idempotency keys and old published
-- outbox events without scanning the whole table.
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);
CREATE INDEX outbox_events_published_at_idx ON outbox_events (published_at) WHERE published_at IS NOT NULL;
