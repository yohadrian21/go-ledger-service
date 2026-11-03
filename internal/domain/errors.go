// Package domain contains the ledger's core business rules. It has no
// dependencies on transport, storage or messaging concerns.
package domain

import "errors"

var (
	ErrAccountNotFound       = errors.New("account not found")
	ErrTransactionNotFound   = errors.New("transaction not found")
	ErrInsufficientFunds     = errors.New("insufficient funds")
	ErrCurrencyMismatch      = errors.New("accounts have different currencies")
	ErrSameAccount           = errors.New("source and destination accounts must differ")
	ErrInvalidAmount         = errors.New("amount must be a positive integer in minor units")
	ErrInvalidCurrency       = errors.New("currency must be a 3-letter ISO 4217 code")
	ErrInvalidAccountName    = errors.New("account name must be 1-100 characters")
	ErrBalanceOverflow       = errors.New("balance would overflow")
	ErrUnbalancedTransaction = errors.New("transaction entries must sum to zero")
)
