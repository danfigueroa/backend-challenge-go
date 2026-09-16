package app

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound           = errors.New("app: not found")
	ErrConflict           = errors.New("app: conflict")
	ErrConcurrentUpdate   = errors.New("app: concurrent update")
	ErrTransient          = errors.New("app: transient failure")
	ErrIntegrityViolation = errors.New("app: integrity violation")
	ErrTxRequired         = errors.New("app: operation requires an active transaction")
	ErrForbidden          = errors.New("app: forbidden")
	ErrInvalidInput       = errors.New("app: invalid input")
	ErrInboxMismatch      = errors.New("app: message id reused with a different payload")
)

type ValidationError struct {
	Code   string
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("app: %s: %s: %s", e.Code, e.Field, e.Reason)
}

func (e *ValidationError) Is(target error) bool { return target == ErrInvalidInput }

type ConflictKind string

const (
	ConflictUnknown             ConflictKind = "UNKNOWN"
	ConflictWalletExists        ConflictKind = "WALLET_EXISTS"
	ConflictOpeningExists       ConflictKind = "OPENING_EXISTS"
	ConflictIdempotencyKey      ConflictKind = "IDEMPOTENCY_KEY"
	ConflictExternalTransaction ConflictKind = "EXTERNAL_TRANSACTION"
	ConflictReferenceReversed   ConflictKind = "REFERENCE_REVERSED"
	ConflictLedgerEntry         ConflictKind = "LEDGER_ENTRY"
	ConflictInboxMessage        ConflictKind = "INBOX_MESSAGE"
	ConflictDuplicateID         ConflictKind = "DUPLICATE_ID"
)

type ConflictError struct {
	Kind       ConflictKind
	Constraint string
	Err        error
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("app: conflict %s (%s): %v", e.Kind, e.Constraint, e.Err)
}

func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

func (e *ConflictError) Unwrap() error { return e.Err }

func ConflictKindOf(err error) (ConflictKind, bool) {
	if conflict, ok := errors.AsType[*ConflictError](err); ok {
		return conflict.Kind, true
	}
	return "", false
}
