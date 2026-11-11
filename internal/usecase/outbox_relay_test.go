package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

type fakePublisher struct {
	mu        sync.Mutex
	published []domain.Event
	failAfter int // fail once this many events have been published; <0 never fails
}

func (p *fakePublisher) Publish(_ context.Context, e domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failAfter >= 0 && len(p.published) >= p.failAfter {
		return errors.New("broker unavailable")
	}
	p.published = append(p.published, e)
	return nil
}

func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.published)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestOutboxRelayPublishesPendingEvents(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	mustDeposit(t, l, acc.ID, 100)

	pub := &fakePublisher{failAfter: -1}
	relay := usecase.NewOutboxRelay(store, pub, 10, time.Second, discardLogger())

	n, err := relay.RelayOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RelayOnce = %d, %v; want 2, nil", n, err)
	}
	if pub.published[0].Type != domain.EventAccountCreated || pub.published[1].Type != domain.EventTransactionPosted {
		t.Fatalf("events published out of order: %v, %v", pub.published[0].Type, pub.published[1].Type)
	}
	if pending := pendingEventTypes(t, store); len(pending) != 0 {
		t.Fatalf("events still pending: %v", pending)
	}

	if n, err := relay.RelayOnce(ctx); err != nil || n != 0 {
		t.Fatalf("second RelayOnce = %d, %v; want 0, nil", n, err)
	}
}

func TestOutboxRelayKeepsUnpublishedEventsOnFailure(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	mustDeposit(t, l, acc.ID, 100)
	mustDeposit(t, l, acc.ID, 200)

	pub := &fakePublisher{failAfter: 1}
	relay := usecase.NewOutboxRelay(store, pub, 10, time.Second, discardLogger())

	n, err := relay.RelayOnce(ctx)
	if err == nil || n != 1 {
		t.Fatalf("RelayOnce = %d, %v; want 1 and an error", n, err)
	}
	if pending := pendingEventTypes(t, store); len(pending) != 2 {
		t.Fatalf("got %d pending events, want 2", len(pending))
	}

	pub.failAfter = -1
	if n, err := relay.RelayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("RelayOnce after recovery = %d, %v; want 2, nil", n, err)
	}
	if pub.count() != 3 {
		t.Fatalf("published %d events in total, want 3", pub.count())
	}
}

func TestOutboxRelayRunDrainsBacklogAndStops(t *testing.T) {
	l, store := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	for range 4 {
		mustDeposit(t, l, acc.ID, 1)
	}

	pub := &fakePublisher{failAfter: -1}
	relay := usecase.NewOutboxRelay(store, pub, 2, time.Hour, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		relay.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for pub.count() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("published %d events, want 5", pub.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop after cancellation")
	}
}
