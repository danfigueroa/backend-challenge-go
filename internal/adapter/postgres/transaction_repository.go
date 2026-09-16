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
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

const transactionColumns = `id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id, failure_code,
	result_balance_minor, result_wallet_version, attempts, next_attempt_at, expires_at,
	created_at, updated_at, completed_at`

type TransactionRepository struct {
	conn
}

var _ app.TransactionRepository = (*TransactionRepository)(nil)

func NewTransactionRepository(pool *pgxpool.Pool) *TransactionRepository {
	return &TransactionRepository{conn{pool: pool}}
}

func (r *TransactionRepository) Insert(ctx context.Context, t *wagering.Transaction) error {
	var payloadHash []byte
	if !t.PayloadHash().IsZero() {
		payloadHash = t.PayloadHash().Bytes()
	}
	resultBalance, resultVersion := resultColumns(t)
	_, err := r.q(ctx).Exec(ctx,
		`INSERT INTO wager_transactions (`+transactionColumns+`)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)`,
		t.ID(), string(t.Origin()), string(t.Kind()), string(t.Status()), t.WalletID(), t.PlayerID(),
		t.Money().Currency().Code(), t.Money().Minor(),
		nullString(t.ProviderID()), nullString(t.ExternalTransactionID()), nullString(t.IdempotencyKey()), payloadHash,
		nullString(t.RoundID()), nullString(t.GameID()), nullString(t.ReferenceExternalTransactionID()),
		nullUUID(t.ReferenceTransactionID()), nullString(string(t.FailureCode())),
		resultBalance, resultVersion, t.Attempts(), nullTime(t.NextAttemptAt()), nullTime(t.ExpiresAt()),
		t.CreatedAt(), t.UpdatedAt(), nullTime(t.CompletedAt()))
	return translate(err)
}

func (r *TransactionRepository) Update(ctx context.Context, t *wagering.Transaction) error {
	resultBalance, resultVersion := resultColumns(t)
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE wager_transactions SET
			status = $2, reference_transaction_id = $3, failure_code = $4,
			result_balance_minor = $5, result_wallet_version = $6, attempts = $7,
			next_attempt_at = $8, expires_at = $9, updated_at = $10, completed_at = $11
		 WHERE id = $1 AND status IN ('PENDING', 'PENDING_REFERENCE')`,
		t.ID(), string(t.Status()), nullUUID(t.ReferenceTransactionID()), nullString(string(t.FailureCode())),
		resultBalance, resultVersion, t.Attempts(), nullTime(t.NextAttemptAt()), nullTime(t.ExpiresAt()),
		t.UpdatedAt(), nullTime(t.CompletedAt()))
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: transaction %s is no longer pending", app.ErrConcurrentUpdate, t.ID())
	}
	return nil
}

func (r *TransactionRepository) Get(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error) {
	return scanTransaction(r.q(ctx).QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1`, id))
}

func (r *TransactionRepository) GetForUpdate(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error) {
	tx, err := r.tx(ctx)
	if err != nil {
		return nil, err
	}
	return scanTransaction(tx.QueryRow(ctx, `SELECT `+transactionColumns+` FROM wager_transactions WHERE id = $1 FOR UPDATE`, id))
}

func (r *TransactionRepository) GetByIdempotencyKey(ctx context.Context, providerID, idempotencyKey string) (*wagering.Transaction, error) {
	return scanTransaction(r.q(ctx).QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions WHERE provider_id = $1 AND idempotency_key = $2`,
		providerID, idempotencyKey))
}

func (r *TransactionRepository) GetByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.Transaction, error) {
	return scanTransaction(r.q(ctx).QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2`,
		providerID, externalTransactionID))
}

func (r *TransactionRepository) GetByExternalIDForUpdate(ctx context.Context, providerID, externalTransactionID string) (*wagering.Transaction, error) {
	tx, err := r.tx(ctx)
	if err != nil {
		return nil, err
	}
	return scanTransaction(tx.QueryRow(ctx,
		`SELECT `+transactionColumns+` FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2 FOR UPDATE`,
		providerID, externalTransactionID))
}

func (r *TransactionRepository) HasProcessedReversal(ctx context.Context, referenceTransactionID uuid.UUID) (bool, error) {
	var exists bool
	err := r.q(ctx).QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			WHERE reference_transaction_id = $1 AND status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK')
		 )`,
		referenceTransactionID).Scan(&exists)
	return exists, translate(err)
}

func (r *TransactionRepository) WakeWaitingOn(ctx context.Context, providerID, externalTransactionID string, now time.Time) (int64, error) {
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE wager_transactions
		 SET next_attempt_at = $3, updated_at = GREATEST(updated_at, $3)
		 WHERE status = 'PENDING_REFERENCE'
		   AND provider_id = $1
		   AND reference_external_transaction_id = $2
		   AND next_attempt_at > $3`,
		providerID, externalTransactionID, now)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

