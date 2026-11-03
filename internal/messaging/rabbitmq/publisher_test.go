package rabbitmq_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/messaging/rabbitmq"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPublishFailsWhenBrokerIsUnreachable(t *testing.T) {
	pub := rabbitmq.NewPublisher("amqp://guest:guest@127.0.0.1:1/", "ledger.events", "", discardLogger())
	defer pub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := pub.Publish(ctx, domain.Event{ID: uuid.New(), Type: domain.EventAccountCreated, Payload: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected an error when the broker is unreachable")
	}
}
