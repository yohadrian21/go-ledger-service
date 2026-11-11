// Package config loads the service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	ShutdownTimeout time.Duration
	LogLevel        slog.Level
	LogFormat       string

	// AuthAPIKeys lists the API key digests clients authenticate with, in
	// the format described in package auth.
	AuthAPIKeys    string
	RateLimitRPS   float64
	RateLimitBurst int

	DatabaseURL    string
	DBMaxConns     int32
	MigrateOnStart bool

	AMQPURL      string
	AMQPExchange string
	AMQPQueue    string

	OutboxPollInterval time.Duration
	OutboxBatchSize    int

	IdempotencyKeyTTL time.Duration
	OutboxRetention   time.Duration
	CleanupInterval   time.Duration
}

// Load reads the configuration from the environment, applying defaults for
// everything except DATABASE_URL and AUTH_API_KEYS. All problems are reported
// at once.
func Load() (Config, error) {
	var errs []error
	cfg := Config{
		HTTPAddr:           stringVar("HTTP_ADDR", ":8080"),
		ShutdownTimeout:    parseVar("SHUTDOWN_TIMEOUT", 15*time.Second, time.ParseDuration, &errs),
		LogLevel:           parseVar("LOG_LEVEL", slog.LevelInfo, parseLevel, &errs),
		LogFormat:          stringVar("LOG_FORMAT", "json"),
		AuthAPIKeys:        os.Getenv("AUTH_API_KEYS"),
		RateLimitRPS:       parseVar("RATE_LIMIT_RPS", 20.0, parseFloat, &errs),
		RateLimitBurst:     parseVar("RATE_LIMIT_BURST", 40, strconv.Atoi, &errs),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		DBMaxConns:         parseVar("DB_MAX_CONNS", int32(10), parseInt32, &errs),
		MigrateOnStart:     parseVar("MIGRATE_ON_START", true, strconv.ParseBool, &errs),
		AMQPURL:            stringVar("AMQP_URL", "amqp://guest:guest@localhost:5672/"),
		AMQPExchange:       stringVar("AMQP_EXCHANGE", "ledger.events"),
		AMQPQueue:          os.Getenv("AMQP_QUEUE"),
		OutboxPollInterval: parseVar("OUTBOX_POLL_INTERVAL", time.Second, time.ParseDuration, &errs),
		OutboxBatchSize:    parseVar("OUTBOX_BATCH_SIZE", 100, strconv.Atoi, &errs),
		IdempotencyKeyTTL:  parseVar("IDEMPOTENCY_KEY_TTL", 24*time.Hour, time.ParseDuration, &errs),
		OutboxRetention:    parseVar("OUTBOX_RETENTION", 7*24*time.Hour, time.ParseDuration, &errs),
		CleanupInterval:    parseVar("CLEANUP_INTERVAL", 15*time.Minute, time.ParseDuration, &errs),
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if strings.TrimSpace(cfg.AuthAPIKeys) == "" {
		errs = append(errs, errors.New("AUTH_API_KEYS is required (generate keys with `go run ./cmd/apikey`)"))
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		errs = append(errs, errors.New("LOG_FORMAT must be json or text"))
	}
	if !(cfg.RateLimitRPS > 0) || math.IsInf(cfg.RateLimitRPS, 0) {
		errs = append(errs, errors.New("RATE_LIMIT_RPS must be a positive number"))
	}
	if cfg.RateLimitBurst < 1 {
		errs = append(errs, errors.New("RATE_LIMIT_BURST must be at least 1"))
	}
	if cfg.DBMaxConns <= 0 {
		errs = append(errs, errors.New("DB_MAX_CONNS must be positive"))
	}
	if cfg.OutboxBatchSize <= 0 {
		errs = append(errs, errors.New("OUTBOX_BATCH_SIZE must be positive"))
	}
	if cfg.OutboxPollInterval <= 0 {
		errs = append(errs, errors.New("OUTBOX_POLL_INTERVAL must be positive"))
	}
	if cfg.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT must be positive"))
	}
	if cfg.IdempotencyKeyTTL < time.Hour {
		errs = append(errs, errors.New("IDEMPOTENCY_KEY_TTL must be at least 1h so that client retries stay safe"))
	}
	if cfg.OutboxRetention <= 0 {
		errs = append(errs, errors.New("OUTBOX_RETENTION must be positive"))
	}
	if cfg.CleanupInterval <= 0 {
		errs = append(errs, errors.New("CLEANUP_INTERVAL must be positive"))
	}
	return cfg, errors.Join(errs...)
}

func stringVar(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func parseVar[T any](name string, def T, parse func(string) (T, error), errs *[]error) T {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	out, err := parse(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: invalid value %q", name, v))
		return def
	}
	return out
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	err := l.UnmarshalText([]byte(s))
	return l, err
}

func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func parseInt32(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	return int32(n), err
}
