package usecase

import (
	"context"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Ledger implements the account and money movement use cases.
type Ledger struct {
	store Store
	now   func() time.Time
}

func NewLedger(store Store) *Ledger {
	return &Ledger{
		store: store,
		// PostgreSQL stores microseconds; truncating keeps responses and
		// idempotent replays identical.
		now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) },
	}
}

type CreateAccountInput struct {
	IdempotencyKey string
	Name           string
	Currency       string
}

type DepositInput struct {
	IdempotencyKey string
	AccountID      uuid.UUID
	Amount         int64
}

type WithdrawInput struct {
	IdempotencyKey string
	AccountID      uuid.UUID
	Amount         int64
}

type TransferInput struct {
	IdempotencyKey string
	FromAccountID  uuid.UUID
	ToAccountID    uuid.UUID
	Amount         int64
}

type ListTransactionsInput struct {
	AccountID uuid.UUID
	// Cursor is the NextCursor of the previous page, or 0 for the first page.
	Cursor int64
	Limit  int
}

type TransactionPage struct {
	Lines []domain.StatementLine
	// NextCursor is 0 when there are no more pages.
	NextCursor int64
}

// CreateAccount opens a customer account owned by the caller. replayed is
// true when the caller already used the idempotency key for the same request,
// in which case the previously created account is returned.
func (l *Ledger) CreateAccount(ctx context.Context, caller Principal, in CreateAccountInput) (acc domain.Account, replayed bool, err error) {
	if err := requireCaller(caller); err != nil {
		return domain.Account{}, false, err
	}
	if err := validateIdempotencyKey(in.IdempotencyKey); err != nil {
		return domain.Account{}, false, err
	}
	currency, err := domain.ParseCurrency(in.Currency)
	if err != nil {
		return domain.Account{}, false, err
	}
	now := l.now()
	acc, err = domain.NewAccount(uuid.New(), caller.ClientID, in.Name, currency, now)
	if err != nil {
		return domain.Account{}, false, err
	}
	requestHash := fingerprint("create_account", acc.Name, acc.Currency)

	err = l.store.InTx(ctx, func(tx Store) error {
		id, fresh, err := reserveKey(ctx, tx, caller, in.IdempotencyKey, requestHash, acc.ID, now)
		if err != nil {
			return err
		}
		if !fresh {
			replayed = true
			acc, err = tx.Accounts().Get(ctx, id)
			return err
		}
		if err := tx.Accounts().Create(ctx, acc); err != nil {
			return err
		}
		if err := tx.Accounts().EnsureExternal(ctx, domain.NewExternalAccount(uuid.New(), currency, now)); err != nil {
			return err
		}
		evt, err := domain.NewAccountCreatedEvent(uuid.New(), acc)
		if err != nil {
			return err
		}
		return tx.Outbox().Add(ctx, evt)
	})
	if err != nil {
		return domain.Account{}, false, err
	}
	return acc, replayed, nil
}

