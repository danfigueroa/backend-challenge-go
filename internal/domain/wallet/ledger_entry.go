package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

func ParseDirection(s string) (Direction, error) {
	switch d := Direction(s); d {
	case DirectionDebit, DirectionCredit:
		return d, nil
	default:
		return "", fmt.Errorf("%w: direction %q", ErrInvalidLedgerEntry, s)
	}
}

func (d Direction) String() string { return string(d) }

type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	walletVersion int64
	createdAt     time.Time
}

type LedgerEntryParams struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	WalletVersion int64
	CreatedAt     time.Time
}

func RehydrateLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	return newLedgerEntry(p)
}

func newLedgerEntry(p LedgerEntryParams) (LedgerEntry, error) {
	if err := validateLedgerEntry(p); err != nil {
		return LedgerEntry{}, err
	}
	return LedgerEntry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		walletVersion: p.WalletVersion,
		createdAt:     p.CreatedAt.UTC(),
	}, nil
}

func validateLedgerEntry(p LedgerEntryParams) error {
	invalid := func(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidLedgerEntry, reason) }

	switch {
	case p.ID == uuid.Nil:
		return invalid("id is required")
	case p.WalletID == uuid.Nil:
		return invalid("wallet id is required")
	case p.TransactionID == uuid.Nil:
		return invalid("transaction id is required")
	case p.WalletVersion < 1:
		return invalid("wallet version must be positive")
	case p.CreatedAt.IsZero():
		return invalid("created at is required")
	case !p.Amount.IsValid() || !p.BalanceBefore.IsValid() || !p.BalanceAfter.IsValid():
		return invalid("amount and balances are required")
	case !p.Amount.IsPositive():
		return invalid("amount must be positive")
	case p.BalanceBefore.IsNegative() || p.BalanceAfter.IsNegative():
		return invalid("balances must not be negative")
	}
	if _, err := ParseDirection(string(p.Direction)); err != nil {
		return err
	}

	expected, err := applyDirection(p.Direction, p.BalanceBefore, p.Amount)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidLedgerEntry, err)
	}
	if !expected.Equal(p.BalanceAfter) {
		return invalid(fmt.Sprintf("balance after %s does not match %s %s %s", p.BalanceAfter, p.BalanceBefore, p.Direction, p.Amount))
	}
	return nil
}

func applyDirection(d Direction, balance, amount money.Money) (money.Money, error) {
	var (
		result money.Money
		err    error
	)
	if d == DirectionDebit {
		result, err = balance.Sub(amount)
	} else {
		result, err = balance.Add(amount)
	}
	switch {
	case errors.Is(err, money.ErrCurrencyMismatch):
		return money.Money{}, fmt.Errorf("%w: %w", ErrCurrencyMismatch, err)
	case errors.Is(err, money.ErrOverflow):
		return money.Money{}, fmt.Errorf("%w: %w", ErrBalanceOverflow, err)
	case err != nil:
		return money.Money{}, fmt.Errorf("%w: %w", ErrInvalidAmount, err)
	}
	return result, nil
}

func (e LedgerEntry) ID() uuid.UUID              { return e.id }
func (e LedgerEntry) WalletID() uuid.UUID        { return e.walletID }
func (e LedgerEntry) TransactionID() uuid.UUID   { return e.transactionID }
func (e LedgerEntry) Direction() Direction       { return e.direction }
func (e LedgerEntry) Amount() money.Money        { return e.amount }
func (e LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }
func (e LedgerEntry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e LedgerEntry) WalletVersion() int64       { return e.walletVersion }
func (e LedgerEntry) CreatedAt() time.Time       { return e.createdAt }
