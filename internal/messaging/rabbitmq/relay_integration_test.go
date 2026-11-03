//go:build integration

package rabbitmq_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/messaging/rabbitmq"
	"github.com/yohadrian21/go-ledger-service/internal/repository/postgres"
	"github.com/yohadrian21/go-ledger-service/internal/testutil/pgtest"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

// TestOutboxRelayPublishesToRabbitMQ runs the whole event pipeline: ledger
// writes go to the outbox in PostgreSQL and the relay delivers them to a
// real RabbitMQ broker.
func TestOutboxRelayPublishesToRabbitMQ(t *testing.T) {
	amqpURL := os.Getenv("TEST_AMQP_URL")
	if amqpURL == "" {
		t.Fatal("TEST_AMQP_URL is not set; start the dependencies with `make deps-up` or run `make test-integration`")
	}
	ctx := context.Background()
	exchange := "ledger.test." + uuid.NewString()
	msgs := consume(t, amqpURL, exchange)

	pool := pgtest.New(t)
	store := postgres.NewStore(pool)
	ledger := usecase.NewLedger(store)

	acc, _, err := ledger.CreateAccount(ctx, usecase.CreateAccountInput{IdempotencyKey: "acc", Name: "Alice", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	dep, _, err := ledger.Deposit(ctx, usecase.DepositInput{IdempotencyKey: "dep", AccountID: acc.ID, Amount: 500})
	if err != nil {
		t.Fatal(err)
	}

	pub := rabbitmq.NewPublisher(amqpURL, exchange, "", discardLogger())
	t.Cleanup(pub.Close)
	relay := usecase.NewOutboxRelay(store, pub, 10, time.Second, discardLogger())

	if n, err := relay.RelayOnce(ctx); err != nil || n != 2 {
		t.Fatalf("RelayOnce = %d, %v; want 2, nil", n, err)
	}

	created := receive(t, msgs)
	if created.Type != domain.EventAccountCreated || created.RoutingKey != domain.EventAccountCreated {
		t.Fatalf("first message type=%q routing key=%q", created.Type, created.RoutingKey)
	}
	if _, err := uuid.Parse(created.MessageId); err != nil {
		t.Errorf("message id %q is not an event UUID", created.MessageId)
	}
	if created.ContentType != "application/json" || created.DeliveryMode != amqp.Persistent {
		t.Errorf("content type %q, delivery mode %d", created.ContentType, created.DeliveryMode)
	}

	posted := receive(t, msgs)
	var payload struct {
		TransactionID uuid.UUID `json:"transaction_id"`
		Amount        int64     `json:"amount"`
	}
	if err := json.Unmarshal(posted.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if posted.Type != domain.EventTransactionPosted || payload.TransactionID != dep.ID || payload.Amount != 500 {
		t.Fatalf("unexpected transaction.posted message: type=%q body=%s", posted.Type, posted.Body)
	}

	if n, err := relay.RelayOnce(ctx); err != nil || n != 0 {
		t.Fatalf("events were not marked as published: RelayOnce = %d, %v", n, err)
	}
}

// consume binds a temporary queue to every event on exchange.
func consume(t *testing.T, url, exchange string) <-chan amqp.Delivery {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("connect to rabbitmq: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.ExchangeDeclare(exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.ExchangeDelete(exchange, false, false) })
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.QueueBind(q.Name, "#", exchange, false, nil); err != nil {
		t.Fatal(err)
	}
	msgs, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func receive(t *testing.T, msgs <-chan amqp.Delivery) amqp.Delivery {
	t.Helper()
	select {
	case m := <-msgs:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a message")
		return amqp.Delivery{}
	}
}
