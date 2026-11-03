package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
)

type transactionRepo struct {
	db DBTX
}

func (r transactionRepo) Create(ctx context.Context, t domain.Transaction) error {
	b := &pgx.Batch{}
	b.Queue(`INSERT INTO transactions (id, type, amount, currency, created_at) VALUES ($1, $2, $3, $4, $5)`,
		t.ID, t.Type, t.Amount, t.Currency, t.CreatedAt)
	for _, e := range t.Entries {
		b.Queue(`INSERT INTO entries (transaction_id, account_id, amount, balance_after) VALUES ($1, $2, $3, $4)`,
			t.ID, e.AccountID, e.Amount, e.BalanceAfter)
	}
	return r.db.SendBatch(ctx, b).Close()
}

func (r transactionRepo) Get(ctx context.Context, id uuid.UUID) (domain.Transaction, error) {
	var t domain.Transaction
	err := r.db.QueryRow(ctx,
		`SELECT id, type, amount, currency, created_at FROM transactions WHERE id = $1`, id).
		Scan(&t.ID, &t.Type, &t.Amount, &t.Currency, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Transaction{}, domain.ErrTransactionNotFound
	}
	if err != nil {
		return domain.Transaction{}, err
	}
	t.CreatedAt = t.CreatedAt.UTC()

	rows, err := r.db.Query(ctx,
		`SELECT account_id, amount, balance_after FROM entries WHERE transaction_id = $1 ORDER BY id`, id)
	if err != nil {
		return domain.Transaction{}, err
	}
	t.Entries, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Entry, error) {
		var e domain.Entry
		err := row.Scan(&e.AccountID, &e.Amount, &e.BalanceAfter)
		return e, err
	})
	return t, err
}

func (r transactionRepo) ListByAccount(ctx context.Context, accountID uuid.UUID, before int64, limit int) ([]domain.StatementLine, error) {
	rows, err := r.db.Query(ctx,
		`SELECT e.id, e.transaction_id, t.type, e.amount, e.balance_after, t.currency, t.created_at
		 FROM entries e
		 JOIN transactions t ON t.id = e.transaction_id
		 WHERE e.account_id = $1 AND e.id < $2
		 ORDER BY e.id DESC
		 LIMIT $3`, accountID, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.StatementLine, error) {
		var l domain.StatementLine
		err := row.Scan(&l.EntryID, &l.TransactionID, &l.Type, &l.Amount, &l.BalanceAfter, &l.Currency, &l.CreatedAt)
		l.CreatedAt = l.CreatedAt.UTC()
		return l, err
	})
}
