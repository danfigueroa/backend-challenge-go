package wallet

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

const InitialVersion int64 = 1

type Wallet struct {
	id            uuid.UUID
	playerID      uuid.UUID
	balance       money.Money
	version       int64
	loadedVersion int64
	createdAt     time.Time
	updatedAt     time.Time
}

type OpenParams struct {
	ID                   uuid.UUID
	PlayerID             uuid.UUID
	InitialBalance       money.Money
	OpeningTransactionID uuid.UUID
	OpeningEntryID       uuid.UUID
	Now                  time.Time
}

type Opened struct {
	Wallet       *Wallet
	OpeningEntry *LedgerEntry
}

func Open(p OpenParams) (Opened, error) {
	switch {
	case p.ID == uuid.Nil:
		return Opened{}, fmt.Errorf("%w: id is required", ErrInvalidWallet)
	case p.PlayerID == uuid.Nil:
		return Opened{}, fmt.Errorf("%w: player id is required", ErrInvalidWallet)
	case !p.InitialBalance.IsValid():
		return Opened{}, fmt.Errorf("%w: initial balance is required", ErrInvalidWallet)
	case p.InitialBalance.IsNegative():
		return Opened{}, fmt.Errorf("%w: initial balance must not be negative", ErrInvalidAmount)
	case p.Now.IsZero():
		return Opened{}, fmt.Errorf("%w: timestamp is required", ErrInvalidWallet)
	}

	now := p.Now.UTC()
	w := &Wallet{
		id:            p.ID,
		playerID:      p.PlayerID,
		balance:       p.InitialBalance,
		version:       InitialVersion,
		loadedVersion: 0,
		createdAt:     now,
		updatedAt:     now,
	}
	if p.InitialBalance.IsZero() {
		return Opened{Wallet: w}, nil
	}

	zero, err := money.Zero(p.InitialBalance.Currency())
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %w", ErrInvalidWallet, err)
	}
	entry, err := newLedgerEntry(LedgerEntryParams{
		ID:            p.OpeningEntryID,
		WalletID:      p.ID,
		TransactionID: p.OpeningTransactionID,
		Direction:     DirectionCredit,
		Amount:        p.InitialBalance,
		BalanceBefore: zero,
		BalanceAfter:  p.InitialBalance,
		WalletVersion: InitialVersion,
		CreatedAt:     now,
	})
	if err != nil {
		return Opened{}, err
	}
	return Opened{Wallet: w, OpeningEntry: &entry}, nil
}

type RehydrateParams struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func Rehydrate(p RehydrateParams) (*Wallet, error) {
	switch {
	case p.ID == uuid.Nil:
		return nil, fmt.Errorf("%w: id is required", ErrInvalidWallet)
	case p.PlayerID == uuid.Nil:
		return nil, fmt.Errorf("%w: player id is required", ErrInvalidWallet)
	case !p.Balance.IsValid():
		return nil, fmt.Errorf("%w: balance is required", ErrInvalidWallet)
	case p.Balance.IsNegative():
		return nil, fmt.Errorf("%w: persisted balance is negative", ErrInvalidWallet)
	case p.Version < InitialVersion:
		return nil, fmt.Errorf("%w: version must be at least %d", ErrInvalidWallet, InitialVersion)
	case p.CreatedAt.IsZero() || p.UpdatedAt.IsZero():
		return nil, fmt.Errorf("%w: timestamps are required", ErrInvalidWallet)
	case p.UpdatedAt.Before(p.CreatedAt):
		return nil, fmt.Errorf("%w: updated at precedes created at", ErrInvalidWallet)
	}
	return &Wallet{
		id:            p.ID,
		playerID:      p.PlayerID,
		balance:       p.Balance,
		version:       p.Version,
		loadedVersion: p.Version,
		createdAt:     p.CreatedAt.UTC(),
		updatedAt:     p.UpdatedAt.UTC(),
	}, nil
}

type Movement struct {
	EntryID       uuid.UUID
	TransactionID uuid.UUID
	Amount        money.Money
	Now           time.Time
}

func (w *Wallet) CanDebit(amount money.Money) (bool, error) {
	if err := w.checkAmount(amount); err != nil {
		return false, err
	}
	cmp, err := w.balance.Cmp(amount)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrCurrencyMismatch, err)
	}
	return cmp >= 0, nil
}

func (w *Wallet) Debit(m Movement) (LedgerEntry, error) {
	ok, err := w.CanDebit(m.Amount)
	if err != nil {
		return LedgerEntry{}, err
	}
	if !ok {
		return LedgerEntry{}, fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, w.balance, m.Amount)
	}
	return w.apply(DirectionDebit, m)
}

func (w *Wallet) Credit(m Movement) (LedgerEntry, error) {
	if err := w.checkAmount(m.Amount); err != nil {
		return LedgerEntry{}, err
	}
	return w.apply(DirectionCredit, m)
}

func (w *Wallet) apply(direction Direction, m Movement) (LedgerEntry, error) {
	if m.Now.IsZero() {
		return LedgerEntry{}, fmt.Errorf("%w: timestamp is required", ErrInvalidLedgerEntry)
	}
	after, err := applyDirection(direction, w.balance, m.Amount)
	if err != nil {
		return LedgerEntry{}, err
	}
	now := m.Now.UTC()
	entry, err := newLedgerEntry(LedgerEntryParams{
		ID:            m.EntryID,
		WalletID:      w.id,
		TransactionID: m.TransactionID,
		Direction:     direction,
		Amount:        m.Amount,
		BalanceBefore: w.balance,
		BalanceAfter:  after,
		WalletVersion: w.version + 1,
		CreatedAt:     now,
	})
	if err != nil {
		return LedgerEntry{}, err
	}

	w.balance = after
	w.version++
	if now.After(w.updatedAt) {
		w.updatedAt = now
	}
	return entry, nil
}

func (w *Wallet) checkAmount(amount money.Money) error {
	switch {
	case !amount.IsValid():
		return fmt.Errorf("%w: amount is required", ErrInvalidAmount)
	case !amount.IsPositive():
		return fmt.Errorf("%w: amount must be positive, got %s", ErrInvalidAmount, amount)
	case amount.Currency() != w.balance.Currency():
		return fmt.Errorf("%w: wallet %s, movement %s", ErrCurrencyMismatch, w.balance.Currency(), amount.Currency())
	}
	return nil
}

func (w *Wallet) ID() uuid.UUID            { return w.id }
func (w *Wallet) PlayerID() uuid.UUID      { return w.playerID }
func (w *Wallet) Currency() money.Currency { return w.balance.Currency() }
func (w *Wallet) Balance() money.Money     { return w.balance }
func (w *Wallet) Version() int64           { return w.version }
func (w *Wallet) LoadedVersion() int64     { return w.loadedVersion }
func (w *Wallet) IsNew() bool              { return w.loadedVersion == 0 }
func (w *Wallet) HasChanges() bool         { return w.version != w.loadedVersion }
func (w *Wallet) CreatedAt() time.Time     { return w.createdAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.updatedAt }
