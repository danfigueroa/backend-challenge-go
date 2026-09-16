package wagering_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

var (
	t0          = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	playerID    = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	walletID    = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	testPolicy  = wagering.PendingPolicy{TTL: 30 * time.Minute, BaseDelay: time.Second, MaxDelay: time.Minute}
	sampleInput = wagering.RequestInput{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PlayerID:              playerID.String(),
		WalletID:              walletID.String(),
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  "BET",
		Amount:                "25.00",
		Currency:              "BRL",
	}
)

func input(mutators ...func(*wagering.RequestInput)) wagering.RequestInput {
	in := sampleInput
	for _, m := range mutators {
		m(&in)
	}
	return in
}

func op(kind, externalID, amount string, mutators ...func(*wagering.RequestInput)) wagering.RequestInput {
	return input(append([]func(*wagering.RequestInput){func(in *wagering.RequestInput) {
		in.Kind = kind
		in.ExternalTransactionID = externalID
		in.IdempotencyKey = in.ProviderID + ":" + externalID
		in.Amount = amount
	}}, mutators...)...)
}

func withReference(externalID string) func(*wagering.RequestInput) {
	return func(in *wagering.RequestInput) { in.ReferenceExternalTransactionID = externalID }
}

func newRequest(t *testing.T, in wagering.RequestInput) wagering.Request {
	t.Helper()
	req, err := wagering.NewRequest(in)
	if err != nil {
		t.Fatalf("NewRequest(%+v): %v", in, err)
	}
	return req
}

func newTransaction(t *testing.T, in wagering.RequestInput) *wagering.Transaction {
	t.Helper()
	tx, err := wagering.NewExternal(uuid.New(), newRequest(t, in), t0)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func newWallet(t *testing.T, balance string) *wallet.Wallet {
	t.Helper()
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: walletID, PlayerID: playerID, Balance: brl(t, balance), Version: 1, CreatedAt: t0, UpdatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func process(t *testing.T, tx *wagering.Transaction, w *wallet.Wallet, ref *wagering.Reference, now time.Time) wagering.Decision {
	t.Helper()
	d, err := wagering.Process(wagering.ProcessParams{
		Transaction: tx, Wallet: w, Reference: ref, EntryID: uuid.New(), Now: now, Policy: testPolicy,
	})
	if err != nil {
		t.Fatalf("Process(%s %s): %v", tx.Kind(), tx.ExternalTransactionID(), err)
	}
	return d
}

func processed(t *testing.T, w *wallet.Wallet, in wagering.RequestInput, ref *wagering.Reference) *wagering.Transaction {
	t.Helper()
	tx := newTransaction(t, in)
	if d := process(t, tx, w, ref, t0); d.Outcome != wagering.OutcomeProcessed {
		t.Fatalf("setup %s %s: outcome %s (%s)", tx.Kind(), tx.ExternalTransactionID(), d.Outcome, d.FailureCode)
	}
	return tx
}
