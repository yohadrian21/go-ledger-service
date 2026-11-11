# go-ledger-service

[![CI](https://github.com/yohadrian21/go-ledger-service/actions/workflows/ci.yml/badge.svg)](https://github.com/yohadrian21/go-ledger-service/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/yohadrian21/go-ledger-service)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A payments ledger service in Go. It keeps balances in a double-entry ledger in PostgreSQL and exposes a REST API for opening accounts, depositing, withdrawing and transferring money. Balances can never go negative, and a request is never applied twice, even under heavy concurrency or client retries: row locks taken in a consistent order prevent double spending and deadlocks, and idempotency keys are stored atomically with each posting. Every change also writes an event to a transactional outbox, and a background worker publishes those events to RabbitMQ. Every API call is authenticated with an API key: clients can only see and spend from their own accounts, and only trusted operators can deposit or withdraw. The code follows Clean Architecture, uses a small set of well-known libraries, and is tested with unit tests, integration tests against real PostgreSQL and RabbitMQ, and a concurrency suite that shows there is no double spending.

## Architecture

```mermaid
flowchart LR
    client([HTTP client])

    subgraph service [go-ledger-service]
        direction LR
        http["delivery/httpapi<br/>auth, rate limit, handlers"]
        ledger["usecase.Ledger"]
        relay["usecase.OutboxRelay<br/>(background worker)"]
        domain["domain<br/>Account, Transaction, Event"]
        repo["repository/postgres"]
        pub["messaging/rabbitmq"]
    end

    pg[("PostgreSQL<br/>accounts, transactions, entries,<br/>idempotency_keys, outbox_events")]
    mq[["RabbitMQ<br/>exchange: ledger.events"]]

    client -- "REST + API key + Idempotency-Key" --> http
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
| Use cases | `internal/usecase` | Posting flow, authorization rules, idempotency, outbox relay, retention job, port interfaces. |
| Repository | `internal/repository/postgres` | PostgreSQL implementation of the ports (pgx). |
|  | `internal/repository/memory` | In-memory implementation used by the unit tests. |
| Delivery | `internal/delivery/httpapi` | REST handlers, DTOs, error mapping, authentication, rate limiting, request ID, access log and panic recovery middleware. |
| Auth | `internal/auth` | API key verification against SHA-256 digests. |
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

    C->>A: POST /v1/transfers (API key, Idempotency-Key: k1)
    Note over A: authenticate the key, apply the client's rate limit
    A->>DB: BEGIN
    A->>DB: INSERT (client, k1) into idempotency_keys (waits if in flight)
    A->>DB: SELECT accounts ... ORDER BY id FOR UPDATE
    Note over A: caller owns the source account,<br/>same currency, enough funds
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
- Keys belong to the authenticated client: the primary key is `(client_id, key)`. Two clients can use the same key without colliding, and a client can never claim or replay another client's key.
- A retry with the same key and the same body returns the original resource with the header `Idempotent-Replayed: true`. Reusing a key with a different body returns `422 idempotency_key_reused`.
- If duplicates arrive at the same time, the second `INSERT` waits on the primary key until the first transaction commits, then finds the key and replays the result. It can never post twice (covered by a test with 20 parallel retries).
- Failed requests (for example, insufficient funds) roll back their key too, so the client can retry with the same key once the problem is fixed.
- Keys expire after `IDEMPOTENCY_KEY_TTL` (24 hours by default, at least 1 hour). A background job deletes expired keys every `CLEANUP_INTERVAL`, in batches of 1000 with `FOR UPDATE SKIP LOCKED`, so it skips rows that are in use instead of waiting for them. After the TTL, a request with the same key is treated as a new request, so clients must finish retrying within the TTL.

### Transactional outbox

- `account.created` and `transaction.posted` events are written to `outbox_events` in the same transaction as the ledger change. This avoids the dual-write problem: an event exists if and only if its change was committed.
- A background relay reads pending events with `FOR UPDATE SKIP LOCKED`, so several replicas can relay in parallel. It publishes them to the durable topic exchange `ledger.events` as persistent messages, waits for publisher confirms, and then marks them as published.
- Delivery is at-least-once. Each message's `message_id` is the event ID, so consumers can deduplicate.
- If RabbitMQ is down, the API keeps working: events wait in the outbox and are published once the broker is back. For that reason, readiness depends only on PostgreSQL.
- The retention job also deletes events that were published more than `OUTBOX_RETENTION` ago (7 days by default). Pending events are never deleted. Ledger tables (accounts, transactions, entries) are never pruned.

### Operations

- Structured JSON logs (`log/slog`) with a request ID on every line (the caller's `X-Request-ID` is honoured). The access log records the authenticated `client_id`, never the API key.
- All configuration comes from environment variables, and invalid values are reported together at startup.
- `GET /healthz` (liveness) and `GET /readyz` (database reachable and not shutting down).
- Graceful shutdown on `SIGTERM`: readiness starts failing, the HTTP server drains in-flight requests, and the relay finishes its current batch before connections are closed.
- Migrations are embedded in the binary and applied on startup (guarded by an advisory lock).

## Authentication and authorization

Every route under `/v1/` requires an API key, sent as a bearer token: `Authorization: Bearer lk_...`. A request without a valid key gets `401 unauthorized` with a `WWW-Authenticate: Bearer` header. Only the `/healthz` and `/readyz` probes are open.

### API keys

API keys fit this service: its clients are a small number of backend systems, and keys need no identity provider at runtime. Verification sits behind the `httpapi.Authenticator` interface. To use OAuth2 client credentials instead, implement that interface: validate the access token against your identity provider (for example a JWT checked against its JWKS), then map the client ID and scope to a principal. The use cases don't change.

- A key is `lk_` followed by 256 random bits, base64url-encoded. The service never stores keys. `AUTH_API_KEYS` holds only their SHA-256 digests, together with the client ID and role each key grants:

  ```text
  AUTH_API_KEYS=client_id:role:sha256_hex[,client_id:role:sha256_hex...]
  ```

  Because keys are long random values, a fast hash is enough: a leaked digest can't be turned back into a working key. Each presented key is compared against every digest in constant time.
- Generate a key with `go run ./cmd/apikey -client <client_id> -role <client|operator>`. It prints the key on the first line and the `AUTH_API_KEYS` entry on the second. Give the key to the client over a secure channel and keep the entry in your secret store. The key isn't saved anywhere, so if it's lost, generate a new one.
- **Rotation:** a client can have several keys. Add the new entry, redeploy, switch the client over, then remove the old entry.
- **Revocation:** remove the entry and redeploy.
- Keys are bearer credentials. Only send them over TLS: terminate TLS in front of the service, which serves plain HTTP. Keys never appear in logs.

### Roles and ownership

| Role | Can do |
| --- | --- |
| `client` | Open accounts, which it then owns. Read its own accounts and statements. Transfer from its own accounts to any account in the same currency. |
| `operator` | Everything a client can do. Also deposit and withdraw, the only operations that move money into or out of the ledger, and read any account and statement. Intended for trusted back-office or payment-rail integrations. |

- Each account records the client that opened it (`accounts.owner_id`). An account that belongs to another client returns `404 account_not_found`, exactly as if it didn't exist, so account IDs can't be probed. This applies to `GET /v1/accounts/{id}`, the statement, and the `from_account_id` of a transfer.
- Transfers always need the source account owner's key. Operators don't get an exception, so they can't move money out of customer accounts. The destination account can belong to anyone. The transfer response leaves out the destination's `balance_after`, so the payer never sees the payee's balance.
- A deposit or withdrawal without the operator role returns `403 forbidden`, and nothing is posted.
- The checks run in the use case layer. A transfer checks ownership inside its posting transaction, after locking the rows and alongside the balance rules. The money-safety invariants above are unchanged.
- Accounts opened before ownership was added (migration `000002`) have no owner, so only operators can read them. To give one to a client, set its `owner_id`.

### Rate limiting

Every `/v1` request uses up a token from a token bucket. The bucket refills at `RATE_LIMIT_RPS` (20 per second by default) and holds at most `RATE_LIMIT_BURST` tokens (40). Authenticated requests are limited per client. Requests that fail authentication are limited per remote address, which slows down key guessing without affecting valid clients that share that address. When a bucket is empty, the request gets `429 rate_limited` with a `Retry-After` header. `X-Forwarded-For` isn't trusted, so behind a proxy the remote address is the proxy's.

## Running it

Requirements: Docker with Compose. For development you also need Go 1.26+ and [golangci-lint](https://golangci-lint.run/) v2.

```bash
make dev-env               # once: write a gitignored .env with a local operator key and client key
make up                    # build and start the app, PostgreSQL and RabbitMQ
set -a; . ./.env; set +a   # load DEV_OPERATOR_API_KEY and DEV_CLIENT_API_KEY into the shell
curl localhost:8080/readyz
make logs                  # follow the application logs
make down                  # stop everything and delete the data
```

The compose stack is for local development only. It publishes every port on `127.0.0.1`, uses the throwaway credentials `ledger`/`ledger`, and runs the app with `APP_ENV=development`. The RabbitMQ management UI runs at <http://localhost:15672> (user `ledger`, password `ledger`). For local inspection, every event is also routed to the queue `ledger.events.audit`.

### Deploying beyond your machine

- `APP_ENV` defaults to `production`. Outside `development`, the service refuses to start unless `AMQP_URL` is set explicitly. It also rejects the RabbitMQ `guest` account and the `guest` and `ledger` default passwords in `AMQP_URL` and `DATABASE_URL`.
- Keep `AUTH_API_KEYS`, `DATABASE_URL` and `AMQP_URL` in a secret store. Never commit them.
- Encrypt connections to dependencies: use `sslmode=verify-full` for PostgreSQL and `amqps://` for RabbitMQ. Don't publish the database or broker ports, or the RabbitMQ management UI, to untrusted networks.
- Terminate TLS in front of the API, and add gateway-level rate limiting if you need one limit across replicas.

Development workflow:

```bash
make run               # run the service with `go run` against the compose dependencies (uses .env)
make test              # unit tests (no Docker needed)
make test-integration  # unit + integration tests against PostgreSQL and RabbitMQ
make lint              # golangci-lint
make help              # list all targets
```

## API

The full contract is in [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1).

| Method | Path | Who may call it | Description |
| --- | --- | --- | --- |
| `POST` | `/v1/accounts` | any client | Open an account (`name`, `currency`), owned by the caller |
| `GET` | `/v1/accounts/{id}` | owner or operator | Get an account and its balance |
| `POST` | `/v1/accounts/{id}/deposits` | operator | Deposit `amount` |
| `POST` | `/v1/accounts/{id}/withdrawals` | operator | Withdraw `amount` |
| `POST` | `/v1/transfers` | owner of `from_account_id` | Transfer `amount` between `from_account_id` and `to_account_id` |
| `GET` | `/v1/accounts/{id}/transactions` | owner or operator | Account statement, newest first (`limit`, `cursor`) |
| `GET` | `/healthz`, `/readyz` | anyone (no key) | Liveness and readiness probes |

All `/v1` endpoints require `Authorization: Bearer <api key>`, and all `POST` endpoints require `Idempotency-Key`. Errors look like `{"error": {"code": "insufficient_funds", "message": "insufficient funds"}}`. Authentication and authorization errors are `401 unauthorized`, `403 forbidden`, `404 account_not_found` (also returned for accounts the caller can't access) and `429 rate_limited`. Request bodies that fail to parse get a generic `400 invalid_request`, without parser details.

### Example

The examples use [`jq`](https://jqlang.org/) to extract IDs, and the keys from `make dev-env`. The client key opens the accounts and makes the transfer. The operator key funds the account and handles withdrawals.

```bash
set -a; . ./.env; set +a
CLIENT="Authorization: Bearer $DEV_CLIENT_API_KEY"
OPERATOR="Authorization: Bearer $DEV_OPERATOR_API_KEY"

ALICE=$(curl -s -X POST localhost:8080/v1/accounts -H "$CLIENT" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: create-alice' \
  -d '{"name": "Alice", "currency": "USD"}' | jq -r .id)

BOB=$(curl -s -X POST localhost:8080/v1/accounts -H "$CLIENT" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: create-bob' \
  -d '{"name": "Bob", "currency": "USD"}' | jq -r .id)

# Deposit $100.00 (amounts are in cents). Only operators can deposit.
curl -s -X POST localhost:8080/v1/accounts/$ALICE/deposits -H "$OPERATOR" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: deposit-1' \
  -d '{"amount": 10000}'

# Transfer $25.00 from Alice to Bob
curl -s -X POST localhost:8080/v1/transfers -H "$CLIENT" \
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
    { "account_id": "c504b36c-89e8-4c9b-b0ff-178b57fa6d64", "amount": 2500 }
  ],
  "created_at": "2026-11-03T08:05:34.177892Z"
}
```

The destination entry has no `balance_after`, because the payer must not learn the payee's balance. Sending the same transfer again with `Idempotency-Key: transfer-1` returns the same transaction with `Idempotent-Replayed: true`, and no money moves. Trying to withdraw more than the balance fails:

```bash
curl -s -X POST localhost:8080/v1/accounts/$BOB/withdrawals -H "$OPERATOR" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: withdraw-1' \
  -d '{"amount": 5000}'
# {"error":{"code":"insufficient_funds","message":"insufficient funds"}}
```

A client key can't withdraw (`403 forbidden`), and a request without a key gets `401 unauthorized`. Check the balance and the statement:

```bash
curl -s localhost:8080/v1/accounts/$ALICE -H "$CLIENT"
# {"id":"fe58b00a-...","name":"Alice","currency":"USD","balance":7500,...}

curl -s "localhost:8080/v1/accounts/$ALICE/transactions?limit=10" -H "$CLIENT"
# {"data":[{"transaction_id":"a1b86210-...","type":"transfer","amount":-2500,"balance_after":7500,...},
#          {"transaction_id":"b697e02c-...","type":"deposit","amount":10000,"balance_after":10000,...}],
#  "next_cursor":null}
```

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `APP_ENV` | `production` | `development`, `staging` or `production`. Only `development` allows a default `AMQP_URL` and default credentials |
| `AUTH_API_KEYS` | *(required)* | API key digests: `client_id:role:sha256_hex`, comma-separated (see [API keys](#api-keys)) |
| `RATE_LIMIT_RPS` | `20` | Requests per second allowed per client (and per remote address for failed authentication) |
| `RATE_LIMIT_BURST` | `40` | Bucket size, the largest burst allowed |
| `DATABASE_URL` | *(required)* | PostgreSQL connection string |
| `DB_MAX_CONNS` | `10` | Connection pool size |
| `MIGRATE_ON_START` | `true` | Apply embedded migrations on startup |
| `HTTP_ADDR` | `:8080` | HTTP listen address |
| `AMQP_URL` | *(required)* | RabbitMQ connection string. Defaults to `amqp://guest:guest@localhost:5672/` only when `APP_ENV=development` |
| `AMQP_EXCHANGE` | `ledger.events` | Topic exchange for events (routing key = event type) |
| `AMQP_QUEUE` | *(empty)* | Optional queue bound to all events, handy for local inspection |
| `OUTBOX_POLL_INTERVAL` | `1s` | How often the relay polls when the outbox is empty |
| `OUTBOX_BATCH_SIZE` | `100` | Events per relay batch |
| `IDEMPOTENCY_KEY_TTL` | `24h` | How long idempotency keys are kept (at least `1h`) |
| `OUTBOX_RETENTION` | `168h` | How long published outbox events are kept |
| `CLEANUP_INTERVAL` | `15m` | How often the retention job runs |
| `SHUTDOWN_TIMEOUT` | `15s` | Time allowed for in-flight requests on shutdown |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

## Project layout

```text
.
├── api/openapi.yaml              # API contract
├── cmd/
│   ├── apikey/                   # generates an API key and its AUTH_API_KEYS entry
│   └── ledger/                   # main: wiring, graceful shutdown
├── internal/
│   ├── auth/                     # API key verification
│   ├── config/                   # environment configuration
│   ├── delivery/httpapi/         # REST handlers, authentication, rate limiting, middleware
│   ├── domain/                   # entities and business rules
│   ├── messaging/rabbitmq/       # outbox event publisher
│   ├── repository/
│   │   ├── memory/               # in-memory store for unit tests
│   │   └── postgres/             # PostgreSQL store, integration and concurrency tests
│   ├── testutil/pgtest/          # creates a throwaway database per test
│   └── usecase/                  # ledger use cases, authorization, idempotency, outbox relay, retention, ports
├── migrations/                   # SQL migrations (embedded into the binary)
├── docker-compose.yml
├── Dockerfile
└── Makefile
```

## Testing

- **Unit tests** (`make test`) cover the domain rules, the use cases (with the in-memory store), the HTTP layer and configuration. The HTTP tests check that every `/v1` route rejects missing and invalid keys, that a client can't read, spend from or list another client's account, that deposits and withdrawals are operator-only, that idempotency keys are separate per client, and the rate limiter. They also cover status codes, error mapping, idempotent replays and pagination. API keys used in tests are generated at runtime.
- **Integration tests** (`make test-integration`, build tag `integration`) run against real PostgreSQL and RabbitMQ. Each test gets its own freshly migrated database, so tests are isolated and can run in parallel. They cover the repository, the database constraints (including account ownership and per-client idempotency keys), retention, `SKIP LOCKED` behaviour, the concurrency scenarios described above, and the full outbox-to-RabbitMQ pipeline.
- CI runs lint, the full test suite with the race detector (PostgreSQL and RabbitMQ run as service containers), and builds the binary and the Docker image.

## Trade-offs and possible next steps

- **Hot external account.** Deposits and withdrawals in the same currency all lock the same external account row, which limits throughput for that currency. You could fix this by sharding the external account or by not caching its balance.
- **Static API keys.** Keys need no identity provider, but adding or revoking one means changing `AUTH_API_KEYS` and redeploying. With many clients or federated identity, move to OAuth2 client credentials by implementing `httpapi.Authenticator`.
- **Coarse roles.** There are two roles, and ownership is per client. Finer-grained permissions, such as read-only keys or per-account grants, would need scopes on the principal.
- **Per-replica rate limits.** Buckets live in memory, so N replicas allow N times the configured rate. A gateway or a shared store (for example Redis) gives a global limit.
- **Out of scope:** currency conversion, metrics and tracing (OpenTelemetry), `lock_timeout` / `statement_timeout` tuning, and mutual TLS.

## License

Released under the [MIT License](LICENSE). Copyright (c) 2026 Yohanes Adrian Risangtomo.
