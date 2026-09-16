package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

var conflictKinds = map[string]app.ConflictKind{
	"wallets_pkey":                                app.ConflictDuplicateID,
	"wallets_player_currency_key":                 app.ConflictWalletExists,
	"wager_transactions_pkey":                     app.ConflictDuplicateID,
	"wager_transactions_single_opening":           app.ConflictOpeningExists,
	"wager_transactions_provider_idempotency_key": app.ConflictIdempotencyKey,
	"wager_transactions_provider_external_key":    app.ConflictExternalTransaction,
	"wager_transactions_single_reversal":          app.ConflictReferenceReversed,
	"ledger_entries_pkey":                         app.ConflictLedgerEntry,
	"ledger_entries_wallet_transaction_key":       app.ConflictLedgerEntry,
	"ledger_entries_wallet_version_key":           app.ConflictLedgerEntry,
	"inbox_messages_pkey":                         app.ConflictInboxMessage,
	"outbox_events_pkey":                          app.ConflictDuplicateID,
}

var transientCodes = map[string]bool{
	"40001": true,
	"40P01": true,
	"55P03": true,
	"57014": true,
	"57P01": true,
	"57P02": true,
	"57P03": true,
	"53300": true,
	"53400": true,
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %w", app.ErrNotFound, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}

	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch {
		case pgErr.Code == "23505":
			kind, known := conflictKinds[pgErr.ConstraintName]
			if !known {
				kind = app.ConflictUnknown
			}
			return &app.ConflictError{Kind: kind, Constraint: pgErr.ConstraintName, Err: err}
		case strings.HasPrefix(pgErr.Code, "23"):
			return fmt.Errorf("%w: %s: %w", app.ErrIntegrityViolation, pgErr.ConstraintName, err)
		case strings.HasPrefix(pgErr.Code, "08"), transientCodes[pgErr.Code]:
			return fmt.Errorf("%w: %w", app.ErrTransient, err)
		}
		return fmt.Errorf("postgres: %w", err)
	}

	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return fmt.Errorf("%w: %w", app.ErrTransient, err)
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return fmt.Errorf("%w: %w", app.ErrTransient, err)
	}
	if pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return fmt.Errorf("%w: %w", app.ErrTransient, err)
	}
	return fmt.Errorf("postgres: %w", err)
}
