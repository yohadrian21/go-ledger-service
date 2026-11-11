// Package usecase implements the application's use cases on top of the domain
// model. It defines the ports (interfaces) that the outer layers implement.
package usecase

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
)

// AccountRepository persists accounts.
type AccountRepository interface {
	Create(ctx context.Context, a domain.Account) error
	// EnsureExternal stores the external account a unless an external account
	// for a.Currency already exists.
	EnsureExternal(ctx context.Context, a domain.Account) error
	// Get returns a customer account or domain.ErrAccountNotFound.
	Get(ctx context.Context, id uuid.UUID) (domain.Account, error)
	// GetForUpdate returns the given customer accounts and locks them until
	// the surrounding transaction ends. Rows are locked in ascending ID order,
	// so concurrent callers always acquire locks in the same order and cannot
	// deadlock. Unknown IDs are omitted from the result.
	GetForUpdate(ctx context.Context, ids ...uuid.UUID) ([]domain.Account, error)
	// GetExternalForUpdate returns and locks the external account for
	// currency. Callers must lock customer accounts first.
	GetExternalForUpdate(ctx context.Context, currency domain.Currency) (domain.Account, error)
	// UpdateBalances persists the balance of each account.
	UpdateBalances(ctx context.Context, accounts ...domain.Account) error
}

// TransactionRepository persists ledger transactions and their entries.
type TransactionRepository interface {
	Create(ctx context.Context, t domain.Transaction) error
	// Get returns a transaction or domain.ErrTransactionNotFound.
	Get(ctx context.Context, id uuid.UUID) (domain.Transaction, error)
	// ListByAccount returns up to limit statement lines of an account whose
	// EntryID is lower than before, newest first.
	ListByAccount(ctx context.Context, accountID uuid.UUID, before int64, limit int) ([]domain.StatementLine, error)
}

// IdempotencyRecord links an idempotency key to the request it was first used
// with and the resource that request created. Keys are scoped to the client
// that sent them: two clients can use the same key independently.
type IdempotencyRecord struct {
	ClientID    string
	Key         string
	RequestHash string
	ResourceID  uuid.UUID
	CreatedAt   time.Time
}

// IdempotencyRepository stores idempotency keys.
type IdempotencyRepository interface {
	// Reserve stores rec unless (rec.ClientID, rec.Key) already exists, and
	// returns the stored record and whether this call created it. If another
	// in-flight transaction holds the same key, Reserve waits for it to finish.
	Reserve(ctx context.Context, rec IdempotencyRecord) (IdempotencyRecord, bool, error)
	// DeleteExpired deletes up to limit keys created before the given time
	// and returns how many it deleted.
	DeleteExpired(ctx context.Context, createdBefore time.Time, limit int) (int, error)
}

// OutboxRepository stores events until they are published.
type OutboxRepository interface {
	Add(ctx context.Context, events ...domain.Event) error
	// FetchPending returns and locks up to limit unpublished events, oldest
	// first. Events locked by a concurrent relay are skipped.
	FetchPending(ctx context.Context, limit int) ([]domain.Event, error)
	MarkPublished(ctx context.Context, ids []uuid.UUID, at time.Time) error
	// DeletePublished deletes up to limit events published before the given
	// time and returns how many it deleted. Pending events are never deleted.
	DeletePublished(ctx context.Context, publishedBefore time.Time, limit int) (int, error)
}

// Store gives access to the repositories. The repositories of the Store
// passed to an InTx callback all share that one database transaction.
type Store interface {
	Accounts() AccountRepository
	Transactions() TransactionRepository
	Idempotency() IdempotencyRepository
	Outbox() OutboxRepository
	// InTx runs fn in a database transaction that is committed if fn returns
	// nil and rolled back otherwise.
	InTx(ctx context.Context, fn func(tx Store) error) error
}

// Publisher delivers events to a message broker. Publish must only return nil
// once the broker has durably accepted the event.
type Publisher interface {
	Publish(ctx context.Context, e domain.Event) error
}
