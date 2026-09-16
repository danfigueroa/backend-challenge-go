//go:build integration

package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var pg *pgtest.Instance

func TestMain(m *testing.M) {
	os.Exit(pgtest.RunMain(m, &pg))
}

var testPolicy = wagering.PendingPolicy{TTL: time.Hour, BaseDelay: time.Second, MaxDelay: time.Minute}

type fixture struct {
	db      *pgtest.Database
	txm     *postgres.TxManager
	wallets *postgres.WalletRepository
	txs     *postgres.TransactionRepository
	ledger  *postgres.LedgerRepository
	inbox   *postgres.InboxRepository
	outbox  *postgres.OutboxRepository
}

func testConfig() postgres.Config {
	return postgres.Config{DSN: "unused", MaxConns: 10, LockTimeout: 2 * time.Second, StatementTimeout: 5 * time.Second}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pg.NewDatabase(t)
	return &fixture{
		db:      db,
		txm:     postgres.NewTxManager(db.AppPool, testConfig()),
		wallets: postgres.NewWalletRepository(db.AppPool),
		txs:     postgres.NewTransactionRepository(db.AppPool),
		ledger:  postgres.NewLedgerRepository(db.AppPool),
		inbox:   postgres.NewInboxRepository(db.AppPool),
		outbox:  postgres.NewOutboxRepository(db.AppPool),
	}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *fixture) openWallet(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	ctx := ctxT(t)
	at := now()
	walletID, playerID, openingID := uuid.New(), uuid.New(), uuid.New()

	opened, err := wallet.Open(wallet.OpenParams{
		ID: walletID, PlayerID: playerID, InitialBalance: brl(t, initial),
		OpeningTransactionID: openingID, OpeningEntryID: uuid.New(), Now: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.wallets.Insert(ctx, opened.Wallet); err != nil {
			return err
		}
		if opened.OpeningEntry == nil {
			return nil
		}
		opening, err := wagering.NewOpening(wagering.OpeningParams{
			ID: openingID, WalletID: walletID, PlayerID: playerID, Amount: opened.Wallet.Balance(), Now: at,
		})
		if err != nil {
			return err
		}
		if err := opening.MarkProcessed(wagering.Result{Balance: opened.Wallet.Balance(), WalletVersion: 1}, uuid.Nil, at); err != nil {
			return err
		}
		if err := f.txs.Insert(ctx, opening); err != nil {
			return err
		}
		return f.ledger.Insert(ctx, *opened.OpeningEntry)
	})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	return opened.Wallet
}

type opInput struct {
	provider   string
	kind       string
	externalID string
	amount     string
	reference  string
}

func (f *fixture) request(t *testing.T, w *wallet.Wallet, in opInput) wagering.Request {
	t.Helper()
	if in.provider == "" {
		in.provider = "provider-a"
	}
	req, err := wagering.NewRequest(wagering.RequestInput{
		ProviderID: in.provider, ExternalTransactionID: in.externalID, IdempotencyKey: in.provider + ":" + in.externalID,
		PlayerID: w.PlayerID().String(), WalletID: w.ID().String(), RoundID: "round-1", GameID: "game-1",
		Kind: in.kind, Amount: in.amount, Currency: "BRL", ReferenceExternalTransactionID: in.reference,
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func (f *fixture) process(ctx context.Context, t *testing.T, walletID uuid.UUID, req wagering.Request) (*wagering.Transaction, wagering.Decision, error) {
	t.Helper()
	var (
		tx       *wagering.Transaction
		decision wagering.Decision
	)
	err := f.txm.WithinTx(ctx, func(ctx context.Context) error {
		w, err := f.wallets.GetForUpdate(ctx, walletID)
		if err != nil {
			return err
		}
		at := now()
		tx, err = wagering.NewExternal(uuid.New(), req, at)
		if err != nil {
			return err
		}
		var ref *wagering.Reference
		if req.ReferenceExternalTransactionID() != "" {
			refTx, err := f.txs.GetByExternalIDForUpdate(ctx, req.ProviderID(), req.ReferenceExternalTransactionID())
			switch {
			case err == nil:
				reversed, err := f.txs.HasProcessedReversal(ctx, refTx.ID())
				if err != nil {
					return err
				}
				ref = &wagering.Reference{Transaction: refTx, AlreadyReversed: reversed}
			case !isNotFound(err):
				return err
			}
		}
		decision, err = wagering.Process(wagering.ProcessParams{
			Transaction: tx, Wallet: w, Reference: ref, EntryID: uuid.New(), Now: at, Policy: testPolicy,
		})
		if err != nil {
			return err
		}
		if err := f.txs.Insert(ctx, tx); err != nil {
			return err
		}
		if err := f.wallets.Update(ctx, w); err != nil {
			return err
		}
		if decision.Entry != nil {
			return f.ledger.Insert(ctx, *decision.Entry)
		}
		return nil
	})
	return tx, decision, err
}

func (f *fixture) mustProcess(t *testing.T, w *wallet.Wallet, in opInput) *wagering.Transaction {
	t.Helper()
	tx, _, err := f.process(ctxT(t), t, w.ID(), f.request(t, w, in))
	if err != nil {
		t.Fatalf("process %s %s: %v", in.kind, in.externalID, err)
	}
	return tx
}

func isNotFound(err error) bool {
	return errors.Is(err, app.ErrNotFound)
}

func expectConstraint(t *testing.T, err error, constraint string) {
	t.Helper()
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		t.Fatalf("expected violation of %s, got %v", constraint, err)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("expected violation of %s, got %s (%s): %s", constraint, pgErr.ConstraintName, pgErr.Code, pgErr.Message)
	}
}

func expectPermissionDenied(t *testing.T, err error) {
	t.Helper()
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok || pgErr.Code != "42501" {
		t.Fatalf("expected permission denied (42501), got %v", err)
	}
}

type row map[string]any

func (r row) with(overrides row) row {
	out := make(row, len(r)+len(overrides))
	maps.Copy(out, r)
	maps.Copy(out, overrides)
	return out
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertRow(ctx context.Context, q execer, table string, r row) error {
	columns := make([]string, 0, len(r))
	for column := range r {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	placeholders := make([]string, len(columns))
	args := make([]any, len(columns))
	for i, column := range columns {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = r[column]
	}
	_, err := q.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(columns, ", "), strings.Join(placeholders, ", ")), args...)
	return err
}

func hashOf(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func lossRow(w *wallet.Wallet) row {
	at := now()
	external := uuid.NewString()
	return row{
		"id": uuid.New(), "origin": "EXTERNAL", "kind": "LOSS", "status": "PROCESSED",
		"wallet_id": w.ID(), "player_id": w.PlayerID(), "currency": "BRL", "amount_minor": int64(0),
		"provider_id": "provider-a", "external_transaction_id": external, "idempotency_key": "provider-a:" + external,
		"payload_hash": hashOf(external), "round_id": "round-1", "game_id": "game-1",
		"result_balance_minor": w.Balance().Minor(), "result_wallet_version": w.Version(),
		"created_at": at, "updated_at": at, "completed_at": at,
	}
}
