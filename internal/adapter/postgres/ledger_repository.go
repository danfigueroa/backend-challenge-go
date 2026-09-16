package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

const ledgerColumns = `id, wallet_id, currency, transaction_id, direction, amount_minor,
	balance_before_minor, balance_after_minor, wallet_version, created_at`

type LedgerRepository struct {
	conn
}

var _ app.LedgerRepository = (*LedgerRepository)(nil)

func NewLedgerRepository(pool *pgxpool.Pool) *LedgerRepository {
	return &LedgerRepository{conn{pool: pool}}
}

func (r *LedgerRepository) Insert(ctx context.Context, e wallet.LedgerEntry) error {
	_, err := r.q(ctx).Exec(ctx,
		`INSERT INTO ledger_entries (`+ledgerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID(), e.WalletID(), e.Amount().Currency().Code(), e.TransactionID(), e.Direction().String(), e.Amount().Minor(),
		e.BalanceBefore().Minor(), e.BalanceAfter().Minor(), e.WalletVersion(), e.CreatedAt())
	return translate(err)
}

func (r *LedgerRepository) List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error) {
	rows, err := r.q(ctx).Query(ctx,
		`SELECT `+ledgerColumns+` FROM ledger_entries
		 WHERE wallet_id = $1 AND wallet_version > $2
		 ORDER BY wallet_version
		 LIMIT $3`,
		walletID, afterVersion, limit)
	if err != nil {
		return nil, translate(err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (wallet.LedgerEntry, error) {
		return scanLedgerEntry(row)
	})
	if err != nil {
		return nil, translate(err)
	}
	return entries, nil
}

func (r *LedgerRepository) Summarize(ctx context.Context, walletID uuid.UUID) (app.LedgerSummary, error) {
	var s app.LedgerSummary
	err := r.q(ctx).QueryRow(ctx,
		`SELECT
			count(*),
			COALESCE(sum(CASE WHEN direction = 'CREDIT' THEN amount_minor ELSE -amount_minor END), 0)::BIGINT,
			COALESCE(max(wallet_version), 0),
			COALESCE((array_agg(balance_after_minor ORDER BY wallet_version DESC))[1], 0),
			count(*) FILTER (WHERE previous_balance IS NOT NULL AND previous_balance <> balance_before_minor)
		 FROM (
			SELECT direction, amount_minor, balance_before_minor, balance_after_minor, wallet_version,
				lag(balance_after_minor) OVER (ORDER BY wallet_version) AS previous_balance
			FROM ledger_entries
			WHERE wallet_id = $1
		 ) entries`,
		walletID).Scan(&s.Entries, &s.NetMinor, &s.LastVersion, &s.LastBalanceMinor, &s.ChainBreaks)
	if err != nil {
		return app.LedgerSummary{}, translate(err)
	}
	return s, nil
}

func scanLedgerEntry(row pgx.Row) (wallet.LedgerEntry, error) {
	var (
		id, walletID, transactionID uuid.UUID
		currencyCode, direction     string
		amount, before, after       int64
		version                     int64
		createdAt                   time.Time
	)
	if err := row.Scan(&id, &walletID, &currencyCode, &transactionID, &direction, &amount, &before, &after, &version, &createdAt); err != nil {
		return wallet.LedgerEntry{}, translate(err)
	}
	amountMoney, err := moneyFromColumns(amount, currencyCode)
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	beforeMoney, err := moneyFromColumns(before, currencyCode)
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	afterMoney, err := moneyFromColumns(after, currencyCode)
	if err != nil {
		return wallet.LedgerEntry{}, err
	}
	entry, err := wallet.RehydrateLedgerEntry(wallet.LedgerEntryParams{
		ID: id, WalletID: walletID, TransactionID: transactionID, Direction: wallet.Direction(direction),
		Amount: amountMoney, BalanceBefore: beforeMoney, BalanceAfter: afterMoney, WalletVersion: version, CreatedAt: createdAt,
	})
	if err != nil {
		return wallet.LedgerEntry{}, fmt.Errorf("%w: ledger entry %s: %w", app.ErrIntegrityViolation, id, err)
	}
	return entry, nil
}
