package domain

import (
	"time"

	"github.com/google/uuid"
)

// TransactionType describes why money moved.
type TransactionType string

const (
	TransactionDeposit    TransactionType = "deposit"
	TransactionWithdrawal TransactionType = "withdrawal"
	TransactionTransfer   TransactionType = "transfer"
)

// Entry is one leg of a double-entry transaction. Amount is signed from the
// account's point of view: positive amounts increase the balance, negative
// amounts decrease it. BalanceAfter is the account balance right after the
// entry was applied, which makes every account statement self-auditing.
type Entry struct {
	AccountID    uuid.UUID
	Amount       int64
	BalanceAfter int64
}

// Transaction is an immutable, balanced set of entries: the amounts of its
// entries always sum to zero, so money is never created or destroyed.
type Transaction struct {
	ID        uuid.UUID
	Type      TransactionType
	Amount    int64
	Currency  Currency
	Entries   []Entry
	CreatedAt time.Time
}

// StatementLine is a single entry as seen on one account's statement.
type StatementLine struct {
	EntryID       int64
	TransactionID uuid.UUID
	Type          TransactionType
	Amount        int64
	BalanceAfter  int64
	Currency      Currency
	CreatedAt     time.Time
}

// Validate checks the double-entry invariant.
func (t Transaction) Validate() error {
	if t.Amount <= 0 {
		return ErrInvalidAmount
	}
	if len(t.Entries) < 2 {
		return ErrUnbalancedTransaction
	}
	var sum int64
	for _, e := range t.Entries {
		if e.Amount == 0 {
			return ErrUnbalancedTransaction
		}
		var err error
		if sum, err = addAmount(sum, e.Amount); err != nil {
			return err
		}
	}
	if sum != 0 {
		return ErrUnbalancedTransaction
	}
	return nil
}

// Deposit moves amount from the external account into a customer account.
func Deposit(id uuid.UUID, account, external *Account, amount int64, now time.Time) (Transaction, error) {
	if account.Kind != AccountKindCustomer || external.Kind != AccountKindExternal {
		return Transaction{}, ErrAccountNotFound
	}
	return post(id, TransactionDeposit, external, account, amount, now)
}

// Withdraw moves amount from a customer account to the external account.
func Withdraw(id uuid.UUID, account, external *Account, amount int64, now time.Time) (Transaction, error) {
	if account.Kind != AccountKindCustomer || external.Kind != AccountKindExternal {
		return Transaction{}, ErrAccountNotFound
	}
	return post(id, TransactionWithdrawal, account, external, amount, now)
}

// Transfer moves amount between two customer accounts.
func Transfer(id uuid.UUID, from, to *Account, amount int64, now time.Time) (Transaction, error) {
	if from.Kind != AccountKindCustomer || to.Kind != AccountKindCustomer {
		return Transaction{}, ErrAccountNotFound
	}
	return post(id, TransactionTransfer, from, to, amount, now)
}

// post debits one account and credits the other by amount. The accounts are
// only modified if the whole posting succeeds. Callers must hold a lock on
// both accounts until the transaction is persisted.
func post(id uuid.UUID, typ TransactionType, debit, credit *Account, amount int64, now time.Time) (Transaction, error) {
	if amount <= 0 {
		return Transaction{}, ErrInvalidAmount
	}
	if debit.ID == credit.ID {
		return Transaction{}, ErrSameAccount
	}
	if debit.Currency != credit.Currency {
		return Transaction{}, ErrCurrencyMismatch
	}

	d, c := *debit, *credit
	if err := d.apply(-amount, now); err != nil {
		return Transaction{}, err
	}
	if err := c.apply(amount, now); err != nil {
		return Transaction{}, err
	}

	tx := Transaction{
		ID:       id,
		Type:     typ,
		Amount:   amount,
		Currency: debit.Currency,
		Entries: []Entry{
			{AccountID: d.ID, Amount: -amount, BalanceAfter: d.Balance},
			{AccountID: c.ID, Amount: amount, BalanceAfter: c.Balance},
		},
		CreatedAt: now,
	}
	if err := tx.Validate(); err != nil {
		return Transaction{}, err
	}
	*debit, *credit = d, c
	return tx, nil
}
