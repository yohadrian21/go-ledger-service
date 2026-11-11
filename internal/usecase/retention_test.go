package usecase_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

const (
	keyTTL          = 24 * time.Hour
	outboxRetention = 7 * 24 * time.Hour
)

func TestRetentionPrunesExpiredKeysAndPublishedEvents(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	dep := usecase.DepositInput{IdempotencyKey: "dep-1", AccountID: acc.ID, Amount: 100}
	if _, _, err := l.Deposit(ctx, ops, dep); err != nil {
		t.Fatal(err)
	}
	if n, err := usecase.NewOutboxRelay(store, &fakePublisher{failAfter: -1}, 10, time.Second, discardLogger()).RelayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("RelayOnce = %d, %v", n, err)
	}
	mustDeposit(t, l, acc.ID, 1) // its event stays pending

	start := time.Now().UTC()
	retention := usecase.NewRetention(store, keyTTL, outboxRetention, time.Hour, discardLogger())
	prune := func(at time.Duration) (int, int) {
		t.Helper()
		retention.SetClock(func() time.Time { return start.Add(at) })
		keys, events, err := retention.PruneOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return keys, events
	}

	if keys, events := prune(0); keys != 0 || events != 0 {
		t.Fatalf("nothing has expired yet, but %d keys and %d events were deleted", keys, events)
	}
	if _, replayed, err := l.Deposit(ctx, ops, dep); err != nil || !replayed {
		t.Fatalf("retry within the TTL: replayed=%v err=%v", replayed, err)
	}

	if keys, events := prune(keyTTL + time.Minute); keys != 3 || events != 0 {
		t.Fatalf("after the key TTL: deleted %d keys and %d events, want 3 and 0", keys, events)
	}
	// The key can be used again once it has expired.
	dep.Amount = 200
	if _, replayed, err := l.Deposit(ctx, ops, dep); err != nil || replayed {
		t.Fatalf("reusing an expired key: replayed=%v err=%v", replayed, err)
	}

	if _, events := prune(outboxRetention + time.Minute); events != 2 {
		t.Fatalf("after the outbox retention: deleted %d events, want the 2 published ones", events)
	}
	if pending := pendingEventTypes(t, store); len(pending) != 2 {
		t.Fatalf("pending events must never be deleted, %d left", len(pending))
	}
	if got := balance(t, l, acc.ID); got != 301 {
		t.Fatalf("balance = %d, want 301", got)
	}
}

func TestRetentionDeletesInBatches(t *testing.T) {
	ctx := context.Background()
	_, store := newLedger(t)
	created := time.Now().UTC().Add(-2 * keyTTL)
	const total = 2500
	for i := range total {
		_, _, err := store.Idempotency().Reserve(ctx, usecase.IdempotencyRecord{
			ClientID: "c", Key: strconv.Itoa(i), RequestHash: "h", ResourceID: uuid.New(), CreatedAt: created,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	keys, _, err := usecase.NewRetention(store, keyTTL, outboxRetention, time.Hour, discardLogger()).PruneOnce(ctx)
	if err != nil || keys != total {
		t.Fatalf("PruneOnce deleted %d keys, %v; want %d", keys, err, total)
	}
}

func TestRetentionRunStopsOnCancel(t *testing.T) {
	_, store := newLedger(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		usecase.NewRetention(store, keyTTL, outboxRetention, time.Hour, discardLogger()).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retention job did not stop after cancellation")
	}
}
