package walletapp

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

type Deps struct {
	Tx           app.TxManager
	Wallets      app.WalletRepository
	Transactions app.TransactionRepository
	Ledger       app.LedgerRepository
	Outbox       app.OutboxRepository
	Clock        app.Clock
	IDs          app.IDGenerator
	Observer     app.Observer
	Retry        app.RetryPolicy
}

type Service struct {
	tx           app.TxManager
	wallets      app.WalletRepository
	transactions app.TransactionRepository
	ledger       app.LedgerRepository
	outbox       app.OutboxRepository
	clock        app.Clock
	ids          app.IDGenerator
	observer     app.Observer
	retry        app.RetryPolicy
}

func NewService(d Deps) (*Service, error) {
	if d.Tx == nil || d.Wallets == nil || d.Transactions == nil || d.Ledger == nil || d.Outbox == nil ||
		d.Clock == nil || d.IDs == nil || d.Observer == nil {
		return nil, errors.New("walletapp: all dependencies are required")
	}
	if err := d.Retry.Validate(); err != nil {
		return nil, err
	}
	return &Service{
		tx: d.Tx, wallets: d.Wallets, transactions: d.Transactions, ledger: d.Ledger, outbox: d.Outbox,
		clock: d.Clock, ids: d.IDs, observer: d.Observer, retry: d.Retry,
	}, nil
}

type WalletView struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func viewOf(w *wallet.Wallet) WalletView {
	return WalletView{
		ID: w.ID(), PlayerID: w.PlayerID(), Balance: w.Balance(), Version: w.Version(),
		CreatedAt: w.CreatedAt(), UpdatedAt: w.UpdatedAt(),
	}
}
