//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/repository/postgres"
	"github.com/yohadrian21/go-ledger-service/internal/testutil/pgtest"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

func newLedger(t *testing.T) (*usecase.Ledger, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.New(t)
	return usecase.NewLedger(postgres.NewStore(pool)), pool
}

func createAccount(t *testing.T, l *usecase.Ledger, currency string) domain.Account {
	t.Helper()
	acc, _, err := l.CreateAccount(context.Background(), usecase.CreateAccountInput{
		IdempotencyKey: uuid.NewString(), Name: "test " + currency, Currency: currency,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return acc
}

func deposit(t *testing.T, l *usecase.Ledger, id uuid.UUID, amount int64) domain.Transaction {
	t.Helper()
	tx, _, err := l.Deposit(context.Background(), usecase.DepositInput{
		IdempotencyKey: uuid.NewString(), AccountID: id, Amount: amount,
	})
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	return tx
}

func balanceOf(t *testing.T, l *usecase.Ledger, id uuid.UUID) int64 {
	t.Helper()
	acc, err := l.GetAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return acc.Balance
}

// assertLedgerConsistent checks the ledger invariants directly in the database.
func assertLedgerConsistent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	checks := []struct {
		name  string
		query string
	}{
		{
			name: "cached balance differs from the sum of entries",
			query: `SELECT count(*) FROM accounts a
			        WHERE a.balance <> (SELECT COALESCE(SUM(e.amount), 0) FROM entries e WHERE e.account_id = a.id)`,
		},
		{
			name: "balance_after differs from the running sum of entries",
			query: `SELECT count(*) FROM (
			          SELECT balance_after, SUM(amount) OVER (PARTITION BY account_id ORDER BY id) AS running
			          FROM entries
			        ) s WHERE balance_after <> running`,
		},
		{
			name: "customer balance went negative",
			query: `SELECT count(*) FROM entries e JOIN accounts a ON a.id = e.account_id
			        WHERE a.kind = 'customer' AND e.balance_after < 0`,
		},
		{
			name:  "transaction is not balanced",
			query: `SELECT count(*) FROM (SELECT 1 FROM entries GROUP BY transaction_id HAVING SUM(amount) <> 0) s`,
		},
		{
			name:  "money was created or destroyed",
			query: `SELECT count(*) FROM (SELECT SUM(balance) AS total FROM accounts) s WHERE s.total <> 0`,
		},
	}
	for _, c := range checks {
		var violations int
		if err := pool.QueryRow(context.Background(), c.query).Scan(&violations); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if violations != 0 {
			t.Errorf("ledger invariant violated (%d rows): %s", violations, c.name)
		}
	}
}
