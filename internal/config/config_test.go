package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "postgres://localhost/ledger")
	t.Setenv("AUTH_API_KEYS", "placeholder")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env != EnvDevelopment || cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo || cfg.LogFormat != "json" {
		t.Errorf("unexpected server defaults: %+v", cfg)
	}
	if cfg.DBMaxConns != 10 || !cfg.MigrateOnStart || cfg.AMQPExchange != "ledger.events" || cfg.AMQPURL != "amqp://guest:guest@localhost:5672/" {
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
	t.Setenv("DATABASE_URL", "postgres://svc:placeholder-password@db/ledger?sslmode=verify-full")
	t.Setenv("AMQP_URL", "amqps://svc:placeholder-password@mq/")
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
	if cfg.Env != EnvProduction {
		t.Errorf("APP_ENV must default to production, got %q", cfg.Env)
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

func TestLoadRequiresExplicitCredentialsOutsideDevelopment(t *testing.T) {
	const (
		goodDB   = "postgres://svc:placeholder-password@db/ledger"
		goodAMQP = "amqp://svc:placeholder-password@mq/"
	)
	tests := []struct {
		name, env, db, amqp, want string
	}{
		{"missing AMQP_URL", "production", goodDB, "", "AMQP_URL is required"},
		{"AMQP without credentials", "production", goodDB, "amqp://mq:5672/", "AMQP_URL: set explicit"},
		{"AMQP guest account", "staging", goodDB, "amqp://guest:s3cret@mq/", "AMQP_URL: set explicit"},
		{"AMQP user without password", "production", goodDB, "amqp://svc@mq/", "AMQP_URL: set explicit"},
		{"AMQP compose credentials", "production", goodDB, "amqp://ledger:ledger@mq/", "AMQP_URL: set explicit"},
		{"AMQP invalid URL", "production", goodDB, "http://svc:s3cret@mq/", "AMQP_URL: invalid URL"},
		{"database compose credentials", "production", "postgres://ledger:ledger@db/ledger?sslmode=disable", goodAMQP, "DATABASE_URL: set non-default"},
		{"database key/value form", "production", "host=db user=ledger password=ledger dbname=ledger", goodAMQP, "DATABASE_URL: set non-default"},
		{"unknown environment", "prod", goodDB, goodAMQP, "APP_ENV must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tt.env)
			t.Setenv("DATABASE_URL", tt.db)
			t.Setenv("AMQP_URL", tt.amqp)
			t.Setenv("AUTH_API_KEYS", "placeholder")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "placeholder-password") {
				t.Fatalf("error must not echo credentials: %v", err)
			}
		})
	}

	t.Run("development allows local defaults", func(t *testing.T) {
		t.Setenv("APP_ENV", "development")
		t.Setenv("DATABASE_URL", "postgres://ledger:ledger@localhost/ledger?sslmode=disable")
		t.Setenv("AMQP_URL", "")
		t.Setenv("AUTH_API_KEYS", "placeholder")
		if _, err := Load(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestLoadReportsAllErrors(t *testing.T) {
	t.Setenv("APP_ENV", "development")
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
