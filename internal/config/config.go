// Package config loads the service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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
	AuthAPIKeys string

	DatabaseURL    string
	DBMaxConns     int32
	MigrateOnStart bool

	AMQPURL      string
	AMQPExchange string
	AMQPQueue    string

	OutboxPollInterval time.Duration
	OutboxBatchSize    int
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
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		DBMaxConns:         parseVar("DB_MAX_CONNS", int32(10), parseInt32, &errs),
		MigrateOnStart:     parseVar("MIGRATE_ON_START", true, strconv.ParseBool, &errs),
		AMQPURL:            stringVar("AMQP_URL", "amqp://guest:guest@localhost:5672/"),
		AMQPExchange:       stringVar("AMQP_EXCHANGE", "ledger.events"),
		AMQPQueue:          os.Getenv("AMQP_QUEUE"),
		OutboxPollInterval: parseVar("OUTBOX_POLL_INTERVAL", time.Second, time.ParseDuration, &errs),
		OutboxBatchSize:    parseVar("OUTBOX_BATCH_SIZE", 100, strconv.Atoi, &errs),
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

func parseInt32(s string) (int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	return int32(n), err
}
