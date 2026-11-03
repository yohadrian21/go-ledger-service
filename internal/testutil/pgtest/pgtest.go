// Package pgtest provides throwaway PostgreSQL databases for integration tests.
package pgtest

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yohadrian21/go-ledger-service/internal/repository/postgres"
)

// New creates an empty, fully migrated database that is dropped when the
// test ends, so tests can run in parallel without sharing state.
// TEST_DATABASE_URL must point at a server where the user can create databases.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Fatal("TEST_DATABASE_URL is not set; start the dependencies with `make deps-up` or run `make test-integration`")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	name := pgx.Identifier{"ledger_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + strings.Trim(name, `"`)
	dbURL := u.String()

	if err := postgres.Migrate(dbURL); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	pool, err := postgres.Connect(ctx, dbURL, 32)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
