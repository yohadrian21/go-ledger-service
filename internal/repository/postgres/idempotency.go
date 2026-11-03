package postgres

import (
	"context"

	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

type idempotencyRepo struct {
	db DBTX
}

// Reserve relies on the primary key: if a concurrent transaction has
// inserted the same key but not committed yet, the INSERT waits for it. Once
// it commits, ON CONFLICT DO NOTHING applies and the existing row is returned;
// if it rolls back, this insert succeeds.
func (r idempotencyRepo) Reserve(ctx context.Context, rec usecase.IdempotencyRecord) (usecase.IdempotencyRecord, bool, error) {
	tag, err := r.db.Exec(ctx,
		`INSERT INTO idempotency_keys (key, request_hash, resource_id) VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO NOTHING`,
		rec.Key, rec.RequestHash, rec.ResourceID)
	if err != nil {
		return usecase.IdempotencyRecord{}, false, err
	}
	if tag.RowsAffected() == 1 {
		return rec, true, nil
	}

	existing := usecase.IdempotencyRecord{Key: rec.Key}
	err = r.db.QueryRow(ctx,
		`SELECT request_hash, resource_id FROM idempotency_keys WHERE key = $1`, rec.Key).
		Scan(&existing.RequestHash, &existing.ResourceID)
	return existing, false, err
}
