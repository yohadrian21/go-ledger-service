// Command ledger runs the payments ledger HTTP API and the outbox relay.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yohadrian21/go-ledger-service/internal/config"
	"github.com/yohadrian21/go-ledger-service/internal/delivery/httpapi"
	"github.com/yohadrian21/go-ledger-service/internal/messaging/rabbitmq"
	"github.com/yohadrian21/go-ledger-service/internal/repository/postgres"
	"github.com/yohadrian21/go-ledger-service/internal/usecase"
)

func main() {
	if err := run(); err != nil {
		slog.Error("ledger service failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.MigrateOnStart {
		if err := postgres.Migrate(cfg.DatabaseURL); err != nil {
			return err
		}
		log.Info("database migrations applied")
	}

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := postgres.NewStore(pool)
	publisher := rabbitmq.NewPublisher(cfg.AMQPURL, cfg.AMQPExchange, cfg.AMQPQueue, log)
	defer publisher.Close()
	relay := usecase.NewOutboxRelay(store, publisher, cfg.OutboxBatchSize, cfg.OutboxPollInterval, log)

	var shuttingDown atomic.Bool
	ready := func(ctx context.Context) error {
		if shuttingDown.Load() {
			return errors.New("shutting down")
		}
		return pool.Ping(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(usecase.NewLedger(store), ready, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		relay.Run(gctx)
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		log.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
		// Fail readiness first so load balancers stop sending new requests,
		// then let in-flight requests finish.
		shuttingDown.Store(true)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h).With("service", "go-ledger-service")
}
