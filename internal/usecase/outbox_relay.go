package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

const relayBatchTimeout = 10 * time.Second

// OutboxRelay moves events from the transactional outbox to the message
// broker. Delivery is at-least-once: an event can be published again if the
// process dies between publishing and marking it as published, so consumers
// should deduplicate by event ID. Several relays can run concurrently.
type OutboxRelay struct {
	store     Store
	publisher Publisher
	batchSize int
	interval  time.Duration
	log       *slog.Logger
	now       func() time.Time
}

func NewOutboxRelay(store Store, publisher Publisher, batchSize int, interval time.Duration, log *slog.Logger) *OutboxRelay {
	return &OutboxRelay{
		store:     store,
		publisher: publisher,
		batchSize: batchSize,
		interval:  interval,
		log:       log,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// Run relays events until ctx is cancelled. A full batch is followed
// immediately by the next one so that a backlog drains quickly.
func (r *OutboxRelay) Run(ctx context.Context) {
	r.log.Info("outbox relay started", "interval", r.interval.String(), "batch_size", r.batchSize)
	defer r.log.Info("outbox relay stopped")

	for ctx.Err() == nil {
		n, err := r.relayBatch(ctx)
		if err != nil {
			r.log.Warn("outbox relay batch failed", "published", n, "error", err)
		} else if n > 0 {
			r.log.Debug("outbox events published", "count", n)
		}
		if err == nil && n == r.batchSize {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.interval):
		}
	}
}

// relayBatch is not cancelled by ctx: on shutdown the in-flight batch is
// allowed to finish instead of being rolled back after its events were sent.
func (r *OutboxRelay) relayBatch(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relayBatchTimeout)
	defer cancel()
	return r.RelayOnce(ctx)
}

// RelayOnce publishes up to one batch of pending events, in order, and marks
// the successfully published ones. It stops at the first publish error and
// returns the number of events published.
func (r *OutboxRelay) RelayOnce(ctx context.Context) (int, error) {
	var (
		published  []uuid.UUID
		publishErr error
	)
	err := r.store.InTx(ctx, func(tx Store) error {
		published, publishErr = nil, nil
		events, err := tx.Outbox().FetchPending(ctx, r.batchSize)
		if err != nil {
			return err
		}
		for _, e := range events {
			if err := r.publisher.Publish(ctx, e); err != nil {
				publishErr = fmt.Errorf("publish event %s: %w", e.ID, err)
				break
			}
			published = append(published, e.ID)
		}
		if len(published) == 0 {
			return nil
		}
		return tx.Outbox().MarkPublished(ctx, published, r.now())
	})
	if err != nil {
		return 0, err
	}
	return len(published), publishErr
}
