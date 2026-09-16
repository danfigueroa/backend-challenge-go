package fxapp

import (
	"context"
	"log/slog"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/health"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/tracing"
)

const databaseReadyInterval = 500 * time.Millisecond

var PostgresModule = fx.Module("postgres",
	fx.Provide(
		NewPostgresConfig,
		NewPool,
		fx.Annotate(postgres.NewTxManager, fx.As(new(app.TxManager))),
		fx.Annotate(postgres.NewWalletRepository, fx.As(new(app.WalletRepository))),
		fx.Annotate(postgres.NewTransactionRepository, fx.As(new(app.TransactionRepository))),
		fx.Annotate(postgres.NewLedgerRepository, fx.As(new(app.LedgerRepository))),
		fx.Annotate(postgres.NewInboxRepository, fx.As(new(app.InboxRepository))),
		postgres.NewOutboxRepository,
		func(r *postgres.OutboxRepository) app.OutboxRepository { return r },
	),
	fx.Invoke(registerDatabaseHealth, registerOutboxBacklog),
)

func NewPostgresConfig(cfg config.Config, _ *tracing.Provider) postgres.Config {
	return postgres.Config{
		DSN: cfg.Database.URL, MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns,
		LockTimeout: cfg.Database.LockTimeout, StatementTimeout: cfg.Database.StatementTimeout,
		ApplicationName: cfg.App.Name + "/" + cfg.App.InstanceID,
		Tracer:          otelpgx.NewTracer(),
	}
}

func NewPool(lc fx.Lifecycle, pgCfg postgres.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.NewLazyPool(context.Background(), pgCfg)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := postgres.WaitReady(ctx, pool, databaseReadyInterval); err != nil {
				return err
			}
			logger.InfoContext(ctx, "postgres connection pool ready")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			pool.Close()
			logger.InfoContext(ctx, "postgres connection pool closed")
			return nil
		},
	})
	return pool, nil
}

func registerDatabaseHealth(checker *health.Checker, pool *pgxpool.Pool) {
	checker.Register("postgres", func(ctx context.Context) error { return postgres.Ping(ctx, pool) })
}

func registerOutboxBacklog(m *metrics.Metrics, outbox *postgres.OutboxRepository, cfg config.Config, logger *slog.Logger) error {
	return m.RegisterOutboxBacklog(outbox, cfg.Database.HealthTimeout, logger)
}
