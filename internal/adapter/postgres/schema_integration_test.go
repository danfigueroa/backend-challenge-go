//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func inTx(ctx context.Context, t *testing.T, conn interface {
	Begin(context.Context) (pgx.Tx, error)
}, fn func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func TestWalletConstraints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	app := f.db.AppPool
	w := f.openWallet(t, "100.00")

	base := row{
		"id": uuid.New(), "player_id": uuid.New(), "currency": "BRL", "balance_minor": int64(0),
		"version": int64(1), "created_at": now(), "updated_at": now(),
	}

	t.Run("negative balance", func(t *testing.T) {
		err := insertRow(ctx, app, "wallets", base.with(row{"id": uuid.New(), "balance_minor": int64(-1)}))
		expectConstraint(t, err, "wallets_balance_non_negative")
	})
	t.Run("version below one", func(t *testing.T) {
		err := insertRow(ctx, app, "wallets", base.with(row{"id": uuid.New(), "version": int64(0)}))
		expectConstraint(t, err, "wallets_version_positive")
	})
	t.Run("invalid currency", func(t *testing.T) {
		err := insertRow(ctx, app, "wallets", base.with(row{"id": uuid.New(), "currency": "br1"}))
		expectConstraint(t, err, "wallets_currency_format")
	})
	t.Run("duplicate player and currency", func(t *testing.T) {
		err := insertRow(ctx, app, "wallets", base.with(row{"id": uuid.New(), "player_id": w.PlayerID()}))
		expectConstraint(t, err, "wallets_player_currency_key")
	})
	t.Run("positive balance without opening entry fails at commit", func(t *testing.T) {
		err := inTx(ctx, t, app, func(tx pgx.Tx) error {
			return insertRow(ctx, tx, "wallets", base.with(row{"id": uuid.New(), "player_id": uuid.New(), "balance_minor": int64(500)}))
		})
		expectConstraint(t, err, "wallets_ledger_consistency")
	})
	t.Run("balance change without ledger entry fails at commit", func(t *testing.T) {
		err := inTx(ctx, t, app, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "UPDATE wallets SET balance_minor = 1, version = 2 WHERE id = $1", w.ID())
			return err
		})
		expectConstraint(t, err, "wallets_ledger_consistency")
	})
	t.Run("version must follow balance", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wallets SET balance_minor = 1, version = 5 WHERE id = $1", w.ID())
		expectConstraint(t, err, "wallets_version_follows_balance")
		_, err = app.Exec(ctx, "UPDATE wallets SET version = 2 WHERE id = $1", w.ID())
		expectConstraint(t, err, "wallets_version_follows_balance")
	})
	t.Run("identity is not updatable by the application", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wallets SET player_id = $1 WHERE id = $2", uuid.New(), w.ID())
		expectPermissionDenied(t, err)
	})
	t.Run("identity is immutable even for the owner", func(t *testing.T) {
		_, err := f.db.OwnerPool.Exec(ctx, "UPDATE wallets SET currency = 'USD' WHERE id = $1", w.ID())
		expectConstraint(t, err, "wallets_identity_immutable")
	})
	t.Run("wallets cannot be deleted", func(t *testing.T) {
		_, err := app.Exec(ctx, "DELETE FROM wallets WHERE id = $1", w.ID())
		expectPermissionDenied(t, err)
		_, err = f.db.OwnerPool.Exec(ctx, "DELETE FROM wallets WHERE id = $1", w.ID())
		expectConstraint(t, err, "wallets_append_only")
	})
}

