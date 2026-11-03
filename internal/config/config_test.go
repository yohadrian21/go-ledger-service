package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/ledger")

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
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://db/ledger")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("DB_MAX_CONNS", "25")
	t.Setenv("MIGRATE_ON_START", "false")
	t.Setenv("AMQP_QUEUE", "audit")
	t.Setenv("OUTBOX_POLL_INTERVAL", "250ms")
	t.Setenv("OUTBOX_BATCH_SIZE", "50")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9090" || cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" || cfg.DBMaxConns != 25 ||
		cfg.MigrateOnStart || cfg.AMQPQueue != "audit" || cfg.OutboxPollInterval != 250*time.Millisecond || cfg.OutboxBatchSize != 50 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("LOG_LEVEL", "loud")
	t.Setenv("DB_MAX_CONNS", "99999999999")
	t.Setenv("OUTBOX_BATCH_SIZE", "0")
	t.Setenv("OUTBOX_POLL_INTERVAL", "soon")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "DB_MAX_CONNS", "OUTBOX_BATCH_SIZE", "OUTBOX_POLL_INTERVAL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
