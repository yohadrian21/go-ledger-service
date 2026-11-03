//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

// TestConcurrentTransfersDoNotDoubleSpend fires many transfers from one
// account at the same time. Only as many as the balance covers may succeed.
func TestConcurrentTransfersDoNotDoubleSpend(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	alice := createAccount(t, l, "USD")
	bob := createAccount(t, l, "USD")
	deposit(t, l, alice.ID, 1_000)

	const (
		workers = 50
		amount  = 100
	)
	var succeeded, rejected atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			_, _, err := l.Transfer(ctx, usecase.TransferInput{
				IdempotencyKey: uuid.NewString(),
				FromAccountID:  alice.ID,
				ToAccountID:    bob.ID,
				Amount:         amount,
			})
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, domain.ErrInsufficientFunds):
				rejected.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	if succeeded.Load() != 10 || rejected.Load() != 40 {
		t.Fatalf("succeeded=%d rejected=%d, want 10 and 40", succeeded.Load(), rejected.Load())
	}
	if got := balanceOf(t, l, alice.ID); got != 0 {
		t.Errorf("alice balance = %d, want 0", got)
	}
	if got := balanceOf(t, l, bob.ID); got != 1_000 {
		t.Errorf("bob balance = %d, want 1000", got)
	}
	assertLedgerConsistent(t, pool)
}

// TestConcurrentOpposingTransfersDoNotDeadlock moves money back and forth
// between a few accounts in every direction at once. Without a consistent
// lock order, A->B and B->A would deadlock and PostgreSQL would abort one of
// them with SQLSTATE 40P01.
func TestConcurrentOpposingTransfersDoNotDeadlock(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)

	const (
		accounts       = 4
		initialBalance = 10_000
		workers        = 200
	)
	ids := make([]uuid.UUID, accounts)
	for i := range ids {
		ids[i] = createAccount(t, l, "EUR").ID
		deposit(t, l, ids[i], initialBalance)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		r := rand.New(rand.NewPCG(uint64(i), 42))
		from := r.IntN(accounts)
		to := (from + 1 + r.IntN(accounts-1)) % accounts
		amount := int64(1 + r.IntN(500))

		wg.Go(func() {
			<-start
			_, _, err := l.Transfer(ctx, usecase.TransferInput{
				IdempotencyKey: uuid.NewString(),
				FromAccountID:  ids[from],
				ToAccountID:    ids[to],
				Amount:         amount,
			})
			if err != nil && !errors.Is(err, domain.ErrInsufficientFunds) {
				t.Errorf("transfer %d -> %d failed: %v", from, to, err)
			}
		})
	}
	close(start)
	wg.Wait()

	var total int64
	for _, id := range ids {
		total += balanceOf(t, l, id)
	}
	if total != accounts*initialBalance {
		t.Errorf("total customer balance = %d, want %d", total, accounts*initialBalance)
	}
	assertLedgerConsistent(t, pool)
}

// TestConcurrentRetriesWithSameIdempotencyKeyPostOnce simulates a client
// retrying the same request in parallel, e.g. after a timeout.
func TestConcurrentRetriesWithSameIdempotencyKeyPostOnce(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	acc := createAccount(t, l, "USD")

	const workers = 20
	in := usecase.DepositInput{IdempotencyKey: "retry-me", AccountID: acc.ID, Amount: 1_234}

	var (
		mu       sync.Mutex
		txIDs    = map[uuid.UUID]int{}
		replayed atomic.Int64
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			tx, isReplay, err := l.Deposit(ctx, in)
			if err != nil {
				t.Errorf("deposit: %v", err)
				return
			}
			if isReplay {
				replayed.Add(1)
			}
			mu.Lock()
			txIDs[tx.ID]++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()

	if len(txIDs) != 1 {
		t.Fatalf("got %d distinct transactions, want 1", len(txIDs))
	}
	if replayed.Load() != workers-1 {
		t.Errorf("replayed = %d, want %d", replayed.Load(), workers-1)
	}
	if got := balanceOf(t, l, acc.ID); got != 1_234 {
		t.Errorf("balance = %d, want 1234 (posted exactly once)", got)
	}
	assertLedgerConsistent(t, pool)
}