func TestLedgerConstraints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	app := f.db.AppPool
	w := f.openWallet(t, "100.00")
	bet := f.mustProcess(t, w, opInput{kind: "BET", externalID: "bet-1", amount: "30.00"})

	loss := lossRow(w)
	if err := insertRow(ctx, app, "wager_transactions", loss); err != nil {
		t.Fatal(err)
	}

	entry := row{
		"id": uuid.New(), "wallet_id": w.ID(), "currency": "BRL", "transaction_id": bet.ID(),
		"direction": "DEBIT", "amount_minor": int64(1000), "balance_before_minor": int64(7000),
		"balance_after_minor": int64(6000), "wallet_version": int64(3), "created_at": now(),
	}

	t.Run("arithmetic", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{"balance_after_minor": int64(6001)}))
		expectConstraint(t, err, "ledger_entries_arithmetic")
	})
	t.Run("amount must be positive", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{"amount_minor": int64(0), "balance_after_minor": int64(7000)}))
		expectConstraint(t, err, "ledger_entries_amount_positive")
	})
	t.Run("negative balance after", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{"amount_minor": int64(8000), "balance_after_minor": int64(-1000)}))
		expectConstraint(t, err, "ledger_entries_balances_non_negative")
	})
	t.Run("duplicate wallet and transaction", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry)
		expectConstraint(t, err, "ledger_entries_wallet_transaction_key")
	})
	t.Run("duplicate wallet version", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{
			"transaction_id": loss["id"], "wallet_version": int64(2), "balance_before_minor": int64(10000), "balance_after_minor": int64(9000),
		}))
		expectConstraint(t, err, "ledger_entries_wallet_version_key")
	})
	t.Run("chain break", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{"transaction_id": loss["id"], "balance_before_minor": int64(9000), "balance_after_minor": int64(8000)}))
		expectConstraint(t, err, "ledger_entries_chain")
	})
	t.Run("currency must match wallet", func(t *testing.T) {
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{"transaction_id": loss["id"], "currency": "USD"}))
		expectConstraint(t, err, "ledger_entries_wallet_fk")
	})
	t.Run("transaction must belong to wallet", func(t *testing.T) {
		other := f.openWallet(t, "10.00")
		err := insertRow(ctx, app, "ledger_entries", entry.with(row{"wallet_id": other.ID(), "wallet_version": int64(2), "balance_before_minor": int64(1000), "balance_after_minor": int64(0)}))
		expectConstraint(t, err, "ledger_entries_transaction_fk")
	})
	t.Run("entry must match a processed transaction at commit", func(t *testing.T) {
		err := inTx(ctx, t, app, func(tx pgx.Tx) error {
			if err := insertRow(ctx, tx, "ledger_entries", entry.with(row{"transaction_id": loss["id"]})); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "UPDATE wallets SET balance_minor = 6000, version = 3 WHERE id = $1", w.ID())
			return err
		})
		expectConstraint(t, err, "ledger_entries_transaction_consistency")
	})
	t.Run("append only for the application", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE ledger_entries SET amount_minor = 1")
		expectPermissionDenied(t, err)
		_, err = app.Exec(ctx, "DELETE FROM ledger_entries")
		expectPermissionDenied(t, err)
	})
	t.Run("append only even for the owner", func(t *testing.T) {
		_, err := f.db.OwnerPool.Exec(ctx, "UPDATE ledger_entries SET created_at = now()")
		expectConstraint(t, err, "ledger_entries_append_only")
		_, err = f.db.OwnerPool.Exec(ctx, "DELETE FROM ledger_entries")
		expectConstraint(t, err, "ledger_entries_append_only")
		_, err = f.db.OwnerPool.Exec(ctx, "TRUNCATE ledger_entries CASCADE")
		expectConstraint(t, err, "ledger_entries_append_only")
	})
}