func (r *TransactionRepository) ClaimDuePending(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]app.PendingClaim, error) {
	rows, err := r.q(ctx).Query(ctx,
		`WITH due AS (
			SELECT id FROM wager_transactions
			WHERE status IN ('PENDING', 'PENDING_REFERENCE') AND next_attempt_at <= $1
			ORDER BY next_attempt_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		 )
		 UPDATE wager_transactions t
		 SET next_attempt_at = $2, updated_at = GREATEST(t.updated_at, $1)
		 FROM due
		 WHERE t.id = due.id
		 RETURNING t.id, t.wallet_id`,
		now, now.Add(lease), limit)
	if err != nil {
		return nil, translate(err)
	}
	claims, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.PendingClaim, error) {
		var c app.PendingClaim
		if err := row.Scan(&c.TransactionID, &c.WalletID); err != nil {
			return app.PendingClaim{}, fmt.Errorf("scan pending claim: %w", err)
		}
		return c, nil
	})
	if err != nil {
		return nil, translate(err)
	}
	return claims, nil
}

func resultColumns(t *wagering.Transaction) (*int64, *int64) {
	result, ok := t.Result()
	if !ok {
		return nil, nil
	}
	balance, version := result.Balance.Minor(), result.WalletVersion
	return &balance, &version
}

func scanTransaction(row pgx.Row) (*wagering.Transaction, error) {
	var (
		id, walletID, playerID                                  uuid.UUID
		origin, kind, status, currencyCode                      string
		amountMinor                                             int64
		providerID, externalID, idempotencyKey, roundID, gameID *string
		referenceExternalID, failureCode                        *string
		payloadHash                                             []byte
		referenceID                                             *uuid.UUID
		resultBalance, resultVersion                            *int64
		attempts                                                int
		nextAttemptAt, expiresAt, completedAt                   *time.Time
		createdAt, updatedAt                                    time.Time
	)
	if err := row.Scan(&id, &origin, &kind, &status, &walletID, &playerID, &currencyCode, &amountMinor,
		&providerID, &externalID, &idempotencyKey, &payloadHash, &roundID, &gameID,
		&referenceExternalID, &referenceID, &failureCode, &resultBalance, &resultVersion,
		&attempts, &nextAttemptAt, &expiresAt, &createdAt, &updatedAt, &completedAt); err != nil {
		return nil, translate(err)
	}

	amount, err := moneyFromColumns(amountMinor, currencyCode)
	if err != nil {
		return nil, err
	}
	params := wagering.RehydrateParams{
		ID: id, Origin: wagering.Origin(origin), Kind: wagering.Kind(kind), Status: wagering.Status(status),
		WalletID: walletID, PlayerID: playerID, Money: amount,
		ProviderID: valueOr(providerID), ExternalTransactionID: valueOr(externalID), IdempotencyKey: valueOr(idempotencyKey),
		RoundID: valueOr(roundID), GameID: valueOr(gameID), ReferenceExternalTransactionID: valueOr(referenceExternalID),
		ReferenceTransactionID: valueOr(referenceID), FailureCode: wagering.FailureCode(valueOr(failureCode)),
		Attempts: attempts, NextAttemptAt: valueOr(nextAttemptAt), ExpiresAt: valueOr(expiresAt),
		CreatedAt: createdAt, UpdatedAt: updatedAt, CompletedAt: valueOr(completedAt),
	}
	if payloadHash != nil {
		if params.PayloadHash, err = wagering.PayloadHashFromBytes(payloadHash); err != nil {
			return nil, fmt.Errorf("%w: transaction %s: %w", app.ErrIntegrityViolation, id, err)
		}
	}
	if resultBalance != nil && resultVersion != nil {
		balance, err := money.New(*resultBalance, amount.Currency())
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s: %w", app.ErrIntegrityViolation, id, err)
		}
		params.Result = wagering.Result{Balance: balance, WalletVersion: *resultVersion}
	}

	t, err := wagering.Rehydrate(params)
	if err != nil {
		return nil, fmt.Errorf("%w: transaction %s: %w", app.ErrIntegrityViolation, id, err)
	}
	return t, nil
}
