package fxapp

import (
	"math/rand/v2"
	"time"

	"go.uber.org/fx"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/config"
)

var ApplicationModule = fx.Module("application",
	fx.Provide(
		fx.Annotate(func() app.SystemClock { return app.SystemClock{} }, fx.As(new(app.Clock))),
		fx.Annotate(func() app.UUIDv7Generator { return app.UUIDv7Generator{} }, fx.As(new(app.IDGenerator))),
		NewWalletService,
		NewWageringService,
	),
)

type repositories struct {
	fx.In

	Tx           app.TxManager
	Wallets      app.WalletRepository
	Transactions app.TransactionRepository
	Ledger       app.LedgerRepository
	Inbox        app.InboxRepository
	Outbox       app.OutboxRepository
	Clock        app.Clock
	IDs          app.IDGenerator
	Observer     app.Observer
}

func retryPolicy(cfg config.Config) app.RetryPolicy {
	return app.RetryPolicy{Attempts: cfg.Retry.Attempts, BaseDelay: cfg.Retry.BaseDelay, MaxDelay: cfg.Retry.MaxDelay}
}

func NewWalletService(cfg config.Config, r repositories) (*walletapp.Service, error) {
	return walletapp.NewService(walletapp.Deps{
		Tx: r.Tx, Wallets: r.Wallets, Transactions: r.Transactions, Ledger: r.Ledger, Outbox: r.Outbox,
		Clock: r.Clock, IDs: r.IDs, Observer: r.Observer, Retry: retryPolicy(cfg),
	})
}

func NewWageringService(cfg config.Config, r repositories) (*wageringapp.Service, error) {
	return wageringapp.NewService(wageringapp.Deps{
		Tx: r.Tx, Wallets: r.Wallets, Transactions: r.Transactions, Ledger: r.Ledger, Inbox: r.Inbox, Outbox: r.Outbox,
		Clock: r.Clock, IDs: r.IDs, Observer: r.Observer, Retry: retryPolicy(cfg),
		Pending: wagering.PendingPolicy{
			TTL: cfg.Pending.TTL, BaseDelay: cfg.Pending.BaseDelay, MaxDelay: cfg.Pending.MaxDelay,
			Jitter: func(d time.Duration) time.Duration { return rand.N(d/5 + 1) },
		},
		ClaimLease: cfg.Pending.ClaimLease,
	})
}