func TestTransactionConstraints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	app := f.db.AppPool
	w := f.openWallet(t, "100.00")
	bet := f.mustProcess(t, w, opInput{kind: "BET", externalID: "bet-1", amount: "30.00"})
	w, err := f.wallets.Get(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}

	insert := func(r row) error { return insertRow(ctx, app, "wager_transactions", r) }

	t.Run("loss amount must be zero", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"amount_minor": int64(1)})), "wager_transactions_amount_policy")
	})
	t.Run("bet amount must be positive", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"kind": "BET"})), "wager_transactions_amount_policy")
	})
	t.Run("external opening is forbidden", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"kind": "OPENING", "amount_minor": int64(1)})), "wager_transactions_external_shape")
	})
	t.Run("internal transaction without external metadata", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"origin": "INTERNAL", "kind": "OPENING", "amount_minor": int64(1)})), "wager_transactions_internal_shape")
	})
	t.Run("external requires payload hash", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"payload_hash": []byte{1, 2, 3}})), "wager_transactions_external_shape")
	})
	t.Run("refund requires reference", func(t *testing.T) {
		r := lossRow(w).with(row{"kind": "REFUND", "amount_minor": int64(100), "status": "REJECTED", "failure_code": "REFERENCE_NOT_FOUND"})
		expectConstraint(t, insert(r), "wager_transactions_reference_shape")
	})
	t.Run("processed requires result", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"result_balance_minor": nil, "result_wallet_version": nil})), "wager_transactions_status_shape")
	})
	t.Run("pending reference requires schedule", func(t *testing.T) {
		r := lossRow(w).with(row{"kind": "WIN", "amount_minor": int64(100), "status": "PENDING_REFERENCE", "reference_external_transaction_id": "bet-x",
			"result_balance_minor": nil, "result_wallet_version": nil, "completed_at": nil})
		expectConstraint(t, insert(r), "wager_transactions_status_shape")
	})
	t.Run("wallet player and currency must match", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"player_id": uuid.New()})), "wager_transactions_wallet_fk")
		expectConstraint(t, insert(lossRow(w).with(row{"currency": "USD"})), "wager_transactions_wallet_fk")
	})
	t.Run("duplicate idempotency key per provider", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"idempotency_key": bet.IdempotencyKey()})), "wager_transactions_provider_idempotency_key")
	})
	t.Run("duplicate external id per provider", func(t *testing.T) {
		expectConstraint(t, insert(lossRow(w).with(row{"external_transaction_id": bet.ExternalTransactionID()})), "wager_transactions_provider_external_key")
	})
	t.Run("same external id is allowed for another provider", func(t *testing.T) {
		if err := insert(lossRow(w).with(row{"provider_id": "provider-b", "external_transaction_id": bet.ExternalTransactionID()})); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("single opening per wallet", func(t *testing.T) {
		at := now()
		err := insert(row{
			"id": uuid.New(), "origin": "INTERNAL", "kind": "OPENING", "status": "REJECTED", "failure_code": "INSUFFICIENT_FUNDS",
			"wallet_id": w.ID(), "player_id": w.PlayerID(), "currency": "BRL", "amount_minor": int64(1),
			"created_at": at, "updated_at": at, "completed_at": at,
		})
		expectConstraint(t, err, "wager_transactions_single_opening")
	})
	t.Run("processed bet requires ledger entry at commit", func(t *testing.T) {
		err := inTx(ctx, t, app, func(tx pgx.Tx) error {
			return insertRow(ctx, tx, "wager_transactions", lossRow(w).with(row{"kind": "BET", "amount_minor": int64(100)}))
		})
		expectConstraint(t, err, "wager_transactions_ledger_consistency")
	})
	t.Run("terminal transactions are immutable", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wager_transactions SET status = 'FAILED', failure_code = 'INTERNAL_PROCESSING_FAILED' WHERE id = $1", bet.ID())
		expectConstraint(t, err, "wager_transactions_terminal_immutable")
	})
	t.Run("request data is not updatable by the application", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wager_transactions SET amount_minor = 1 WHERE id = $1", bet.ID())
		expectPermissionDenied(t, err)
	})
	t.Run("transactions cannot be deleted", func(t *testing.T) {
		_, err := f.db.OwnerPool.Exec(ctx, "DELETE FROM wager_transactions WHERE id = $1", bet.ID())
		expectConstraint(t, err, "wager_transactions_append_only")
	})
}

func TestPendingTransactionGuards(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	app := f.db.AppPool
	w := f.openWallet(t, "100.00")
	pending, _, err := f.process(ctx, t, w.ID(), f.request(t, w, opInput{kind: "REFUND", externalID: "refund-1", amount: "10.00", reference: "bet-404"}))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("expiration is immutable", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wager_transactions SET expires_at = expires_at + interval '1 day' WHERE id = $1", pending.ID())
		expectConstraint(t, err, "wager_transactions_expiration_immutable")
	})
	t.Run("attempts cannot decrease", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wager_transactions SET attempts = 0 WHERE id = $1", pending.ID())
		expectConstraint(t, err, "wager_transactions_attempts_monotonic")
	})
	t.Run("cannot return to pending", func(t *testing.T) {
		_, err := app.Exec(ctx, "UPDATE wager_transactions SET status = 'PENDING' WHERE id = $1", pending.ID())
		expectConstraint(t, err, "wager_transactions_status_transition")
	})
}

