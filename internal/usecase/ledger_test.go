package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/repository/memory"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

func newLedger(t *testing.T) (*usecase.Ledger, *memory.Store) {
	t.Helper()
	store := memory.New()
	return usecase.NewLedger(store), store
}

func mustCreateAccount(t *testing.T, l *usecase.Ledger, currency string) domain.Account {
	t.Helper()
	acc, _, err := l.CreateAccount(context.Background(), usecase.CreateAccountInput{
		IdempotencyKey: uuid.NewString(),
		Name:           "Account " + currency,
		Currency:       currency,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return acc
}

func mustDeposit(t *testing.T, l *usecase.Ledger, id uuid.UUID, amount int64) domain.Transaction {
	t.Helper()
	tx, _, err := l.Deposit(context.Background(), usecase.DepositInput{
		IdempotencyKey: uuid.NewString(), AccountID: id, Amount: amount,
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	return tx
}

func balance(t *testing.T, l *usecase.Ledger, id uuid.UUID) int64 {
	t.Helper()
	acc, err := l.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return acc.Balance
}

func pendingEventTypes(t *testing.T, s *memory.Store) []string {
	t.Helper()
	events, err := s.Outbox().FetchPending(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func TestCreateAccount(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)

	acc, replayed, err := l.CreateAccount(ctx, usecase.CreateAccountInput{IdempotencyKey: "k1", Name: "Alice", Currency: "usd"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed || acc.Currency != "USD" || acc.Balance != 0 {
		t.Fatalf("unexpected result: %+v replayed=%v", acc, replayed)
	}

	got, err := l.GetAccount(ctx, acc.ID)
	if err != nil || got.ID != acc.ID || got.Name != "Alice" {
		t.Fatalf("GetAccount = %+v, %v", got, err)
	}
	if types := pendingEventTypes(t, store); len(types) != 1 || types[0] != domain.EventAccountCreated {
		t.Fatalf("outbox events = %v", types)
	}
}

func TestCreateAccountIdempotency(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)
	in := usecase.CreateAccountInput{IdempotencyKey: "same-key", Name: "Alice", Currency: "EUR"}

	first, _, err := l.CreateAccount(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, replayed, err := l.CreateAccount(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed || second.ID != first.ID {
		t.Fatalf("replay returned %s (replayed=%v), want %s", second.ID, replayed, first.ID)
	}
	if n := len(pendingEventTypes(t, store)); n != 1 {
		t.Fatalf("got %d outbox events, want 1", n)
	}

	in.Name = "Mallory"
	if _, _, err := l.CreateAccount(ctx, in); !errors.Is(err, usecase.ErrIdempotencyKeyReused) {
		t.Fatalf("reused key: got %v", err)
	}
}

func TestCreateAccountValidation(t *testing.T) {
	l, _ := newLedger(t)
	tests := []struct {
		name string
		in   usecase.CreateAccountInput
		want error
	}{
		{name: "missing key", in: usecase.CreateAccountInput{Name: "A", Currency: "USD"}, want: usecase.ErrIdempotencyKeyMissing},
		{name: "invalid currency", in: usecase.CreateAccountInput{IdempotencyKey: "k", Name: "A", Currency: "dollars"}, want: domain.ErrInvalidCurrency},
		{name: "blank name", in: usecase.CreateAccountInput{IdempotencyKey: "k", Name: " ", Currency: "USD"}, want: domain.ErrInvalidAccountName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := l.CreateAccount(context.Background(), tt.in); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDepositWithdrawTransfer(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)
	alice := mustCreateAccount(t, l, "USD")
	bob := mustCreateAccount(t, l, "USD")

	mustDeposit(t, l, alice.ID, 10_000)

	tx, _, err := l.Transfer(ctx, usecase.TransferInput{
		IdempotencyKey: "t1", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: 2_500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Type != domain.TransactionTransfer || len(tx.Entries) != 2 {
		t.Fatalf("unexpected transaction: %+v", tx)
	}

	if _, _, err := l.Withdraw(ctx, usecase.WithdrawInput{IdempotencyKey: "w1", AccountID: bob.ID, Amount: 1_000}); err != nil {
		t.Fatal(err)
	}

	if got := balance(t, l, alice.ID); got != 7_500 {
		t.Fatalf("alice balance = %d, want 7500", got)
	}
	if got := balance(t, l, bob.ID); got != 1_500 {
		t.Fatalf("bob balance = %d, want 1500", got)
	}

	want := []string{
		domain.EventAccountCreated, domain.EventAccountCreated,
		domain.EventTransactionPosted, domain.EventTransactionPosted, domain.EventTransactionPosted,
	}
	if got := pendingEventTypes(t, store); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("outbox events = %v, want %v", got, want)
	}
}

func TestPostingRejections(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	usd := mustCreateAccount(t, l, "USD")
	usd2 := mustCreateAccount(t, l, "USD")
	eur := mustCreateAccount(t, l, "EUR")
	mustDeposit(t, l, usd.ID, 100)

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{name: "overdraw withdrawal", want: domain.ErrInsufficientFunds, run: func() error {
			_, _, err := l.Withdraw(ctx, usecase.WithdrawInput{IdempotencyKey: "a", AccountID: usd.ID, Amount: 101})
			return err
		}},
		{name: "overdraw transfer", want: domain.ErrInsufficientFunds, run: func() error {
			_, _, err := l.Transfer(ctx, usecase.TransferInput{IdempotencyKey: "b", FromAccountID: usd.ID, ToAccountID: usd2.ID, Amount: 101})
			return err
		}},
		{name: "currency mismatch", want: domain.ErrCurrencyMismatch, run: func() error {
			_, _, err := l.Transfer(ctx, usecase.TransferInput{IdempotencyKey: "c", FromAccountID: usd.ID, ToAccountID: eur.ID, Amount: 1})
			return err
		}},
		{name: "same account", want: domain.ErrSameAccount, run: func() error {
			_, _, err := l.Transfer(ctx, usecase.TransferInput{IdempotencyKey: "d", FromAccountID: usd.ID, ToAccountID: usd.ID, Amount: 1})
			return err
		}},
		{name: "unknown account", want: domain.ErrAccountNotFound, run: func() error {
			_, _, err := l.Deposit(ctx, usecase.DepositInput{IdempotencyKey: "e", AccountID: uuid.New(), Amount: 1})
			return err
		}},
		{name: "unknown destination", want: domain.ErrAccountNotFound, run: func() error {
			_, _, err := l.Transfer(ctx, usecase.TransferInput{IdempotencyKey: "f", FromAccountID: usd.ID, ToAccountID: uuid.New(), Amount: 1})
			return err
		}},
		{name: "zero amount", want: domain.ErrInvalidAmount, run: func() error {
			_, _, err := l.Deposit(ctx, usecase.DepositInput{IdempotencyKey: "g", AccountID: usd.ID, Amount: 0})
			return err
		}},
		{name: "missing key", want: usecase.ErrIdempotencyKeyMissing, run: func() error {
			_, _, err := l.Deposit(ctx, usecase.DepositInput{AccountID: usd.ID, Amount: 1})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	if got := balance(t, l, usd.ID); got != 100 {
		t.Fatalf("balance changed by rejected postings: %d", got)
	}
}

func TestPostingIdempotency(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, "USD")
	in := usecase.DepositInput{IdempotencyKey: "dep-1", AccountID: acc.ID, Amount: 700}

	first, replayed, err := l.Deposit(ctx, in)
	if err != nil || replayed {
		t.Fatalf("first deposit: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := l.Deposit(ctx, in)
	if err != nil || !replayed {
		t.Fatalf("retried deposit: replayed=%v err=%v", replayed, err)
	}
	if second.ID != first.ID || len(second.Entries) != 2 {
		t.Fatalf("replay returned %+v, want transaction %s", second, first.ID)
	}
	if got := balance(t, l, acc.ID); got != 700 {
		t.Fatalf("balance = %d, want 700 (deposit applied once)", got)
	}

	in.Amount = 701
	if _, _, err := l.Deposit(ctx, in); !errors.Is(err, usecase.ErrIdempotencyKeyReused) {
		t.Fatalf("key reused with different amount: got %v", err)
	}
	if _, _, err := l.Withdraw(ctx, usecase.WithdrawInput{IdempotencyKey: "dep-1", AccountID: acc.ID, Amount: 700}); !errors.Is(err, usecase.ErrIdempotencyKeyReused) {
		t.Fatalf("key reused for a different operation: got %v", err)
	}
}

func TestFailedPostingDoesNotConsumeKey(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, "USD")
	in := usecase.WithdrawInput{IdempotencyKey: "wd-1", AccountID: acc.ID, Amount: 50}

	if _, _, err := l.Withdraw(ctx, in); !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
	mustDeposit(t, l, acc.ID, 50)

	if _, replayed, err := l.Withdraw(ctx, in); err != nil || replayed {
		t.Fatalf("retry after funding: replayed=%v err=%v", replayed, err)
	}
	if got := balance(t, l, acc.ID); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestListTransactionsPagination(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, "USD")
	other := mustCreateAccount(t, l, "USD")
	for i := int64(1); i <= 5; i++ {
		mustDeposit(t, l, acc.ID, i*100)
	}
	mustDeposit(t, l, other.ID, 1)

	var amounts []int64
	cursor := int64(0)
	pages := 0
	for {
		page, err := l.ListTransactions(ctx, usecase.ListTransactionsInput{AccountID: acc.ID, Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, line := range page.Lines {
			amounts = append(amounts, line.Amount)
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 3 || fmt.Sprint(amounts) != "[500 400 300 200 100]" {
		t.Fatalf("got %d pages with amounts %v", pages, amounts)
	}

	first, err := l.ListTransactions(ctx, usecase.ListTransactionsInput{AccountID: acc.ID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Lines[0].BalanceAfter != 1500 || first.Lines[4].BalanceAfter != 100 {
		t.Fatalf("unexpected running balances: %+v", first.Lines)
	}

	if _, err := l.ListTransactions(ctx, usecase.ListTransactionsInput{AccountID: uuid.New()}); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("unknown account: got %v", err)
	}
}
