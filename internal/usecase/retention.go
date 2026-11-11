package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	retentionBatchSize = 1000
	retentionTimeout   = time.Minute
)

// Retention deletes idempotency keys once their TTL has passed and outbox
// events some time after they were published. Keys are kept for at least
// the TTL: a retry within that window is always recognised, a request sent
// after it is treated as new. Several instances can run concurrently.
type Retention struct {
	store           Store
	keyTTL          time.Duration
	outboxRetention time.Duration
	interval        time.Duration
	log             *slog.Logger
	now             func() time.Time
}

func NewRetention(store Store, keyTTL, outboxRetention, interval time.Duration, log *slog.Logger) *Retention {
	return &Retention{
		store:           store,
		keyTTL:          keyTTL,
		outboxRetention: outboxRetention,
		interval:        interval,
		log:             log,
		now:             func() time.Time { return time.Now().UTC() },
	}
}

// Run prunes once immediately and then every interval until ctx is cancelled.
func (r *Retention) Run(ctx context.Context) {
	r.log.Info("retention job started", "interval", r.interval.String(),
		"idempotency_key_ttl", r.keyTTL.String(), "outbox_retention", r.outboxRetention.String())
	defer r.log.Info("retention job stopped")

	for ctx.Err() == nil {
		runCtx, cancel := context.WithTimeout(ctx, retentionTimeout)
		keys, events, err := r.PruneOnce(runCtx)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Warn("retention run failed", "keys_deleted", keys, "events_deleted", events, "error", err)
		case keys > 0 || events > 0:
			r.log.Info("retention run finished", "keys_deleted", keys, "events_deleted", events)
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.interval):
		}
	}
}

// PruneOnce deletes everything that is currently past its retention period,
// in small batches so that no statement holds locks for long.
func (r *Retention) PruneOnce(ctx context.Context) (keys, events int, err error) {
	now := r.now()
	keys, err = deleteInBatches(ctx, func(ctx context.Context) (int, error) {
		return r.store.Idempotency().DeleteExpired(ctx, now.Add(-r.keyTTL), retentionBatchSize)
	})
	if err != nil {
		return keys, 0, fmt.Errorf("delete expired idempotency keys: %w", err)
	}
	events, err = deleteInBatches(ctx, func(ctx context.Context) (int, error) {
		return r.store.Outbox().DeletePublished(ctx, now.Add(-r.outboxRetention), retentionBatchSize)
	})
	if err != nil {
		return keys, events, fmt.Errorf("delete published outbox events: %w", err)
	}
	return keys, events, nil
}

func deleteInBatches(ctx context.Context, deleteBatch func(context.Context) (int, error)) (int, error) {
	total := 0
	for {
		n, err := deleteBatch(ctx)
		total += n
		if err != nil || n < retentionBatchSize {
			return total, err
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
