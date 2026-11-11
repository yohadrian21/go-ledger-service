package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxAccountNameLength = 100

// AccountKind distinguishes customer accounts from the ledger's internal
// external accounts.
type AccountKind string

const (
	// AccountKindCustomer accounts belong to customers and can never have a
	// negative balance.
	AccountKindCustomer AccountKind = "customer"
	// AccountKindExternal accounts represent money outside the ledger (one per
	// currency). They are the counterparty of deposits and withdrawals, so
	// their balance is the negated total of customer funds in that currency.
	AccountKindExternal AccountKind = "external"
)

// Account holds a balance in a single currency. Balance is a cached
// projection of the account's ledger entries and is only changed by posting a
// Transaction.
type Account struct {
	ID   uuid.UUID
	Name string
	// OwnerID is the API client that opened the account. It never changes.
	// External accounts have no owner, and neither do customer accounts
	// opened before accounts had owners.
	OwnerID   string
	Currency  Currency
	Kind      AccountKind
	Balance   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAccount validates the input and returns a customer account owned by
// ownerID with a zero balance.
func NewAccount(id uuid.UUID, ownerID, name string, currency Currency, now time.Time) (Account, error) {
	if ownerID == "" {
		return Account{}, ErrAccountOwnerRequired
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxAccountNameLength {
		return Account{}, ErrInvalidAccountName
	}
	if !currency.Valid() {
		return Account{}, ErrInvalidCurrency
	}
	return Account{
		ID:        id,
		Name:      name,
		OwnerID:   ownerID,
		Currency:  currency,
		Kind:      AccountKindCustomer,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// OwnedBy reports whether clientID owns the account. An empty clientID never
// owns anything, including accounts without an owner.
func (a Account) OwnedBy(clientID string) bool {
	return clientID != "" && a.OwnerID == clientID
}

// NewExternalAccount returns the external account for currency.
func NewExternalAccount(id uuid.UUID, currency Currency, now time.Time) Account {
	return Account{
		ID:        id,
		Name:      "external " + string(currency),
		Currency:  currency,
		Kind:      AccountKindExternal,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// apply adds a signed amount to the balance. Customer balances may never
// become negative.
func (a *Account) apply(amount int64, now time.Time) error {
	next, err := addAmount(a.Balance, amount)
	if err != nil {
		return err
	}
	if next < 0 && a.Kind != AccountKindExternal {
		return ErrInsufficientFunds
	}
	a.Balance = next
	a.UpdatedAt = now
	return nil
}