// GetAccount returns a customer account including its current balance.
// Accounts the caller may not read are reported as domain.ErrAccountNotFound.
func (l *Ledger) GetAccount(ctx context.Context, caller Principal, id uuid.UUID) (domain.Account, error) {
	if err := requireCaller(caller); err != nil {
		return domain.Account{}, err
	}
	acc, err := l.store.Accounts().Get(ctx, id)
	if err != nil {
		return domain.Account{}, err
	}
	if !caller.canRead(acc) {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	return acc, nil
}

// Deposit adds money to any customer account. Only operators may deposit,
// because a deposit brings new money into the ledger.
func (l *Ledger) Deposit(ctx context.Context, caller Principal, in DepositInput) (domain.Transaction, bool, error) {
	if err := requireOperator(caller); err != nil {
		return domain.Transaction{}, false, err
	}
	requestHash := fingerprint("deposit", in.AccountID, in.Amount)
	return l.post(ctx, caller, in.IdempotencyKey, requestHash, in.Amount,
		func(ctx context.Context, tx Store, id uuid.UUID, now time.Time) (domain.Transaction, []domain.Account, error) {
			acc, ext, err := lockWithExternal(ctx, tx, in.AccountID)
			if err != nil {
				return domain.Transaction{}, nil, err
			}
			t, err := domain.Deposit(id, &acc, &ext, in.Amount, now)
			return t, []domain.Account{acc, ext}, err
		})
}

// Withdraw takes money out of any customer account. The balance can never go
// negative. Only operators may withdraw, because a withdrawal pays money out
// of the ledger.
func (l *Ledger) Withdraw(ctx context.Context, caller Principal, in WithdrawInput) (domain.Transaction, bool, error) {
	if err := requireOperator(caller); err != nil {
		return domain.Transaction{}, false, err
	}
	requestHash := fingerprint("withdraw", in.AccountID, in.Amount)
	return l.post(ctx, caller, in.IdempotencyKey, requestHash, in.Amount,
		func(ctx context.Context, tx Store, id uuid.UUID, now time.Time) (domain.Transaction, []domain.Account, error) {
			acc, ext, err := lockWithExternal(ctx, tx, in.AccountID)
			if err != nil {
				return domain.Transaction{}, nil, err
			}
			t, err := domain.Withdraw(id, &acc, &ext, in.Amount, now)
			return t, []domain.Account{acc, ext}, err
		})
}

// Transfer moves money between two accounts of the same currency. The caller
// must own the source account; this applies to operators too. The
// destination can be any customer account.
func (l *Ledger) Transfer(ctx context.Context, caller Principal, in TransferInput) (domain.Transaction, bool, error) {
	if err := requireCaller(caller); err != nil {
		return domain.Transaction{}, false, err
	}
	requestHash := fingerprint("transfer", in.FromAccountID, in.ToAccountID, in.Amount)
	return l.post(ctx, caller, in.IdempotencyKey, requestHash, in.Amount,
		func(ctx context.Context, tx Store, id uuid.UUID, now time.Time) (domain.Transaction, []domain.Account, error) {
			locked, err := tx.Accounts().GetForUpdate(ctx, in.FromAccountID, in.ToAccountID)
			if err != nil {
				return domain.Transaction{}, nil, err
			}
			from, okFrom := findAccount(locked, in.FromAccountID)
			to, okTo := findAccount(locked, in.ToAccountID)
			// Checked before any balance rule so that a foreign source
			// account looks exactly like a missing one.
			if !okFrom || !from.OwnedBy(caller.ClientID) || !okTo {
				return domain.Transaction{}, nil, domain.ErrAccountNotFound
			}
			t, err := domain.Transfer(id, &from, &to, in.Amount, now)
			return t, []domain.Account{from, to}, err
		})
}

// ListTransactions returns an account's statement, newest first, using
// keyset pagination. Accounts the caller may not read are reported as
// domain.ErrAccountNotFound.
func (l *Ledger) ListTransactions(ctx context.Context, caller Principal, in ListTransactionsInput) (TransactionPage, error) {
	if err := requireCaller(caller); err != nil {
		return TransactionPage{}, err
	}
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	before := in.Cursor
	if before <= 0 {
		before = math.MaxInt64
	}

	if _, err := l.GetAccount(ctx, caller, in.AccountID); err != nil {
		return TransactionPage{}, err
	}
	lines, err := l.store.Transactions().ListByAccount(ctx, in.AccountID, before, limit+1)
	if err != nil {
		return TransactionPage{}, err
	}
	page := TransactionPage{Lines: lines}
	if len(lines) > limit {
		page.Lines = lines[:limit]
		page.NextCursor = lines[limit-1].EntryID
	}
	return page, nil
}

// postFunc locks the accounts a posting touches, applies the domain operation
// and returns the transaction along with the updated accounts.
type postFunc func(ctx context.Context, tx Store, id uuid.UUID, now time.Time) (domain.Transaction, []domain.Account, error)

// post runs fn in a database transaction guarded by the caller's idempotency
// key and persists the result: new balances, the transaction with its entries
// and a transaction.posted outbox event all commit atomically.
//
// A replay returns the stored transaction without calling fn. That is safe
// because keys are scoped per client and the request fingerprint covers the
// account IDs, so only the caller that passed fn's checks can replay it.
func (l *Ledger) post(ctx context.Context, caller Principal, key, requestHash string, amount int64, fn postFunc) (t domain.Transaction, replayed bool, err error) {
	if err := validateIdempotencyKey(key); err != nil {
		return domain.Transaction{}, false, err
	}
	if amount <= 0 {
		return domain.Transaction{}, false, domain.ErrInvalidAmount
	}

	err = l.store.InTx(ctx, func(tx Store) error {
		now := l.now()
		id, fresh, err := reserveKey(ctx, tx, caller, key, requestHash, uuid.New(), now)
		if err != nil {
			return err
		}
		if !fresh {
			replayed = true
			t, err = tx.Transactions().Get(ctx, id)
			return err
		}

		var accounts []domain.Account
		if t, accounts, err = fn(ctx, tx, id, now); err != nil {
			return err
		}
		if err := tx.Accounts().UpdateBalances(ctx, accounts...); err != nil {
			return err
		}
		if err := tx.Transactions().Create(ctx, t); err != nil {
			return err
		}
		evt, err := domain.NewTransactionPostedEvent(uuid.New(), t)
		if err != nil {
			return err
		}
		return tx.Outbox().Add(ctx, evt)
	})
	if err != nil {
		return domain.Transaction{}, false, err
	}
	return t, replayed, nil
}

// lockWithExternal locks a customer account and then the external account of
// its currency, always in that order.
func lockWithExternal(ctx context.Context, tx Store, id uuid.UUID) (acc, ext domain.Account, err error) {
	locked, err := tx.Accounts().GetForUpdate(ctx, id)
	if err != nil {
		return acc, ext, err
	}
	acc, ok := findAccount(locked, id)
	if !ok {
		return acc, ext, domain.ErrAccountNotFound
	}
	ext, err = tx.Accounts().GetExternalForUpdate(ctx, acc.Currency)
	return acc, ext, err
}

func findAccount(accounts []domain.Account, id uuid.UUID) (domain.Account, bool) {
	for _, a := range accounts {
		if a.ID == id {
			return a, true
		}
	}
	return domain.Account{}, false
}
