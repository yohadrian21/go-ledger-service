//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/repository/postgres"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

func TestMigrateIsIdempotent(t *testing.T) {
	_, pool := newLedger(t)
	if err := postgres.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	alice := createAccount(t, l, "USD")
	bob := createAccount(t, l, "USD")

	deposit(t, l, alice.ID, 10_000)
	transfer, _, err := l.Transfer(ctx, usecase.TransferInput{
		IdempotencyKey: "transfer-1", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: 2_500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Withdraw(ctx, usecase.WithdrawInput{IdempotencyKey: "withdraw-1", AccountID: bob.ID, Amount: 500}); err != nil {
		t.Fatal(err)
	}

	if got := balanceOf(t, l, alice.ID); got != 7_500 {
		t.Errorf("alice balance = %d, want 7500", got)
	}
	if got := balanceOf(t, l, bob.ID); got != 2_000 {
		t.Errorf("bob balance = %d, want 2000", got)
	}

	replay, replayed, err := l.Transfer(ctx, usecase.TransferInput{
		IdempotencyKey: "transfer-1", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: 2_500,
	})
	if err != nil || !replayed {
		t.Fatalf("replay: replayed=%v err=%v", replayed, err)
	}
	if !reflect.DeepEqual(replay, transfer) {
		t.Fatalf("replayed transaction differs:\n got %+v\nwant %+v", replay, transfer)
	}

	page, err := l.ListTransactions(ctx, usecase.ListTransactionsInput{AccountID: bob.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Lines[0].Type != domain.TransactionWithdrawal || page.NextCursor == 0 {
		t.Fatalf("unexpected first page: %+v", page)
	}
	page, err = l.ListTransactions(ctx, usecase.ListTransactionsInput{AccountID: bob.ID, Limit: 1, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Lines) != 1 || page.Lines[0].TransactionID != transfer.ID || page.Lines[0].BalanceAfter != 2_500 || page.NextCursor != 0 {
		t.Fatalf("unexpected last page: %+v", page)
	}

	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 5 {
		t.Errorf("pending outbox events = %d, want 5 (2 accounts, 3 transactions)", pending)
	}
	assertLedgerConsistent(t, pool)
}

func TestExternalAccountsAreNotAddressable(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	acc := createAccount(t, l, "USD")
	dep := deposit(t, l, acc.ID, 100)

	var externalID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM accounts WHERE kind = 'external' AND currency = 'USD'`).Scan(&externalID); err != nil {
		t.Fatal(err)
	}
	if dep.Entries[0].AccountID != externalID {
		t.Fatalf("deposit should debit the external account")
	}

	if _, err := l.GetAccount(ctx, externalID); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("GetAccount(external) = %v, want ErrAccountNotFound", err)
	}
	_, _, err := l.Transfer(ctx, usecase.TransferInput{
		IdempotencyKey: "mint", FromAccountID: externalID, ToAccountID: acc.ID, Amount: 1_000_000,
	})
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("transfer from external account = %v, want ErrAccountNotFound", err)
	}
}

func TestDatabaseRejectsNegativeCustomerBalance(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	acc := createAccount(t, l, "USD")

	_, err := pool.Exec(ctx, `UPDATE accounts SET balance = -1 WHERE id = $1`, acc.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "accounts_balance_non_negative" {
		t.Fatalf("expected check constraint violation, got %v", err)
	}

	acc.Balance = -1
	err = postgres.NewStore(pool).Accounts().UpdateBalances(ctx, acc)
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("UpdateBalances = %v, want ErrInsufficientFunds", err)
	}
}

func TestDatabaseRejectsUnbalancedTransaction(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	acc := createAccount(t, l, "USD")
	store := postgres.NewStore(pool)

	err := store.InTx(ctx, func(tx usecase.Store) error {
		return tx.Transactions().Create(ctx, domain.Transaction{
			ID:        uuid.New(),
			Type:      domain.TransactionDeposit,
			Amount:    100,
			Currency:  "USD",
			Entries:   []domain.Entry{{AccountID: acc.ID, Amount: 100, BalanceAfter: 100}},
			CreatedAt: time.Now(),
		})
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("expected commit to fail with check_violation, got %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transactions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("unbalanced transaction was persisted")
	}
}

func TestOutboxFetchPendingSkipsLockedEvents(t *testing.T) {
	ctx := context.Background()
	l, pool := newLedger(t)
	createAccount(t, l, "USD")
	createAccount(t, l, "USD")
	store := postgres.NewStore(pool)

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	first, err := postgres.NewStore(tx1).Outbox().FetchPending(ctx, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first relay: %d events, %v", len(first), err)
	}

	err = store.InTx(ctx, func(tx usecase.Store) error {
		second, err := tx.Outbox().FetchPending(ctx, 10)
		if err != nil {
			return err
		}
		if len(second) != 1 || second[0].ID == first[0].ID {
			return fmt.Errorf("second relay should only see the unlocked event, got %d events", len(second))
		}
		return tx.Outbox().MarkPublished(ctx, []uuid.UUID{second[0].ID}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx1.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}

	pending, err := store.Outbox().FetchPending(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != first[0].ID {
		t.Fatalf("after rollback the first event should be pending again: %v, %v", pending, err)
	}
}
