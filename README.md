# go-ledger-service

[![CI](https://github.com/yohadrian21/go-ledger-service/actions/workflows/ci.yml/badge.svg)](https://github.com/yohadrian21/go-ledger-service/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/yohadrian21/go-ledger-service)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A payments ledger service in Go. It keeps balances in a double-entry ledger in PostgreSQL and exposes a REST API for opening accounts, depositing, withdrawing and transferring money. Balances can never go negative, and a request is never applied twice, even under heavy concurrency or client retries: row locks taken in a consistent order prevent double spending and deadlocks, and idempotency keys are stored atomically with each posting. Every change also writes an event to a transactional outbox, and a background worker publishes those events to RabbitMQ. The code follows Clean Architecture, uses a small set of well-known libraries, and is tested with unit tests, integration tests against real PostgreSQL and RabbitMQ, and a concurrency suite that shows there is no double spending.

## Architecture

```mermaid
flowchart LR
    client([HTTP client])

    subgraph service [go-ledger-service]
        direction LR
        http["delivery/httpapi<br/>handlers, middleware"]
        ledger["usecase.Ledger"]
        relay["usecase.OutboxRelay<br/>(background worker)"]
        domain["domain<br/>Account, Transaction, Event"]
        repo["repository/postgres"]
        pub["messaging/rabbitmq"]
    end

    pg[("PostgreSQL<br/>accounts, transactions, entries,<br/>idempotency_keys, outbox_events")]
    mq[["RabbitMQ<br/>exchange: ledger.events"]]

    client -- "REST + Idempotency-Key" --> http
    http --> ledger
    ledger --> domain
    ledger -- "Store port" --> repo
    relay -- "Store port" --> repo
    relay -- "Publisher port" --> pub
    repo --> pg
    pub --> mq
```

Dependencies point inwards. `domain` has no project imports, `usecase` depends only on `domain` and defines the ports (`Store`, `Publisher`), and the adapters (`repository`, `delivery`, `messaging`) implement or call those ports. `cmd/ledger` wires everything together.

| Layer | Package | Responsibility |
| --- | --- | --- |
| Domain | `internal/domain` | Accounts, balanced transactions, events and their rules. Pure Go, no I/O. |
| Use cases | `internal/usecase` | Posting flow, idempotency, outbox relay, port interfaces. |
| Repository | `internal/repository/postgres` | PostgreSQL implementation of the ports (pgx). |
|  | `internal/repository/memory` | In-memory implementation used by the unit tests. |
| Delivery | `internal/delivery/httpapi` | REST handlers, DTOs, error mapping, request ID, access log and panic recovery middleware. |
| Messaging | `internal/messaging/rabbitmq` | Event publisher with publisher confirms. |

This is what happens when a client posts a transfer:

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant A as API
    participant DB as PostgreSQL
    participant R as Outbox relay
    participant MQ as RabbitMQ

    C->>A: POST /v1/transfers (Idempotency-Key: k1)
    A->>DB: BEGIN
    A->>DB: INSERT key k1 into idempotency_keys (waits if k1 is in flight)
    A->>DB: SELECT accounts ... ORDER BY id FOR UPDATE
    Note over A: domain rules: same currency, enough funds
    A->>DB: UPDATE balances, INSERT transaction, entries and outbox event
    A->>DB: COMMIT
    A-->>C: 201 Created
    loop every second
        R->>DB: SELECT pending events FOR UPDATE SKIP LOCKED
        R->>MQ: publish (persistent) and wait for confirm
        R->>DB: UPDATE outbox_events SET published_at
    end
```

## Key design decisions

### Double-entry ledger with integer money

- Every movement is a transaction with at least two entries whose signed amounts sum to zero. Deposits and withdrawals use a per-currency *external* account as their counterparty, so money never appears from nowhere and the sum of all balances is always zero.
- Amounts are `int64` minor units (cents for USD), never floats. Additions are overflow-checked.
- `accounts.balance` is a cached projection of the entries and is updated in the same database transaction. Each entry also stores `balance_after`, so every statement line can be audited on its own.
- The database enforces the invariants as well: a `CHECK` constraint keeps customer balances non-negative, and a deferred constraint trigger rejects unbalanced transactions at `COMMIT`.

### Concurrency: row locks in a consistent order

- Each posting runs in a single `READ COMMITTED` transaction and locks the accounts it touches with `SELECT ... FOR UPDATE` before reading their balances. The balance check and the update therefore happen atomically, so there are no lost updates and no double spending.
- Locks are always taken in the same order: customer accounts in ascending ID order (one `ORDER BY id FOR UPDATE` query), then the external account. Two transfers in opposite directions (A to B and B to A) can therefore never deadlock.
- Explicit locks were chosen over `SERIALIZABLE` because the rows to lock are known up front, and serializable isolation would need retry loops around every posting.
- The integration tests prove this against real PostgreSQL:
  - 50 parallel transfers from an account whose balance covers only 10 of them: exactly 10 succeed and the balance ends at zero.
  - 200 random transfers in all directions between 4 accounts: no deadlocks, and the total amount of money is unchanged.
  - After each run, SQL checks that every cached balance equals the sum of its entries, that every `balance_after` matches the running total, and that every transaction is balanced.
  - Locking accounts in request order instead makes the deadlock test fail with `deadlock detected (SQLSTATE 40P01)`.

### Idempotency keys

- Every write endpoint requires an `Idempotency-Key` header. The key is inserted into `idempotency_keys` **in the same database transaction** as the posting, together with a SHA-256 fingerprint of the request and the ID of the resource it created. The key and the money movement commit or roll back together.
- A retry with the same key and the same body returns the original resource with the header `Idempotent-Replayed: true`. Reusing a key with a different body returns `422 idempotency_key_reused`.
- If duplicates arrive at the same time, the second `INSERT` waits on the primary key until the first transaction commits, then finds the key and replays the result. It can never post twice (covered by a test with 20 parallel retries).
- Failed requests (for example, insufficient funds) roll back their key too, so the client can retry with the same key once the problem is fixed.

### Transactional outbox

- `account.created` and `transaction.posted` events are written to `outbox_events` in the same transaction as the ledger change. This avoids the dual-write problem: an event exists if and only if its change was committed.
- A background relay reads pending events with `FOR UPDATE SKIP LOCKED`, so several replicas can relay in parallel. It publishes them to the durable topic exchange `ledger.events` as persistent messages, waits for publisher confirms, and then marks them as published.
- Delivery is at-least-once. Each message's `message_id` is the event ID, so consumers can deduplicate.
- If RabbitMQ is down, the API keeps working: events wait in the outbox and are published once the broker is back. For that reason, readiness depends only on PostgreSQL.

### Operations

- Structured JSON logs (`log/slog`) with a request ID on every line (the caller's `X-Request-ID` is honoured).
- All configuration comes from environment variables, and invalid values are reported together at startup.
- `GET /healthz` (liveness) and `GET /readyz` (database reachable and not shutting down).
- Graceful shutdown on `SIGTERM`: readiness starts failing, the HTTP server drains in-flight requests, and the relay finishes its current batch before connections are closed.
- Migrations are embedded in the binary and applied on startup (guarded by an advisory lock).

## Running it

Requirements: Docker with Compose. For development you also need Go 1.26+ and [golangci-lint](https://golangci-lint.run/) v2.

```bash
make up        # build and start the app, PostgreSQL and RabbitMQ
curl localhost:8080/readyz
make logs      # follow the application logs
make down      # stop everything and delete the data
```

The RabbitMQ management UI runs at <http://localhost:15672> (user `ledger`, password `ledger`). For local inspection, every event is also routed to the queue `ledger.events.audit`.

Development workflow:

```bash
make run               # run the service with `go run` against the compose dependencies
make test              # unit tests (no Docker needed)
make test-integration  # unit + integration tests against PostgreSQL and RabbitMQ
make lint              # golangci-lint
make help              # list all targets
```

## API

The full contract is in [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1).

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/v1/accounts` | Open an account (`name`, `currency`) |
| `GET` | `/v1/accounts/{id}` | Get an account and its balance |
| `POST` | `/v1/accounts/{id}/deposits` | Deposit `amount` |
| `POST` | `/v1/accounts/{id}/withdrawals` | Withdraw `amount` |
| `POST` | `/v1/transfers` | Transfer `amount` between `from_account_id` and `to_account_id` |
| `GET` | `/v1/accounts/{id}/transactions` | Account statement, newest first (`limit`, `cursor`) |
| `GET` | `/healthz`, `/readyz` | Liveness and readiness probes |

All `POST` endpoints require `Idempotency-Key`. Errors look like `{"error": {"code": "insufficient_funds", "message": "insufficient funds"}}`.

### Example

The examples use [`jq`](https://jqlang.org/) to extract IDs.

```bash
ALICE=$(curl -s -X POST localhost:8080/v1/accounts \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: create-alice' \
  -d '{"name": "Alice", "currency": "USD"}' | jq -r .id)

BOB=$(curl -s -X POST localhost:8080/v1/accounts \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: create-bob' \
  -d '{"name": "Bob", "currency": "USD"}' | jq -r .id)

# Deposit $100.00 (amounts are in cents)
curl -s -X POST localhost:8080/v1/accounts/$ALICE/deposits \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: deposit-1' \
  -d '{"amount": 10000}'

# Transfer $25.00 from Alice to Bob
curl -s -X POST localhost:8080/v1/transfers \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: transfer-1' \
  -d "{\"from_account_id\": \"$ALICE\", \"to_account_id\": \"$BOB\", \"amount\": 2500}"
```

```json
{
  "id": "a1b86210-34f1-4baf-99aa-6558ec91db14",
  "type": "transfer",
  "amount": 2500,
  "currency": "USD",
  "entries": [
    { "account_id": "fe58b00a-001b-484c-970f-5098b90e8b07", "amount": -2500, "balance_after": 7500 },
    { "account_id": "c504b36c-89e8-4c9b-b0ff-178b57fa6d64", "amount": 2500, "balance_after": 2500 }
  ],
  "created_at": "2026-11-03T08:05:34.177892Z"
}
```

Sending the same transfer again with `Idempotency-Key: transfer-1` returns the same transaction with `Idempotent-Replayed: true`, and no money moves. Trying to withdraw more than the balance fails:

```bash
curl -s -X POST localhost:8080/v1/accounts/$BOB/withdrawals \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: withdraw-1' \
  -d '{"amount": 5000}'
# {"error":{"code":"insufficient_funds","message":"insufficient funds"}}
```

Check the balance and the statement:

```bash
curl -s localhost:8080/v1/accounts/$ALICE
# {"id":"fe58b00a-...","name":"Alice","currency":"USD","balance":7500,...}

curl -s "localhost:8080/v1/accounts/$ALICE/transactions?limit=10"
# {"data":[{"transaction_id":"a1b86210-...","type":"transfer","amount":-2500,"balance_after":7500,...},
#          {"transaction_id":"b697e02c-...","type":"deposit","amount":10000,"balance_after":10000,...}],
#  "next_cursor":null}
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | *(required)* | PostgreSQL connection string |
| `DB_MAX_CONNS` | `10` | Connection pool size |
| `MIGRATE_ON_START` | `true` | Apply embedded migrations on startup |
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `AMQP_URL` | `amqp://guest:guest@localhost:5672/` | RabbitMQ connection string |
| `AMQP_EXCHANGE` | `ledger.events` | Topic exchange for events (routing key = event type) |
| `AMQP_QUEUE` | *(empty)* | Optional queue bound to all events, handy for local inspection |
| `OUTBOX_POLL_INTERVAL` | `1s` | How often the relay polls when the outbox is empty |
| `OUTBOX_BATCH_SIZE` | `100` | Events per relay batch |
| `SHUTDOWN_TIMEOUT` | `15s` | Time allowed for in-flight requests on shutdown |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

## Project layout

```text
.
├── api/openapi.yaml              # API contract
├── cmd/ledger/                   # main: wiring, graceful shutdown
├── internal/
│   ├── config/                   # environment configuration
│   ├── delivery/httpapi/         # REST handlers and middleware
│   ├── domain/                   # entities and business rules
│   ├── messaging/rabbitmq/       # outbox event publisher
│   ├── repository/
│   │   ├── memory/               # in-memory store for unit tests
│   │   └── postgres/             # PostgreSQL store, integration and concurrency tests
│   ├── testutil/pgtest/          # creates a throwaway database per test
│   └── usecase/                  # ledger use cases, idempotency, outbox relay, ports
├── migrations/                   # SQL migrations (embedded into the binary)
├── docker-compose.yml
├── Dockerfile
└── Makefile
```

## Testing

- **Unit tests** (`make test`) cover the domain rules, the use cases (with the in-memory store), the HTTP layer (status codes, error mapping, idempotent replays, pagination) and configuration.
- **Integration tests** (`make test-integration`, build tag `integration`) run against real PostgreSQL and RabbitMQ. Each test gets its own freshly migrated database, so tests are isolated and can run in parallel. They cover the repository, the database constraints, `SKIP LOCKED` behaviour, the concurrency scenarios described above, and the full outbox-to-RabbitMQ pipeline.
- CI runs lint, the full test suite with the race detector (PostgreSQL and RabbitMQ run as service containers), and builds the binary and the Docker image.

## Trade-offs and possible next steps

- **Hot external account.** Deposits and withdrawals in the same currency all lock the same external account row, which limits throughput for that currency. You could fix this by sharding the external account or by not caching its balance.
- **Key and event retention.** Idempotency keys are global and kept forever, and published outbox rows are never deleted. A production system would scope keys per client, expire them after a retention window, and clean up published events.
- **Out of scope:** authentication, rate limiting, currency conversion, metrics and tracing (OpenTelemetry), and `lock_timeout` / `statement_timeout` tuning.

## License

Released under the [MIT License](LICENSE). Copyright (c) 2026 Yohanes Adrian Risangtomo.
