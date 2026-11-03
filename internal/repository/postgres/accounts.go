package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
)

const accountColumns = `id, name, currency, kind, balance, created_at, updated_at`

type accountRepo struct {
	db DBTX
}

func scanAccount(row pgx.Row) (domain.Account, error) {
	var a domain.Account
	err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.Kind, &a.Balance, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrAccountNotFound
	}
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	return a, err
}

func (r accountRepo) Create(ctx context.Context, a domain.Account) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO accounts (`+accountColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		a.ID, a.Name, a.Currency, a.Kind, a.Balance, a.CreatedAt, a.UpdatedAt)
	return err
}

func (r accountRepo) EnsureExternal(ctx context.Context, a domain.Account) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO accounts (`+accountColumns+`) VALUES ($1, $2, $3, 'external', $4, $5, $6)
		 ON CONFLICT (currency) WHERE kind = 'external' DO NOTHING`,
		a.ID, a.Name, a.Currency, a.Balance, a.CreatedAt, a.UpdatedAt)
	return err
}

func (r accountRepo) Get(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	return scanAccount(r.db.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = $1 AND kind = 'customer'`, id))
}

func (r accountRepo) GetForUpdate(ctx context.Context, ids ...uuid.UUID) ([]domain.Account, error) {
	// PostgreSQL sorts before it applies the locking clause, so rows are
	// locked one by one in ascending id order. Every transaction therefore
	// acquires its locks in the same global order, which rules out deadlocks.
	rows, err := r.db.Query(ctx,
		`SELECT `+accountColumns+` FROM accounts
		 WHERE id = ANY($1) AND kind = 'customer'
		 ORDER BY id
		 FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Account, error) {
		return scanAccount(row)
	})
}

func (r accountRepo) GetExternalForUpdate(ctx context.Context, currency domain.Currency) (domain.Account, error) {
	return scanAccount(r.db.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts
		 WHERE kind = 'external' AND currency = $1
		 FOR UPDATE`, currency))
}

func (r accountRepo) UpdateBalances(ctx context.Context, accounts ...domain.Account) error {
	b := &pgx.Batch{}
	for _, a := range accounts {
		b.Queue(`UPDATE accounts SET balance = $2, updated_at = $3 WHERE id = $1`, a.ID, a.Balance, a.UpdatedAt).
			Exec(func(tag pgconn.CommandTag) error {
				if tag.RowsAffected() != 1 {
					return domain.ErrAccountNotFound
				}
				return nil
			})
	}
	err := r.db.SendBatch(ctx, b).Close()
	if isConstraintViolation(err, "accounts_balance_non_negative") {
		return domain.ErrInsufficientFunds
	}
	return err
}
