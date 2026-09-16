package wallet

import "errors"

var (
	ErrInvalidWallet      = errors.New("wallet: invalid wallet")
	ErrInvalidLedgerEntry = errors.New("wallet: invalid ledger entry")
	ErrInvalidAmount      = errors.New("wallet: invalid movement amount")
	ErrCurrencyMismatch   = errors.New("wallet: currency mismatch")
	ErrInsufficientFunds  = errors.New("wallet: insufficient funds")
	ErrBalanceOverflow    = errors.New("wallet: balance overflow")
)
