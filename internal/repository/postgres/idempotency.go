package postgres

import (
	"context"

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
	existing.CreatedAt = existing.CreatedAt.UTC()
	return existing, false, err
}
