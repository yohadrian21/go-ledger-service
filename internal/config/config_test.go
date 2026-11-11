package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/ledger")
	t.Setenv("AUTH_API_KEYS", "placeholder")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo || cfg.LogFormat != "json" {
		t.Errorf("unexpected server defaults: %+v", cfg)
	}
	if cfg.DBMaxConns != 10 || !cfg.MigrateOnStart || cfg.AMQPExchange != "ledger.events" {
		t.Errorf("unexpected dependency defaults: %+v", cfg)
	}
	if cfg.OutboxPollInterval != time.Second || cfg.OutboxBatchSize != 100 || cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("unexpected worker defaults: %+v", cfg)
	}
	if cfg.IdempotencyKeyTTL != 24*time.Hour || cfg.OutboxRetention != 7*24*time.Hour || cfg.CleanupInterval != 15*time.Minute {
		t.Errorf("unexpected retention defaults: %+v", cfg)
	}
	if cfg.RateLimitRPS != 20 || cfg.RateLimitBurst != 40 {
		t.Errorf("unexpected rate limit defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://db/ledger")
	t.Setenv("AUTH_API_KEYS", "placeholder")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("DB_MAX_CONNS", "25")
	t.Setenv("MIGRATE_ON_START", "false")
	t.Setenv("AMQP_QUEUE", "audit")
	t.Setenv("OUTBOX_POLL_INTERVAL", "250ms")
	t.Setenv("OUTBOX_BATCH_SIZE", "50")
	t.Setenv("IDEMPOTENCY_KEY_TTL", "48h")
	t.Setenv("OUTBOX_RETENTION", "72h")
	t.Setenv("CLEANUP_INTERVAL", "5m")
	t.Setenv("RATE_LIMIT_RPS", "2.5")
	t.Setenv("RATE_LIMIT_BURST", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" || cfg.DBMaxConns != 25 ||
		cfg.MigrateOnStart || cfg.AMQPQueue != "audit" || cfg.OutboxPollInterval != 250*time.Millisecond || cfg.OutboxBatchSize != 50 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.IdempotencyKeyTTL != 48*time.Hour || cfg.OutboxRetention != 72*time.Hour || cfg.CleanupInterval != 5*time.Minute {
		t.Errorf("retention overrides not applied: %+v", cfg)
	}
	if cfg.RateLimitRPS != 2.5 || cfg.RateLimitBurst != 5 {
		t.Errorf("rate limit overrides not applied: %+v", cfg)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AUTH_API_KEYS", " ")
	t.Setenv("LOG_LEVEL", "loud")
	t.Setenv("DB_MAX_CONNS", "99999999999")
	t.Setenv("OUTBOX_BATCH_SIZE", "0")
	t.Setenv("OUTBOX_POLL_INTERVAL", "soon")
	t.Setenv("IDEMPOTENCY_KEY_TTL", "5m")
	t.Setenv("OUTBOX_RETENTION", "-1h")
	t.Setenv("CLEANUP_INTERVAL", "0s")
	t.Setenv("RATE_LIMIT_RPS", "NaN")
	t.Setenv("RATE_LIMIT_BURST", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"DATABASE_URL", "AUTH_API_KEYS", "LOG_LEVEL", "DB_MAX_CONNS", "OUTBOX_BATCH_SIZE", "OUTBOX_POLL_INTERVAL",
		"IDEMPOTENCY_KEY_TTL", "OUTBOX_RETENTION", "CLEANUP_INTERVAL", "RATE_LIMIT_RPS", "RATE_LIMIT_BURST",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
