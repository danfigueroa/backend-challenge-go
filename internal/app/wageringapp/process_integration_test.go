//go:build integration

package wageringapp_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/apptest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var pg *pgtest.Instance

func TestMain(m *testing.M) {
	os.Exit(pgtest.RunMain(m, &pg))
}

func resultBalance(t *testing.T, tx *wagering.Transaction) string {
	t.Helper()
	r, ok := tx.Result()
	if !ok {
		t.Fatalf("transaction %s (%s) has no result", tx.ID(), tx.Status())
	}
	return r.Balance.Amount()
}

func TestBetIsProcessedAndReplayReturnsOriginalBalance(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "1000.00")

	first := h.Process(t, apptest.Input(w, "BET", "transaction-123", "25.00", ""))
	if first.IdempotentReplay || first.Transaction.Status() != wagering.StatusProcessed || resultBalance(t, first.Transaction) != "975.00" {
		t.Fatalf("first = %+v", first)
	}

	h.Process(t, apptest.Input(w, "WIN", "win-1", "100.00", ""))

	replay := h.Process(t, apptest.Input(w, "BET", "transaction-123", "25.00", ""))
	if !replay.IdempotentReplay || replay.Transaction.ID() != first.Transaction.ID() || resultBalance(t, replay.Transaction) != "975.00" {
		t.Errorf("replay = %+v, balance %s", replay, resultBalance(t, replay.Transaction))
	}

	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Errorf("debits = %d, want 1", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", first.Transaction.ID()); n != 1 {
		t.Errorf("events for the bet = %d, replay must not publish again", n)
	}
	if h.Observer.Replays() != 1 {
		t.Errorf("replays observed = %d", h.Observer.Replays())
	}
	h.AssertAllWalletsReconcile(t)
}

func TestIdempotencyConflicts(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")
	original := h.Process(t, apptest.Input(w, "BET", "bet-1", "10.00", ""))

	process := func(in wagering.RequestInput) error {
		_, err := h.Wagering.Process(ctx, wageringapp.ProcessCommand{Actor: apptest.ProviderA, Input: in})
		return err
	}

	sameKeyOtherAmount := apptest.Input(w, "BET", "bet-1", "11.00", "")
	err := process(sameKeyOtherAmount)
	conflict, ok := errors.AsType[*wageringapp.IdempotencyConflictError](err)
	if !ok || conflict.Code != wagering.CodeIdempotencyKeyConflict || conflict.ExistingTransactionID != original.Transaction.ID() || !errors.Is(err, app.ErrConflict) {
		t.Errorf("same key, different payload: %v", err)
	}

	otherKey := apptest.Input(w, "BET", "bet-1", "10.00", "")
	otherKey.IdempotencyKey = "retry-with-another-key"
	err = process(otherKey)
	if conflict, ok := errors.AsType[*wageringapp.IdempotencyConflictError](err); !ok || conflict.Code != wagering.CodeExternalTransactionConflict {
		t.Errorf("same external id, different key: %v", err)
	}

	if n := h.Count(t, "SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'BET'", w.ID); n != 1 {
		t.Errorf("bets persisted = %d", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1", w.ID); n != 2 {
		t.Errorf("ledger entries = %d", n)
	}
	if len(h.Observer.Conflicts) != 2 {
		t.Errorf("conflicts observed = %v", h.Observer.Conflicts)
	}
}

func TestRejectionsAreDistinguishableAndPersisted(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "10.00")

	rejected := h.Process(t, apptest.Input(w, "BET", "too-big", "10.01", ""))
	if rejected.Transaction.Status() != wagering.StatusRejected || rejected.Transaction.FailureCode() != wagering.CodeInsufficientFunds || resultBalance(t, rejected.Transaction) != "10.00" {
		t.Errorf("rejected = %s %s", rejected.Transaction.Status(), rejected.Transaction.FailureCode())
	}
	replay := h.Process(t, apptest.Input(w, "BET", "too-big", "10.01", ""))
	if !replay.IdempotentReplay || replay.Transaction.FailureCode() != wagering.CodeInsufficientFunds {
		t.Errorf("replay of rejection = %+v", replay)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionRejected'", rejected.Transaction.ID()); n != 1 {
		t.Errorf("rejection events = %d", n)
	}
}

