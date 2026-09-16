package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

func TestTranslate(t *testing.T) {
	t.Parallel()

	pgErr := func(code, constraint string) error {
		return fmt.Errorf("exec: %w", &pgconn.PgError{Code: code, ConstraintName: constraint})
	}

	tests := []struct {
		name     string
		err      error
		want     error
		conflict app.ConflictKind
	}{
		{"no rows", pgx.ErrNoRows, app.ErrNotFound, ""},
		{"wallet exists", pgErr("23505", "wallets_player_currency_key"), app.ErrConflict, app.ConflictWalletExists},
		{"idempotency key", pgErr("23505", "wager_transactions_provider_idempotency_key"), app.ErrConflict, app.ConflictIdempotencyKey},
		{"external id", pgErr("23505", "wager_transactions_provider_external_key"), app.ErrConflict, app.ConflictExternalTransaction},
		{"reversal", pgErr("23505", "wager_transactions_single_reversal"), app.ErrConflict, app.ConflictReferenceReversed},
		{"opening", pgErr("23505", "wager_transactions_single_opening"), app.ErrConflict, app.ConflictOpeningExists},
		{"unknown unique", pgErr("23505", "something_else"), app.ErrConflict, app.ConflictUnknown},
		{"check violation", pgErr("23514", "wallets_balance_non_negative"), app.ErrIntegrityViolation, ""},
		{"trigger violation", pgErr("23000", "ledger_entries_append_only"), app.ErrIntegrityViolation, ""},
		{"foreign key", pgErr("23503", "ledger_entries_wallet_fk"), app.ErrIntegrityViolation, ""},
		{"serialization", pgErr("40001", ""), app.ErrTransient, ""},
		{"deadlock", pgErr("40P01", ""), app.ErrTransient, ""},
		{"lock timeout", pgErr("55P03", ""), app.ErrTransient, ""},
		{"statement timeout", pgErr("57014", ""), app.ErrTransient, ""},
		{"admin shutdown", pgErr("57P01", ""), app.ErrTransient, ""},
		{"too many connections", pgErr("53300", ""), app.ErrTransient, ""},
		{"connection failure", pgErr("08006", ""), app.ErrTransient, ""},
		{"connect error", &pgconn.ConnectError{}, app.ErrTransient, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := translate(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("translate = %v, want %v", got, tc.want)
			}
			if tc.conflict != "" {
				if kind, ok := app.ConflictKindOf(got); !ok || kind != tc.conflict {
					t.Errorf("conflict kind = %v, want %s", kind, tc.conflict)
				}
			}
			if !errors.Is(got, tc.err) && !errors.Is(tc.err, pgx.ErrNoRows) {
				if _, ok := errors.AsType[*pgconn.PgError](got); !ok {
					t.Errorf("original error lost: %v", got)
				}
			}
		})
	}

	if translate(nil) != nil {
		t.Error("translate(nil) != nil")
	}
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := translate(ctxErr); !errors.Is(got, ctxErr) || errors.Is(got, app.ErrTransient) {
			t.Errorf("context error %v translated to %v", ctxErr, got)
		}
	}
	if got := translate(pgErr("42501", "")); errors.Is(got, app.ErrTransient) || errors.Is(got, app.ErrIntegrityViolation) {
		t.Errorf("permission denied misclassified: %v", got)
	}
}
