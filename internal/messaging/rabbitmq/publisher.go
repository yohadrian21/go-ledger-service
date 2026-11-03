// Package rabbitmq publishes domain events to a RabbitMQ topic exchange.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/yohadrian21/go-ledger-service/internal/domain"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

const dialTimeout = 5 * time.Second

// Publisher implements usecase.Publisher. It connects lazily and reconnects
// after failures, so the service keeps accepting requests while the broker
// is down; events simply wait in the outbox until it is back.
type Publisher struct {
	url      string
	exchange string
	queue    string
	log      *slog.Logger

	mu   sync.Mutex
	conn *amqp.Connection
	ch   *amqp.Channel
}

var _ usecase.Publisher = (*Publisher)(nil)

// NewPublisher publishes to a durable topic exchange using the event type as
// routing key. If queue is not empty, a durable queue bound to every event is
// declared as well, which is handy for local development.
func NewPublisher(url, exchange, queue string, log *slog.Logger) *Publisher {
	return &Publisher{url: url, exchange: exchange, queue: queue, log: log}
}

// Publish sends a persistent message and waits for the broker's publisher
// confirm, so a nil error means RabbitMQ has taken responsibility for it.
func (p *Publisher) Publish(ctx context.Context, e domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	ch, err := p.channel()
	if err != nil {
		return err
	}
	confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, p.exchange, e.Type, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    e.ID.String(),
		Type:         e.Type,
		Timestamp:    e.OccurredAt,
		AppId:        "go-ledger-service",
		Body:         e.Payload,
	})
	if err != nil {
		p.reset()
		return fmt.Errorf("publish: %w", err)
	}
	acked, err := confirm.WaitContext(ctx)
	if err != nil {
		p.reset()
		return fmt.Errorf("wait for publisher confirm: %w", err)
	}
	if !acked {
		return errors.New("broker rejected the message")
	}
	return nil
}

// Close closes the connection to the broker.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reset()
}

func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	p.reset()

	cfg := amqp.Config{Properties: amqp.NewConnectionProperties(), Dial: amqp.DefaultDial(dialTimeout)}
	cfg.Properties.SetClientConnectionName("go-ledger-service")
	conn, err := amqp.DialConfig(p.url, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to rabbitmq: %w", err)
	}
	p.conn = conn

	ch, err := conn.Channel()
	if err != nil {
		p.reset()
		return nil, fmt.Errorf("open channel: %w", err)
	}
	p.ch = ch
	if err := p.setup(ch); err != nil {
		p.reset()
		return nil, err
	}
	p.log.Info("connected to rabbitmq", "exchange", p.exchange)
	return ch, nil
}

func (p *Publisher) setup(ch *amqp.Channel) error {
	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}
	if err := ch.ExchangeDeclare(p.exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange: %w", err)
	}
	if p.queue == "" {
		return nil
	}
	if _, err := ch.QueueDeclare(p.queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare queue: %w", err)
	}
	if err := ch.QueueBind(p.queue, "#", p.exchange, false, nil); err != nil {
		return fmt.Errorf("bind queue: %w", err)
	}
	return nil
}

func (p *Publisher) reset() {
	if p.ch != nil {
		_ = p.ch.Close()
	}
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.ch, p.conn = nil, nil
}
