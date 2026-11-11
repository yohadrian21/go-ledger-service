package domain

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func customer(t *testing.T, currency Currency, balance int64) Account {
	t.Helper()
	a, err := NewAccount(uuid.New(), "owner", "test", currency, now)
	if err != nil {
		t.Fatal(err)
	}
	a.Balance = balance
	return a
}

func assertBalanced(t *testing.T, tx Transaction) {
	t.Helper()
	var sum int64
	for _, e := range tx.Entries {
		sum += e.Amount
	}
	if sum != 0 {
		t.Fatalf("entries sum to %d, want 0", sum)
	}
}

func TestNewAccount(t *testing.T) {
	a, err := NewAccount(uuid.New(), "acme", "  Alice  ", "USD", now)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Alice" || a.OwnerID != "acme" || a.Balance != 0 || a.Kind != AccountKindCustomer {
		t.Fatalf("unexpected account: %+v", a)
	}

	if _, err := NewAccount(uuid.New(), "", "Alice", "USD", now); !errors.Is(err, ErrAccountOwnerRequired) {
		t.Fatalf("missing owner: got %v", err)
	}
	if _, err := NewAccount(uuid.New(), "acme", " ", "USD", now); !errors.Is(err, ErrInvalidAccountName) {
		t.Fatalf("blank name: got %v", err)
	}
	if _, err := NewAccount(uuid.New(), "acme", strings.Repeat("x", 101), "USD", now); !errors.Is(err, ErrInvalidAccountName) {
		t.Fatalf("long name: got %v", err)
	}
	if _, err := NewAccount(uuid.New(), "acme", "Bob", "usd", now); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("lower-case currency: got %v", err)
	}
}

func TestAccountOwnedBy(t *testing.T) {
	a, err := NewAccount(uuid.New(), "acme", "Alice", "USD", now)
	if err != nil {
		t.Fatal(err)
	}
	if !a.OwnedBy("acme") || a.OwnedBy("other") || a.OwnedBy("") {
		t.Fatal("OwnedBy must match only the owning client")
	}
	if NewExternalAccount(uuid.New(), "USD", now).OwnedBy("") {
		t.Fatal("an empty client id must not own an account without owner")
	}
}

func TestTransfer(t *testing.T) {
	from := customer(t, "USD", 1000)
	to := customer(t, "USD", 50)

	tx, err := Transfer(uuid.New(), &from, &to, 300, now)
	if err != nil {
		t.Fatal(err)
	}
	if from.Balance != 700 || to.Balance != 350 {
		t.Fatalf("balances = %d/%d, want 700/350", from.Balance, to.Balance)
	}
	assertBalanced(t, tx)
	want := []Entry{
		{AccountID: from.ID, Amount: -300, BalanceAfter: 700},
		{AccountID: to.ID, Amount: 300, BalanceAfter: 350},
	}
	for i, e := range want {
		if tx.Entries[i] != e {
			t.Fatalf("entry %d = %+v, want %+v", i, tx.Entries[i], e)
		}
	}
	if tx.Type != TransactionTransfer || tx.Amount != 300 || tx.Currency != "USD" {
		t.Fatalf("unexpected transaction: %+v", tx)
	}
}

func TestTransferRejections(t *testing.T) {
	usd := func(balance int64) Account { return customer(t, "USD", balance) }
	external := NewExternalAccount(uuid.New(), "USD", now)
	maxed := usd(math.MaxInt64)

	tests := []struct {
		name     string
		from, to Account
		amount   int64
		want     error
	}{
		{name: "insufficient funds", from: usd(100), to: usd(0), amount: 101, want: ErrInsufficientFunds},
		{name: "zero amount", from: usd(100), to: usd(0), amount: 0, want: ErrInvalidAmount},
		{name: "negative amount", from: usd(100), to: usd(0), amount: -5, want: ErrInvalidAmount},
		{name: "same account", from: maxed, to: maxed, amount: 1, want: ErrSameAccount},
		{name: "currency mismatch", from: usd(100), to: customer(t, "EUR", 0), amount: 1, want: ErrCurrencyMismatch},
		{name: "balance overflow", from: usd(100), to: usd(math.MaxInt64), amount: 1, want: ErrBalanceOverflow},
		{name: "external source", from: external, to: usd(0), amount: 1, want: ErrAccountNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to := tt.from, tt.to
			_, err := Transfer(uuid.New(), &from, &to, tt.amount, now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if from != tt.from || to != tt.to {
				t.Fatal("accounts must be unchanged after a failed transfer")
			}
		})
	}
}

