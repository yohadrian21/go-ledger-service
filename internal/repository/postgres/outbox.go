package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
)

type outboxRepo struct {
	db DBTX
}

func (r outboxRepo) Add(ctx context.Context, events ...domain.Event) error {
	b := &pgx.Batch{}
	for _, e := range events {
		b.Queue(`INSERT INTO outbox_events (id, event_type, payload, occurred_at) VALUES ($1, $2, $3, $4)`,
			e.ID, e.Type, e.Payload, e.OccurredAt)
	}
	return r.db.SendBatch(ctx, b).Close()
}

// FetchPending uses SKIP LOCKED so that several relay instances can work
// through the outbox in parallel without blocking on each other.
func (r outboxRepo) FetchPending(ctx context.Context, limit int) ([]domain.Event, error) {
	rows, err := r.db.Query(ctx,
		`SELECT id, event_type, payload, occurred_at FROM outbox_events
		 WHERE published_at IS NULL
		 ORDER BY seq
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Event, error) {
		var e domain.Event
		err := row.Scan(&e.ID, &e.Type, &e.Payload, &e.OccurredAt)
		e.OccurredAt = e.OccurredAt.UTC()
		return e, err
	})
}

func (r outboxRepo) MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE outbox_events SET published_at = $2 WHERE id = ANY($1)`, ids, at)
	return err
}
