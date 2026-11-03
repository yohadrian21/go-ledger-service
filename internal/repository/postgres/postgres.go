// Package postgres implements the usecase repositories on PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver used for migrations

	"github.com/yohadrian21/go-ledger-service/internal/usecase"
	"github.com/yohadrian21/go-ledger-service/migrations"
)

// DBTX is implemented by both *pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store implements usecase.Store. Transactions run at PostgreSQL's default
// READ COMMITTED isolation level; correctness comes from explicit row locks
// (SELECT ... FOR UPDATE) rather than from serializable retries.
type Store struct {
	db DBTX
}

var _ usecase.Store = (*Store)(nil)

func NewStore(db DBTX) *Store {
	return &Store{db: db}
}

func (s *Store) Accounts() usecase.AccountRepository         { return accountRepo{s.db} }
func (s *Store) Transactions() usecase.TransactionRepository { return transactionRepo{s.db} }
func (s *Store) Idempotency() usecase.IdempotencyRepository  { return idempotencyRepo{s.db} }
func (s *Store) Outbox() usecase.OutboxRepository            { return outboxRepo{s.db} }

func (s *Store) InTx(ctx context.Context, fn func(tx usecase.Store) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		return fn(&Store{db: tx})
	})
}

// Connect opens a connection pool and verifies that the database is reachable.
func Connect(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending schema migrations. golang-migrate holds an
// advisory lock while migrating, so concurrent instances are safe.
func Migrate(ctx context.Context, databaseURL string) (err error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	// golang-migrate does not take a context, so make sure the database is
	// reachable before handing over the connection.
	if err := db.PingContext(ctx); err != nil {
		return errors.Join(fmt.Errorf("ping database: %w", err), db.Close())
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return errors.Join(fmt.Errorf("init migration driver: %w", err), db.Close())
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return errors.Join(fmt.Errorf("init migrations: %w", err), driver.Close())
	}
	defer func() {
		srcErr, dbErr := m.Close()
		err = errors.Join(err, srcErr, dbErr)
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func isConstraintViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == constraint
}
