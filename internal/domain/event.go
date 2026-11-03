package domain

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event types published to the message broker. They double as routing keys.
const (
	EventAccountCreated    = "account.created"
	EventTransactionPosted = "transaction.posted"
)

// Event is a domain event stored in the transactional outbox and later
// published to the broker. Payload is JSON.
type Event struct {
	ID         uuid.UUID
	Type       string
	OccurredAt time.Time
	Payload    []byte
}

type accountCreatedPayload struct {
	AccountID uuid.UUID `json:"account_id"`
	Name      string    `json:"name"`
	Currency  Currency  `json:"currency"`
	CreatedAt time.Time `json:"created_at"`
}

type entryPayload struct {
	AccountID    uuid.UUID `json:"account_id"`
	Amount       int64     `json:"amount"`
	BalanceAfter int64     `json:"balance_after"`
}

type transactionPostedPayload struct {
	TransactionID uuid.UUID       `json:"transaction_id"`
	Type          TransactionType `json:"type"`
	Amount        int64           `json:"amount"`
	Currency      Currency        `json:"currency"`
	Entries       []entryPayload  `json:"entries"`
	CreatedAt     time.Time       `json:"created_at"`
}

// NewAccountCreatedEvent describes a newly opened customer account.
func NewAccountCreatedEvent(id uuid.UUID, a Account) (Event, error) {
	return newEvent(id, EventAccountCreated, a.CreatedAt, accountCreatedPayload{
		AccountID: a.ID,
		Name:      a.Name,
		Currency:  a.Currency,
		CreatedAt: a.CreatedAt,
	})
}

// NewTransactionPostedEvent describes a posted ledger transaction.
func NewTransactionPostedEvent(id uuid.UUID, t Transaction) (Event, error) {
	entries := make([]entryPayload, len(t.Entries))
	for i, e := range t.Entries {
		entries[i] = entryPayload(e)
	}
	return newEvent(id, EventTransactionPosted, t.CreatedAt, transactionPostedPayload{
		TransactionID: t.ID,
		Type:          t.Type,
		Amount:        t.Amount,
		Currency:      t.Currency,
		Entries:       entries,
		CreatedAt:     t.CreatedAt,
	})
}

func newEvent(id uuid.UUID, typ string, at time.Time, payload any) (Event, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, Type: typ, OccurredAt: at, Payload: b}, nil
}