func TestLossProcessesWithoutMovingTheWallet(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "10.00")

	loss := h.Process(t, apptest.Input(w, "LOSS", "loss-1", "0.00", ""))
	if loss.Transaction.Status() != wagering.StatusProcessed || resultBalance(t, loss.Transaction) != "10.00" {
		t.Errorf("loss = %s", loss.Transaction.Status())
	}
	if n := h.Count(t, "SELECT version FROM wallets WHERE id = $1", w.ID); n != 1 {
		t.Errorf("wallet version = %d, want 1", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", loss.Transaction.ID()); n != 1 {
		t.Errorf("loss events = %d", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE partition_key = $1 AND event_type = 'WalletBalanceChanged'", w.ID.String()); n != 1 {
		t.Errorf("balance events = %d (only the opening)", n)
	}
}

func TestCorrectableErrorsAreNotPersisted(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")
	usd, err := h.Wallets.OpenWallet(ctx, walletappOpen(w.PlayerID.String(), "USD"))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		input wagering.RequestInput
		code  wagering.FailureCode
	}{
		"opening kind":       {apptest.Input(w, "OPENING", "op-1", "1.00", ""), wagering.CodeOpeningNotAllowed},
		"float amount":       {apptest.Input(w, "BET", "b-1", "1.5", ""), wagering.CodeInvalidAmount},
		"refund without ref": {apptest.Input(w, "REFUND", "r-1", "1.00", ""), wagering.CodeReferenceRequired},
		"unknown wallet": {func() wagering.RequestInput {
			in := apptest.Input(w, "BET", "b-2", "1.00", "")
			in.WalletID = uuid.NewString()
			return in
		}(), wagering.CodeWalletNotFound},
		"wallet of another player": {func() wagering.RequestInput {
			in := apptest.Input(w, "BET", "b-3", "1.00", "")
			in.PlayerID = uuid.NewString()
			return in
		}(), wagering.CodeWalletPlayerMismatch},
		"currency mismatch": {func() wagering.RequestInput {
			in := apptest.Input(usd, "BET", "b-4", "1.00", "")
			in.Currency = "BRL"
			return in
		}(), wagering.CodeWalletCurrencyMismatch},
	}
	for name, tc := range cases {
		_, err := h.Wagering.Process(ctx, wageringapp.ProcessCommand{Actor: apptest.ProviderA, Input: tc.input})
		verr, ok := errors.AsType[*wagering.ValidationError](err)
		if !ok || verr.Code != tc.code {
			t.Errorf("%s: error = %v, want %s", name, err, tc.code)
		}
	}
	if n := h.Count(t, "SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'"); n != 0 {
		t.Errorf("external transactions persisted = %d", n)
	}

	fixed := apptest.Input(w, "BET", "b-2", "1.00", "")
	if res := h.Process(t, fixed); res.IdempotentReplay || res.Transaction.Status() != wagering.StatusProcessed {
		t.Errorf("correctable error must not consume the key: %+v", res)
	}
}

func TestProviderIsolation(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")
	bet := h.Process(t, apptest.Input(w, "BET", "bet-a", "10.00", ""))

	_, err := h.Wagering.Process(ctx, wageringapp.ProcessCommand{Actor: apptest.ProviderB, Input: apptest.Input(w, "BET", "bet-a", "10.00", "")})
	if !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provider-b acting for provider-a (replay attempt): %v", err)
	}
	_, err = h.Wagering.Process(ctx, wageringapp.ProcessCommand{Actor: apptest.ProviderB, Input: apptest.Input(w, "BET", "new-bet", "10.00", "")})
	if !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provider-b creating provider-a transaction: %v", err)
	}
	if n := h.Count(t, "SELECT count(*) FROM wager_transactions WHERE kind = 'BET'"); n != 1 {
		t.Errorf("unauthorized request had financial effects: %d bets", n)
	}

	if _, err := h.Wagering.GetTransaction(ctx, apptest.ProviderB, bet.Transaction.ID()); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("provider-b reading provider-a transaction by id: %v", err)
	}
	if _, err := h.Wagering.GetByExternalID(ctx, apptest.ProviderB, "provider-a", "bet-a"); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provider-b reading provider-a namespace: %v", err)
	}
	if _, err := h.Wagering.GetByExternalID(ctx, apptest.ProviderB, "provider-b", "bet-a"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("provider-b own namespace must not see provider-a data: %v", err)
	}

	own, err := h.Wagering.GetTransaction(ctx, apptest.ProviderA, bet.Transaction.ID())
	if err != nil || own.ID() != bet.Transaction.ID() {
		t.Errorf("owner read = %v, %v", own, err)
	}
	if _, err := h.Wagering.GetByExternalID(ctx, apptest.ProviderA, "provider-a", "bet-a"); err != nil {
		t.Errorf("owner read by external id: %v", err)
	}
	if _, err := h.Wagering.GetTransaction(ctx, apptest.Internal, bet.Transaction.ID()); err != nil {
		t.Errorf("internal service read: %v", err)
	}

	var openingID uuid.UUID
	if err := h.DB.OwnerPool.QueryRow(ctx, "SELECT id FROM wager_transactions WHERE kind = 'OPENING' AND wallet_id = $1", w.ID).Scan(&openingID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wagering.GetTransaction(ctx, apptest.ProviderA, openingID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("providers must not read internal OPENING transactions: %v", err)
	}
}

func TestReplayAfterRestartUsesPersistedState(t *testing.T) {
	t.Parallel()
	db := pg.NewDatabase(t)
	first := apptest.Attach(t, db)
	w := first.OpenWallet(t, "50.00")
	original := first.Process(t, apptest.Input(w, "BET", "bet-restart", "20.00", ""))
	first.Pool.Close()

	restarted := apptest.Attach(t, db)
	replay := restarted.Process(t, apptest.Input(w, "BET", "bet-restart", "20.00", ""))
	if !replay.IdempotentReplay || replay.Transaction.ID() != original.Transaction.ID() || resultBalance(t, replay.Transaction) != "30.00" {
		t.Errorf("replay after restart = %+v", replay)
	}
	restarted.AssertAllWalletsReconcile(t)
}