func TestDepositAndWithdraw(t *testing.T) {
	acc := customer(t, "USD", 0)
	ext := NewExternalAccount(uuid.New(), "USD", now)

	dep, err := Deposit(uuid.New(), &acc, &ext, 500, now)
	if err != nil {
		t.Fatal(err)
	}
	assertBalanced(t, dep)
	if acc.Balance != 500 || ext.Balance != -500 {
		t.Fatalf("after deposit: account=%d external=%d", acc.Balance, ext.Balance)
	}

	wd, err := Withdraw(uuid.New(), &acc, &ext, 200, now)
	if err != nil {
		t.Fatal(err)
	}
	assertBalanced(t, wd)
	if acc.Balance != 300 || ext.Balance != -300 {
		t.Fatalf("after withdrawal: account=%d external=%d", acc.Balance, ext.Balance)
	}

	if _, err := Withdraw(uuid.New(), &acc, &ext, 301, now); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overdraw: got %v", err)
	}
	if acc.Balance != 300 {
		t.Fatalf("balance changed by failed withdrawal: %d", acc.Balance)
	}

	other := customer(t, "USD", 0)
	if _, err := Deposit(uuid.New(), &acc, &other, 1, now); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("deposit from customer account: got %v", err)
	}
}

func TestTransactionValidate(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	tests := []struct {
		name    string
		tx      Transaction
		wantErr error
	}{
		{name: "balanced", tx: Transaction{Amount: 5, Entries: []Entry{{AccountID: a, Amount: -5}, {AccountID: b, Amount: 5}}}},
		{name: "unbalanced", tx: Transaction{Amount: 5, Entries: []Entry{{AccountID: a, Amount: -5}, {AccountID: b, Amount: 4}}}, wantErr: ErrUnbalancedTransaction},
		{name: "single entry", tx: Transaction{Amount: 5, Entries: []Entry{{AccountID: a, Amount: 5}}}, wantErr: ErrUnbalancedTransaction},
		{name: "zero entry", tx: Transaction{Amount: 5, Entries: []Entry{{AccountID: a}, {AccountID: b}}}, wantErr: ErrUnbalancedTransaction},
		{name: "invalid amount", tx: Transaction{Amount: 0}, wantErr: ErrInvalidAmount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.tx.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestTransactionPostedEvent(t *testing.T) {
	from := customer(t, "USD", 100)
	to := customer(t, "USD", 0)
	tx, err := Transfer(uuid.New(), &from, &to, 40, now)
	if err != nil {
		t.Fatal(err)
	}

	evt, err := NewTransactionPostedEvent(uuid.New(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if evt.Type != EventTransactionPosted || !evt.OccurredAt.Equal(now) {
		t.Fatalf("unexpected event: %+v", evt)
	}

	var payload struct {
		TransactionID uuid.UUID `json:"transaction_id"`
		Amount        int64     `json:"amount"`
		Entries       []struct {
			AccountID    uuid.UUID `json:"account_id"`
			Amount       int64     `json:"amount"`
			BalanceAfter int64     `json:"balance_after"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TransactionID != tx.ID || payload.Amount != 40 || len(payload.Entries) != 2 {
		t.Fatalf("unexpected payload: %s", evt.Payload)
	}
	if payload.Entries[0].AccountID != from.ID || payload.Entries[0].BalanceAfter != 60 {
		t.Fatalf("unexpected debit entry: %+v", payload.Entries[0])
	}
}
