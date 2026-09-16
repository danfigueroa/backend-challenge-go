package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

type WalletRepository struct {
	conn
}

var _ app.WalletRepository = (*WalletRepository)(nil)

func NewWalletRepository(pool *pgxpool.Pool) *WalletRepository {
	return &WalletRepository{conn{pool: pool}}
}

func (r *WalletRepository) Insert(ctx context.Context, w *wallet.Wallet) error {
	_, err := r.q(ctx).Exec(ctx,
		`INSERT INTO wallets (`+walletColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency().Code(), w.Balance().Minor(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	return translate(err)
}

func (r *WalletRepository) Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return scanWallet(r.q(ctx).QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id))
}

func (r *WalletRepository) GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	tx, err := r.tx(ctx)
	if err != nil {
		return nil, err
	}
	return scanWallet(tx.QueryRow(ctx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id))
}

func (r *WalletRepository) Update(ctx context.Context, w *wallet.Wallet) error {
	if !w.HasChanges() {
		return nil
	}
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE wallets SET balance_minor = $1, version = $2, updated_at = $3 WHERE id = $4 AND version = $5`,
		w.Balance().Minor(), w.Version(), w.UpdatedAt(), w.ID(), w.LoadedVersion())
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: wallet %s is no longer at version %d", app.ErrConcurrentUpdate, w.ID(), w.LoadedVersion())
	}
	return nil
}

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, playerID         uuid.UUID
		currencyCode         string
		balanceMinor         int64
		version              int64
		createdAt, updatedAt time.Time
	)
	if err := row.Scan(&id, &playerID, &currencyCode, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		return nil, translate(err)
	}
	balance, err := moneyFromColumns(balanceMinor, currencyCode)
	if err != nil {
		return nil, err
	}
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: id, PlayerID: playerID, Balance: balance, Version: version, CreatedAt: createdAt, UpdatedAt: updatedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: wallet %s: %w", app.ErrIntegrityViolation, id, err)
	}
	return w, nil
}

func moneyFromColumns(minor int64, currencyCode string) (money.Money, error) {
	currency, err := money.ParseCurrency(currencyCode)
	if err != nil {
		return money.Money{}, fmt.Errorf("%w: %w", app.ErrIntegrityViolation, err)
	}
	m, err := money.New(minor, currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("%w: %w", app.ErrIntegrityViolation, err)
	}
	return m, nil
}
