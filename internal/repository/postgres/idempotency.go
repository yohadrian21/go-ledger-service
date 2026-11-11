package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

type idempotencyRepo struct {
	db DBTX
}

// Reserve relies on the primary key (client_id, key): if a concurrent
// transaction has inserted the same key but not committed yet, the INSERT
// waits for it. Once it commits, ON CONFLICT DO NOTHING applies and the
// existing row is returned; if it rolls back, this insert succeeds.
func (r idempotencyRepo) Reserve(ctx context.Context, rec usecase.IdempotencyRecord) (usecase.IdempotencyRecord, bool, error) {
	for attempt := 0; ; attempt++ {
		tag, err := r.db.Exec(ctx,
			`INSERT INTO idempotency_keys (client_id, key, request_hash, resource_id, created_at) VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (client_id, key) DO NOTHING`,
			rec.ClientID, rec.Key, rec.RequestHash, rec.ResourceID, rec.CreatedAt)
		if err != nil {
			return usecase.IdempotencyRecord{}, false, err
		}
		if tag.RowsAffected() == 1 {
			return rec, true, nil
		}

		existing := usecase.IdempotencyRecord{ClientID: rec.ClientID, Key: rec.Key}
		err = r.db.QueryRow(ctx,
			`SELECT request_hash, resource_id, created_at FROM idempotency_keys WHERE client_id = $1 AND key = $2`,
			rec.ClientID, rec.Key).
			Scan(&existing.RequestHash, &existing.ResourceID, &existing.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) && attempt == 0 {
			// The key expired and was pruned between the two statements.
			continue
		}
		existing.CreatedAt = existing.CreatedAt.UTC()
		return existing, false, err
	}
}

// DeleteExpired skips rows locked by a concurrent cleanup, so several
// replicas can prune at the same time without waiting for each other.
func (r idempotencyRepo) DeleteExpired(ctx context.Context, createdBefore time.Time, limit int) (int, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE (client_id, key) IN (
		   SELECT client_id, key FROM idempotency_keys
		   WHERE created_at < $1
		   LIMIT $2
		   FOR UPDATE SKIP LOCKED)`, createdBefore, limit)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