func TestReversalUniquenessIsEnforcedByTheDatabase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	w := f.openWallet(t, "100.00")
	bet := f.mustProcess(t, w, opInput{kind: "BET", externalID: "bet-1", amount: "40.00"})
	refund := f.mustProcess(t, w, opInput{kind: "REFUND", externalID: "refund-1", amount: "40.00", reference: "bet-1"})
	if refund.ReferenceTransactionID() != bet.ID() {
		t.Fatalf("refund reference = %s", refund.ReferenceTransactionID())
	}
	current, err := f.wallets.Get(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}

	err = inTx(ctx, t, f.db.AppPool, func(tx pgx.Tx) error {
		at := now()
		external := "rollback-bypassing-domain"
		if err := insertRow(ctx, tx, "wager_transactions", row{
			"id": uuid.New(), "origin": "EXTERNAL", "kind": "ROLLBACK", "status": "PROCESSED",
			"wallet_id": w.ID(), "player_id": w.PlayerID(), "currency": "BRL", "amount_minor": int64(4000),
			"provider_id": "provider-a", "external_transaction_id": external, "idempotency_key": "provider-a:" + external,
			"payload_hash": hashOf(external), "round_id": "round-1", "game_id": "game-1",
			"reference_external_transaction_id": "bet-1", "reference_transaction_id": bet.ID(),
			"result_balance_minor": current.Balance().Minor() + 4000, "result_wallet_version": current.Version() + 1,
			"created_at": at, "updated_at": at, "completed_at": at,
		}); err != nil {
			return err
		}
		return nil
	})
	expectConstraint(t, err, "wager_transactions_single_reversal")
}

func TestInboxAndOutboxConstraints(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	app := f.db.AppPool
	at := now()

	t.Run("inbox hash length", func(t *testing.T) {
		err := insertRow(ctx, app, "inbox_messages", row{
			"consumer_name": "c", "message_id": "m-short", "payload_hash": []byte{1}, "received_at": at,
		})
		expectConstraint(t, err, "inbox_messages_hash_length")
	})
	t.Run("inbox processed message is immutable", func(t *testing.T) {
		if err := insertRow(ctx, app, "inbox_messages", row{
			"consumer_name": "c", "message_id": "m-1", "payload_hash": hashOf("m-1"), "received_at": at, "processed_at": at,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := app.Exec(ctx, "UPDATE inbox_messages SET processed_at = now() WHERE message_id = 'm-1'")
		expectConstraint(t, err, "inbox_messages_processed_immutable")
		_, err = app.Exec(ctx, "DELETE FROM inbox_messages WHERE message_id = 'm-1'")
		expectPermissionDenied(t, err)
	})

	event := row{
		"id": uuid.New(), "aggregate_type": "Wallet", "aggregate_id": uuid.New(), "event_type": "WalletBalanceChanged",
		"event_version": 1, "partition_key": "p", "correlation_id": "c", "payload": `{"a":1}`,
		"occurred_at": at, "next_attempt_at": at,
	}
	if err := insertRow(ctx, app, "outbox_events", event); err != nil {
		t.Fatal(err)
	}

	t.Run("outbox payload must be valid json", func(t *testing.T) {
		err := insertRow(ctx, app, "outbox_events", event.with(row{"id": uuid.New(), "payload": "not json"}))
		if err == nil {
			t.Fatal("invalid JSON payload accepted")
		}
	})
	t.Run("outbox snapshot is not updatable by the application", func(t *testing.T) {
		_, err := app.Exec(ctx, `UPDATE outbox_events SET payload = '{"a":2}' WHERE id = $1`, event["id"])
		expectPermissionDenied(t, err)
	})
	t.Run("outbox snapshot is immutable for the owner", func(t *testing.T) {
		_, err := f.db.OwnerPool.Exec(ctx, `UPDATE outbox_events SET payload = '{"a":2}' WHERE id = $1`, event["id"])
		expectConstraint(t, err, "outbox_events_snapshot_immutable")
	})
	t.Run("unpublished events cannot be deleted", func(t *testing.T) {
		_, err := app.Exec(ctx, "DELETE FROM outbox_events WHERE id = $1", event["id"])
		expectConstraint(t, err, "outbox_events_unpublished_retained")
	})
	t.Run("publication is final and deletable afterwards", func(t *testing.T) {
		if _, err := app.Exec(ctx, "UPDATE outbox_events SET published_at = now() WHERE id = $1", event["id"]); err != nil {
			t.Fatal(err)
		}
		_, err := app.Exec(ctx, "UPDATE outbox_events SET published_at = now() + interval '1 hour' WHERE id = $1", event["id"])
		expectConstraint(t, err, "outbox_events_publication_final")
		if _, err := app.Exec(ctx, "DELETE FROM outbox_events WHERE id = $1", event["id"]); err != nil {
			t.Fatalf("published event should be removable by retention: %v", err)
		}
	})
}
