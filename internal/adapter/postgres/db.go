package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

type Config struct {
	DSN              string
	MaxConns         int32
	MinConns         int32
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	ApplicationName  string
	Tracer           pgx.QueryTracer
}

func (c Config) Validate() error {
	switch {
	case c.DSN == "":
		return errors.New("postgres: DSN is required")
	case c.MaxConns < 1 || c.MinConns < 0 || c.MinConns > c.MaxConns:
		return fmt.Errorf("postgres: invalid pool size min=%d max=%d", c.MinConns, c.MaxConns)
	case c.LockTimeout <= 0 || c.StatementTimeout <= 0:
		return errors.New("postgres: lock and statement timeouts must be positive")
	}
	return nil
}

func NewLazyPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	poolCfg.ConnConfig.RuntimeParams["application_name"] = cmp.Or(cfg.ApplicationName, "wallet-service")
	if cfg.Tracer != nil {
		poolCfg.ConnConfig.Tracer = cfg.Tracer
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, translate(err)
	}
	return pool, nil
}

func NewPool(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	pool, err := NewLazyPool(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, translate(err)
	}
	return pool, nil
}

func WaitReady(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) error {
	for {
		err := pool.Ping(ctx)
		if err == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("postgres: not ready: %w", errors.Join(translate(err), ctx.Err()))
		case <-timer.C:
		}
	}
}

func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	return translate(pool.Ping(ctx))
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

type txKey struct{}

type TxManager struct {
	pool             *pgxpool.Pool
	lockTimeout      time.Duration
	statementTimeout time.Duration
}

var _ app.TxManager = (*TxManager)(nil)

func NewTxManager(pool *pgxpool.Pool, cfg Config) *TxManager {
	return &TxManager{pool: pool, lockTimeout: cfg.LockTimeout, statementTimeout: cfg.StatementTimeout}
}

func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return m.run(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, fn)
}

func (m *TxManager) WithinSnapshot(ctx context.Context, fn func(ctx context.Context) error) error {
	return m.run(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func (m *TxManager) run(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context) error) (err error) {
	if _, active := ctx.Value(txKey{}).(pgx.Tx); active {
		return fn(ctx)
	}

	tx, err := m.pool.BeginTx(ctx, opts)
	if err != nil {
		return translate(err)
	}
	defer func() {
		if err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if _, err = tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)",
		durationSetting(m.lockTimeout), durationSetting(m.statementTimeout)); err != nil {
		return translate(err)
	}
	if err = fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return translate(err)
	}
	return nil
}

func durationSetting(d time.Duration) string {
	return fmt.Sprintf("%dms", d.Milliseconds())
}

type conn struct {
	pool *pgxpool.Pool
}

func (c conn) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx
	}
	return c.pool
}

func (c conn) tx(ctx context.Context) (pgx.Tx, error) {
	if tx, ok := ctx.Value(txKey{}).(pgx.Tx); ok {
		return tx, nil
	}
	return nil, app.ErrTxRequired
}
