package httpapi

import (
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

type createAccountRequest struct {
	Name     string `json:"name"`
	Currency string `json:"currency"`
}

type amountRequest struct {
	Amount int64 `json:"amount"`
}

type transferRequest struct {
	FromAccountID uuid.UUID `json:"from_account_id"`
	ToAccountID   uuid.UUID `json:"to_account_id"`
	Amount        int64     `json:"amount"`
}

type statusResponse struct {
	Status string `json:"status"`
}

type accountResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Currency  string    `json:"currency"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type entryResponse struct {
	AccountID    uuid.UUID `json:"account_id"`
	Amount       int64     `json:"amount"`
	BalanceAfter *int64    `json:"balance_after,omitempty"`
}

type transactionResponse struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Amount    int64           `json:"amount"`
	Currency  string          `json:"currency"`
	Entries   []entryResponse `json:"entries"`
	CreatedAt time.Time       `json:"created_at"`
}

type statementLineResponse struct {
	TransactionID uuid.UUID `json:"transaction_id"`
	Type          string    `json:"type"`
	Amount        int64     `json:"amount"`
	BalanceAfter  int64     `json:"balance_after"`
	Currency      string    `json:"currency"`
	CreatedAt     time.Time `json:"created_at"`
}

type listTransactionsResponse struct {
	Data       []statementLineResponse `json:"data"`
	NextCursor *string                 `json:"next_cursor"`
}

func toAccountResponse(a domain.Account) accountResponse {
	return accountResponse{
		ID:        a.ID,
		Name:      a.Name,
		Currency:  string(a.Currency),
		Balance:   a.Balance,
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
}

// toTransactionResponse includes an entry's balance_after only if
// showBalance allows it for the entry's account.
func toTransactionResponse(t domain.Transaction, showBalance func(accountID uuid.UUID) bool) transactionResponse {
	entries := make([]entryResponse, len(t.Entries))
	for i, e := range t.Entries {
		entries[i] = entryResponse{AccountID: e.AccountID, Amount: e.Amount}
		if showBalance(e.AccountID) {
			entries[i].BalanceAfter = &e.BalanceAfter
		}
	}
	return transactionResponse{
		ID:        t.ID,
		Type:      string(t.Type),
		Amount:    t.Amount,
		Currency:  string(t.Currency),
		Entries:   entries,
		CreatedAt: t.CreatedAt,
	}
}

func toListTransactionsResponse(p usecase.TransactionPage) listTransactionsResponse {
	resp := listTransactionsResponse{Data: make([]statementLineResponse, len(p.Lines))}
	for i, l := range p.Lines {
		resp.Data[i] = statementLineResponse{
			TransactionID: l.TransactionID,
			Type:          string(l.Type),
			Amount:        l.Amount,
			BalanceAfter:  l.BalanceAfter,
			Currency:      string(l.Currency),
			CreatedAt:     l.CreatedAt,
		}
	}
	if p.NextCursor != 0 {
		c := encodeCursor(p.NextCursor)
		resp.NextCursor = &c
	}
	return resp
}

// Cursors are opaque to clients; internally they are the last entry id seen.
func encodeCursor(entryID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(entryID, 10)))
}

func decodeCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, errors.New("cursor out of range")
	}
	return id, nil
}
