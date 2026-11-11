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

var (
	clientA = usecase.Principal{ClientID: "client-a", Role: usecase.RoleClient}
	clientB = usecase.Principal{ClientID: "client-b", Role: usecase.RoleClient}
	ops     = usecase.Principal{ClientID: "ops", Role: usecase.RoleOperator}
)

func newLedger(t *testing.T) (*usecase.Ledger, *memory.Store) {
	t.Helper()
	store := memory.New()
	return usecase.NewLedger(store), store
}

func mustCreateAccount(t *testing.T, l *usecase.Ledger, owner usecase.Principal, currency string) domain.Account {
	t.Helper()
	acc, _, err := l.CreateAccount(context.Background(), owner, usecase.CreateAccountInput{
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
	tx, _, err := l.Deposit(context.Background(), ops, usecase.DepositInput{
		IdempotencyKey: uuid.NewString(), AccountID: id, Amount: amount,
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	return tx
}

func balance(t *testing.T, l *usecase.Ledger, id uuid.UUID) int64 {
	t.Helper()
	acc, err := l.GetAccount(context.Background(), ops, id)
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

	acc, replayed, err := l.CreateAccount(ctx, clientA, usecase.CreateAccountInput{IdempotencyKey: "k1", Name: "Alice", Currency: "usd"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed || acc.Currency != "USD" || acc.Balance != 0 || acc.OwnerID != clientA.ClientID {
		t.Fatalf("unexpected result: %+v replayed=%v", acc, replayed)
	}

	got, err := l.GetAccount(ctx, clientA, acc.ID)
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

	first, _, err := l.CreateAccount(ctx, clientA, in)
	if err != nil {
		t.Fatal(err)
	}
	second, replayed, err := l.CreateAccount(ctx, clientA, in)
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
	if _, _, err := l.CreateAccount(ctx, clientA, in); !errors.Is(err, usecase.ErrIdempotencyKeyReused) {
		t.Fatalf("reused key: got %v", err)
	}
}

func TestCreateAccountValidation(t *testing.T) {
	l, _ := newLedger(t)
	tests := []struct {
		name   string
		caller usecase.Principal
		in     usecase.CreateAccountInput
		want   error
	}{
		{name: "missing key", caller: clientA, in: usecase.CreateAccountInput{Name: "A", Currency: "USD"}, want: usecase.ErrIdempotencyKeyMissing},
		{name: "invalid currency", caller: clientA, in: usecase.CreateAccountInput{IdempotencyKey: "k", Name: "A", Currency: "dollars"}, want: domain.ErrInvalidCurrency},
		{name: "blank name", caller: clientA, in: usecase.CreateAccountInput{IdempotencyKey: "k", Name: " ", Currency: "USD"}, want: domain.ErrInvalidAccountName},
		{name: "no caller", in: usecase.CreateAccountInput{IdempotencyKey: "k", Name: "A", Currency: "USD"}, want: usecase.ErrForbidden},
		{name: "unknown role", caller: usecase.Principal{ClientID: "x", Role: "admin"}, in: usecase.CreateAccountInput{IdempotencyKey: "k", Name: "A", Currency: "USD"}, want: usecase.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := l.CreateAccount(context.Background(), tt.caller, tt.in); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDepositWithdrawTransfer(t *testing.T) {
	ctx := context.Background()
	l, store := newLedger(t)
	alice := mustCreateAccount(t, l, clientA, "USD")
	bob := mustCreateAccount(t, l, clientB, "USD")

	mustDeposit(t, l, alice.ID, 10_000)

	tx, _, err := l.Transfer(ctx, clientA, usecase.TransferInput{
		IdempotencyKey: "t1", FromAccountID: alice.ID, ToAccountID: bob.ID, Amount: 2_500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Type != domain.TransactionTransfer || len(tx.Entries) != 2 {
		t.Fatalf("unexpected transaction: %+v", tx)
	}

	if _, _, err := l.Withdraw(ctx, ops, usecase.WithdrawInput{IdempotencyKey: "w1", AccountID: bob.ID, Amount: 1_000}); err != nil {
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
	usd := mustCreateAccount(t, l, clientA, "USD")
	usd2 := mustCreateAccount(t, l, clientA, "USD")
	eur := mustCreateAccount(t, l, clientA, "EUR")
	mustDeposit(t, l, usd.ID, 100)

	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{name: "overdraw withdrawal", want: domain.ErrInsufficientFunds, run: func() error {
			_, _, err := l.Withdraw(ctx, ops, usecase.WithdrawInput{IdempotencyKey: "a", AccountID: usd.ID, Amount: 101})
			return err
		}},
		{name: "overdraw transfer", want: domain.ErrInsufficientFunds, run: func() error {
			_, _, err := l.Transfer(ctx, clientA, usecase.TransferInput{IdempotencyKey: "b", FromAccountID: usd.ID, ToAccountID: usd2.ID, Amount: 101})
			return err
		}},
		{name: "currency mismatch", want: domain.ErrCurrencyMismatch, run: func() error {
			_, _, err := l.Transfer(ctx, clientA, usecase.TransferInput{IdempotencyKey: "c", FromAccountID: usd.ID, ToAccountID: eur.ID, Amount: 1})
			return err
		}},
		{name: "same account", want: domain.ErrSameAccount, run: func() error {
			_, _, err := l.Transfer(ctx, clientA, usecase.TransferInput{IdempotencyKey: "d", FromAccountID: usd.ID, ToAccountID: usd.ID, Amount: 1})
			return err
		}},
		{name: "unknown account", want: domain.ErrAccountNotFound, run: func() error {
			_, _, err := l.Deposit(ctx, ops, usecase.DepositInput{IdempotencyKey: "e", AccountID: uuid.New(), Amount: 1})
			return err
		}},
		{name: "unknown destination", want: domain.ErrAccountNotFound, run: func() error {
			_, _, err := l.Transfer(ctx, clientA, usecase.TransferInput{IdempotencyKey: "f", FromAccountID: usd.ID, ToAccountID: uuid.New(), Amount: 1})
			return err
		}},
		{name: "zero amount", want: domain.ErrInvalidAmount, run: func() error {
			_, _, err := l.Deposit(ctx, ops, usecase.DepositInput{IdempotencyKey: "g", AccountID: usd.ID, Amount: 0})
			return err
		}},
		{name: "missing key", want: usecase.ErrIdempotencyKeyMissing, run: func() error {
			_, _, err := l.Deposit(ctx, ops, usecase.DepositInput{AccountID: usd.ID, Amount: 1})
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
	acc := mustCreateAccount(t, l, clientA, "USD")
	in := usecase.DepositInput{IdempotencyKey: "dep-1", AccountID: acc.ID, Amount: 700}

	first, replayed, err := l.Deposit(ctx, ops, in)
	if err != nil || replayed {
		t.Fatalf("first deposit: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := l.Deposit(ctx, ops, in)
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
	if _, _, err := l.Deposit(ctx, ops, in); !errors.Is(err, usecase.ErrIdempotencyKeyReused) {
		t.Fatalf("key reused with different amount: got %v", err)
	}
	if _, _, err := l.Withdraw(ctx, ops, usecase.WithdrawInput{IdempotencyKey: "dep-1", AccountID: acc.ID, Amount: 700}); !errors.Is(err, usecase.ErrIdempotencyKeyReused) {
		t.Fatalf("key reused for a different operation: got %v", err)
	}
}

func TestIdempotencyKeysAreScopedPerClient(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	in := usecase.CreateAccountInput{IdempotencyKey: "shared-key", Name: "Alice", Currency: "USD"}

	a, replayedA, err := l.CreateAccount(ctx, clientA, in)
	if err != nil {
		t.Fatal(err)
	}
	b, replayedB, err := l.CreateAccount(ctx, clientB, in)
	if err != nil {
		t.Fatal(err)
	}
	if replayedA || replayedB || a.ID == b.ID {
		t.Fatalf("clients must not share keys: a=%s (replayed=%v) b=%s (replayed=%v)", a.ID, replayedA, b.ID, replayedB)
	}
	if a.OwnerID != clientA.ClientID || b.OwnerID != clientB.ClientID {
		t.Fatalf("owners = %q, %q", a.OwnerID, b.OwnerID)
	}

	mustDeposit(t, l, a.ID, 1_000)
	mustDeposit(t, l, b.ID, 1_000)
	transfer := func(caller usecase.Principal, from, to uuid.UUID) (domain.Transaction, bool, error) {
		return l.Transfer(ctx, caller, usecase.TransferInput{IdempotencyKey: "pay", FromAccountID: from, ToAccountID: to, Amount: 100})
	}
	ta, _, err := transfer(clientA, a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	tb, replayed, err := transfer(clientB, b.ID, a.ID)
	if err != nil || replayed || tb.ID == ta.ID {
		t.Fatalf("second client's transfer with the same key: id=%s replayed=%v err=%v", tb.ID, replayed, err)
	}
}

func TestFailedPostingDoesNotConsumeKey(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	in := usecase.WithdrawInput{IdempotencyKey: "wd-1", AccountID: acc.ID, Amount: 50}

	if _, _, err := l.Withdraw(ctx, ops, in); !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("got %v, want ErrInsufficientFunds", err)
	}
	mustDeposit(t, l, acc.ID, 50)

	if _, replayed, err := l.Withdraw(ctx, ops, in); err != nil || replayed {
		t.Fatalf("retry after funding: replayed=%v err=%v", replayed, err)
	}
	if got := balance(t, l, acc.ID); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestReadsRequireOwnershipOrOperator(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	mustDeposit(t, l, acc.ID, 100)

	for _, caller := range []usecase.Principal{clientA, ops} {
		if _, err := l.GetAccount(ctx, caller, acc.ID); err != nil {
			t.Errorf("GetAccount as %s: %v", caller.ClientID, err)
		}
		if _, err := l.ListTransactions(ctx, caller, usecase.ListTransactionsInput{AccountID: acc.ID}); err != nil {
			t.Errorf("ListTransactions as %s: %v", caller.ClientID, err)
		}
	}

	for _, caller := range []usecase.Principal{clientB, {ClientID: clientA.ClientID}, {}} {
		if _, err := l.GetAccount(ctx, caller, acc.ID); !errors.Is(err, domain.ErrAccountNotFound) && !errors.Is(err, usecase.ErrForbidden) {
			t.Errorf("GetAccount as %+v: got %v, want not found or forbidden", caller, err)
		}
		if _, err := l.ListTransactions(ctx, caller, usecase.ListTransactionsInput{AccountID: acc.ID}); !errors.Is(err, domain.ErrAccountNotFound) && !errors.Is(err, usecase.ErrForbidden) {
			t.Errorf("ListTransactions as %+v: got %v, want not found or forbidden", caller, err)
		}
	}
	if _, err := l.GetAccount(ctx, clientB, acc.ID); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("another client's account must look missing, got %v", err)
	}
}

func TestTransferRequiresOwnershipOfSource(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	a := mustCreateAccount(t, l, clientA, "USD")
	b := mustCreateAccount(t, l, clientB, "USD")
	mustDeposit(t, l, a.ID, 1_000)

	for _, caller := range []usecase.Principal{clientB, ops} {
		_, _, err := l.Transfer(ctx, caller, usecase.TransferInput{IdempotencyKey: "steal", FromAccountID: a.ID, ToAccountID: b.ID, Amount: 1})
		if !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatalf("transfer from a foreign account as %s: got %v, want ErrAccountNotFound", caller.ClientID, err)
		}
	}
	// Even an amount larger than the balance must not reveal anything about it.
	_, _, err := l.Transfer(ctx, clientB, usecase.TransferInput{IdempotencyKey: "probe", FromAccountID: a.ID, ToAccountID: b.ID, Amount: 1_000_000})
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("got %v, want ErrAccountNotFound", err)
	}
	if got := balance(t, l, a.ID); got != 1_000 {
		t.Fatalf("balance = %d, want 1000", got)
	}

	// The source owner may pay into an account owned by someone else.
	if _, _, err := l.Transfer(ctx, clientA, usecase.TransferInput{IdempotencyKey: "pay", FromAccountID: a.ID, ToAccountID: b.ID, Amount: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestDepositAndWithdrawRequireOperator(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	mustDeposit(t, l, acc.ID, 100)

	for _, caller := range []usecase.Principal{clientA, clientB, {}} {
		if _, _, err := l.Deposit(ctx, caller, usecase.DepositInput{IdempotencyKey: "d", AccountID: acc.ID, Amount: 1}); !errors.Is(err, usecase.ErrForbidden) {
			t.Errorf("deposit as %+v: got %v, want ErrForbidden", caller, err)
		}
		if _, _, err := l.Withdraw(ctx, caller, usecase.WithdrawInput{IdempotencyKey: "w", AccountID: acc.ID, Amount: 1}); !errors.Is(err, usecase.ErrForbidden) {
			t.Errorf("withdraw as %+v: got %v, want ErrForbidden", caller, err)
		}
	}
	if got := balance(t, l, acc.ID); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
}

func TestListTransactionsPagination(t *testing.T) {
	ctx := context.Background()
	l, _ := newLedger(t)
	acc := mustCreateAccount(t, l, clientA, "USD")
	other := mustCreateAccount(t, l, clientA, "USD")
	for i := int64(1); i <= 5; i++ {
		mustDeposit(t, l, acc.ID, i*100)
	}
	mustDeposit(t, l, other.ID, 1)

	var amounts []int64
	cursor := int64(0)
	pages := 0
	for {
		page, err := l.ListTransactions(ctx, clientA, usecase.ListTransactionsInput{AccountID: acc.ID, Cursor: cursor, Limit: 2})
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

	first, err := l.ListTransactions(ctx, clientA, usecase.ListTransactionsInput{AccountID: acc.ID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Lines[0].BalanceAfter != 1500 || first.Lines[4].BalanceAfter != 100 {
		t.Fatalf("unexpected running balances: %+v", first.Lines)
	}

	if _, err := l.ListTransactions(ctx, clientA, usecase.ListTransactionsInput{AccountID: uuid.New()}); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("unknown account: got %v", err)
	}
}
