package event_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/event"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

var (
	t0       = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	txID     = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	eventID  = uuid.MustParse("0192f2a0-0000-7000-8000-000000000001")
	policy   = wagering.PendingPolicy{TTL: 30 * time.Minute, BaseDelay: time.Second, MaxDelay: time.Minute}
)

func meta() event.Metadata {
	return event.Metadata{
		EventID:       eventID,
		CorrelationID: "corr-1",
		CausationID:   "msg-123",
		OccurredAt:    t0.In(time.FixedZone("BRT", -3*3600)),
	}
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openedWallet(t *testing.T) (wallet.Opened, *wagering.Transaction) {
	t.Helper()
	opened, err := wallet.Open(wallet.OpenParams{
		ID: walletID, PlayerID: playerID, InitialBalance: brl(t, "1000.00"),
		OpeningTransactionID: txID, OpeningEntryID: uuid.New(), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	opening, err := wagering.NewOpening(wagering.OpeningParams{ID: txID, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "1000.00"), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if err := opening.MarkProcessed(wagering.Result{Balance: opened.Wallet.Balance(), WalletVersion: 1}, uuid.Nil, t0); err != nil {
		t.Fatal(err)
	}
	return opened, opening
}

func externalTx(t *testing.T, kind, externalID, amount, reference string) *wagering.Transaction {
	t.Helper()
	req, err := wagering.NewRequest(wagering.RequestInput{
		ProviderID: "provider-a", ExternalTransactionID: externalID, IdempotencyKey: "provider-a:" + externalID,
		PlayerID: playerID.String(), WalletID: walletID.String(), RoundID: "round-987", GameID: "fortune-chimp",
		Kind: kind, Amount: amount, Currency: "BRL", ReferenceExternalTransactionID: reference,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := wagering.NewExternal(txID, req, t0)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func assertJSON(t *testing.T, e event.Event, want string) {
	t.Helper()
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var gotV, wantV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantV); err != nil {
		t.Fatalf("invalid golden: %v", err)
	}
	gotN, _ := json.Marshal(gotV)
	wantN, _ := json.Marshal(wantV)
	if string(gotN) != string(wantN) {
		t.Errorf("event JSON\n got: %s\nwant: %s", gotN, wantN)
	}
}

func TestOpeningEvents(t *testing.T) {
	t.Parallel()

	opened, opening := openedWallet(t)

	processed, err := event.NewWagerTransactionProcessed(meta(), opening)
	if err != nil {
		t.Fatal(err)
	}
	if processed.Type() != event.TypeWagerTransactionProcessed || processed.Version() != 1 ||
		processed.AggregateType() != event.AggregateWagerTransaction || processed.AggregateID() != txID ||
		processed.PartitionKey() != walletID.String() || processed.ID() != eventID ||
		processed.CorrelationID() != "corr-1" || processed.CausationID() != "msg-123" ||
		processed.OccurredAt().Location() != time.UTC || !processed.OccurredAt().Equal(t0) {
		t.Errorf("unexpected envelope: %+v", processed)
	}
	assertJSON(t, processed, `{
		"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WagerTransactionProcessed",
		"aggregateType":"WagerTransaction","aggregateId":"0192f298-345e-7e38-af88-e43f851a819d",
		"correlationId":"corr-1","causationId":"msg-123","occurredAt":"2026-09-08T12:00:00.000Z","version":1,
		"data":{
			"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","origin":"INTERNAL","kind":"OPENING",
			"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
			"money":{"amount":"1000.00","currency":"BRL"},
			"balance":{"amount":"1000.00","currency":"BRL"},"walletVersion":1,"processedAt":"2026-09-08T12:00:00.000Z"
		}
	}`)

	changed, err := event.NewWalletBalanceChanged(meta(), *opened.OpeningEntry)
	if err != nil {
		t.Fatal(err)
	}
	if changed.AggregateType() != event.AggregateWallet || changed.AggregateID() != walletID || changed.PartitionKey() != walletID.String() {
		t.Errorf("unexpected wallet envelope: %+v", changed)
	}
	assertJSON(t, changed, `{
		"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WalletBalanceChanged",
		"aggregateType":"Wallet","aggregateId":"0192f291-27dd-7d3f-8071-5f8685deef37",
		"correlationId":"corr-1","causationId":"msg-123","occurredAt":"2026-09-08T12:00:00.000Z","version":1,
		"data":{
			"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","transactionId":"0192f298-345e-7e38-af88-e43f851a819d",
			"direction":"CREDIT","money":{"amount":"1000.00","currency":"BRL"},
			"balanceBefore":{"amount":"0.00","currency":"BRL"},"balanceAfter":{"amount":"1000.00","currency":"BRL"},
			"walletVersion":1
		}
	}`)
}

func TestExternalProcessedEvent(t *testing.T) {
	t.Parallel()

	w, err := wallet.Rehydrate(wallet.RehydrateParams{ID: walletID, PlayerID: playerID, Balance: brl(t, "1000.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	tx := externalTx(t, "BET", "transaction-123", "25.00", "")
	d, err := wagering.Process(wagering.ProcessParams{Transaction: tx, Wallet: w, EntryID: uuid.New(), Now: t0, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}

	m := meta()
	m.CausationID = ""
	processed, err := event.NewWagerTransactionProcessed(m, tx)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, processed, `{
		"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WagerTransactionProcessed",
		"aggregateType":"WagerTransaction","aggregateId":"0192f298-345e-7e38-af88-e43f851a819d",
		"correlationId":"corr-1","occurredAt":"2026-09-08T12:00:00.000Z","version":1,
		"data":{
			"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","origin":"EXTERNAL","kind":"BET",
			"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
			"money":{"amount":"25.00","currency":"BRL"},"providerId":"provider-a","externalTransactionId":"transaction-123",
			"roundId":"round-987","gameId":"fortune-chimp",
			"balance":{"amount":"975.00","currency":"BRL"},"walletVersion":2,"processedAt":"2026-09-08T12:00:00.000Z"
		}
	}`)

	changed, err := event.NewWalletBalanceChanged(m, *d.Entry)
	if err != nil {
		t.Fatal(err)
	}
	data := changed.Data()
	if data.Direction != wallet.DirectionDebit || data.BalanceBefore.Amount() != "1000.00" ||
		data.BalanceAfter.Amount() != "975.00" || data.WalletVersion != 2 || data.Money.Amount() != "25.00" {
		t.Errorf("balance changed data = %+v", data)
	}
}

func TestLossProducesOnlyProcessedEvent(t *testing.T) {
	t.Parallel()

	w, err := wallet.Rehydrate(wallet.RehydrateParams{ID: walletID, PlayerID: playerID, Balance: brl(t, "10.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	tx := externalTx(t, "LOSS", "loss-1", "0.00", "")
	d, err := wagering.Process(wagering.ProcessParams{Transaction: tx, Wallet: w, EntryID: uuid.New(), Now: t0, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if d.Entry != nil {
		t.Fatal("LOSS produced a ledger entry, which would emit WalletBalanceChanged")
	}
	if _, err := event.NewWagerTransactionProcessed(meta(), tx); err != nil {
		t.Errorf("LOSS must emit WagerTransactionProcessed: %v", err)
	}
}

func TestRejectedEvent(t *testing.T) {
	t.Parallel()

	tx := externalTx(t, "REFUND", "refund-1", "25.00", "bet-404")
	if err := tx.Reject(wagering.Rejection{Code: wagering.CodeReferenceNotFound}, t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rejected, err := event.NewWagerTransactionRejected(meta(), tx)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, rejected, `{
		"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WagerTransactionRejected",
		"aggregateType":"WagerTransaction","aggregateId":"0192f298-345e-7e38-af88-e43f851a819d",
		"correlationId":"corr-1","causationId":"msg-123","occurredAt":"2026-09-08T12:00:00.000Z","version":1,
		"data":{
			"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","origin":"EXTERNAL","kind":"REFUND",
			"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
			"money":{"amount":"25.00","currency":"BRL"},"providerId":"provider-a","externalTransactionId":"refund-1",
			"roundId":"round-987","gameId":"fortune-chimp","referenceExternalTransactionId":"bet-404",
			"failureCode":"REFERENCE_NOT_FOUND","failureCategory":"DEFINITIVE","rejectedAt":"2026-09-08T12:30:00.000Z"
		}
	}`)
}

func TestPendingReferenceEvent(t *testing.T) {
	t.Parallel()

	tx := externalTx(t, "ROLLBACK", "rb-1", "25.00", "bet-1")
	if err := tx.AwaitReference(t0.Add(time.Second), t0.Add(30*time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	pending, err := event.NewWagerTransactionPendingReference(meta(), tx, wagering.CodeReferenceNotFound)
	if err != nil {
		t.Fatal(err)
	}
	data := pending.Data()
	if pending.Type() != event.TypeWagerTransactionPendingReference || data.WaitingReason != wagering.CodeReferenceNotFound ||
		data.Attempts != 1 || !data.NextAttemptAt.Time().Equal(t0.Add(time.Second)) || !data.ExpiresAt.Time().Equal(t0.Add(30*time.Minute)) ||
		data.ReferenceExternalTransactionID != "bet-1" {
		t.Errorf("pending data = %+v", data)
	}

	if _, err := event.NewWagerTransactionPendingReference(meta(), tx, wagering.CodeInsufficientFunds); !errors.Is(err, event.ErrInvalidEvent) {
		t.Errorf("invalid reason error = %v", err)
	}
}

func TestConstructorsRequireMatchingState(t *testing.T) {
	t.Parallel()

	pending := externalTx(t, "BET", "bet-1", "25.00", "")

	if _, err := event.NewWagerTransactionProcessed(meta(), pending); !errors.Is(err, event.ErrInvalidEvent) {
		t.Errorf("processed from pending: %v", err)
	}
	if _, err := event.NewWagerTransactionRejected(meta(), pending); !errors.Is(err, event.ErrInvalidEvent) {
		t.Errorf("rejected from pending: %v", err)
	}
	if _, err := event.NewWagerTransactionPendingReference(meta(), pending, wagering.CodeReferenceNotFound); !errors.Is(err, event.ErrInvalidEvent) {
		t.Errorf("pending reference from pending: %v", err)
	}
	if _, err := event.NewWagerTransactionProcessed(meta(), nil); !errors.Is(err, event.ErrInvalidEvent) {
		t.Errorf("nil transaction: %v", err)
	}
	if _, err := event.NewWalletBalanceChanged(meta(), wallet.LedgerEntry{}); !errors.Is(err, event.ErrInvalidEvent) {
		t.Errorf("empty entry: %v", err)
	}
}

func TestMetadataValidation(t *testing.T) {
	t.Parallel()

	_, opening := openedWallet(t)
	for name, mutate := range map[string]func(*event.Metadata){
		"event id":    func(m *event.Metadata) { m.EventID = uuid.Nil },
		"correlation": func(m *event.Metadata) { m.CorrelationID = "" },
		"occurred at": func(m *event.Metadata) { m.OccurredAt = time.Time{} },
	} {
		m := meta()
		mutate(&m)
		if _, err := event.NewWagerTransactionProcessed(m, opening); !errors.Is(err, event.ErrInvalidEvent) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	t.Parallel()

	ts := event.Timestamp(time.Date(2026, 9, 8, 9, 0, 0, 123_000_000, time.FixedZone("BRT", -3*3600)))
	data, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"2026-09-08T12:00:00.123Z"` {
		t.Errorf("marshal = %s", data)
	}
	var back event.Timestamp
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Time().Equal(ts.Time()) || back.Time().Location() != time.UTC {
		t.Errorf("round trip = %v", back.Time())
	}
	for _, bad := range []string{`"yesterday"`, `12`, `"2026-09-08 12:00:00"`} {
		if err := json.Unmarshal([]byte(bad), &back); !errors.Is(err, event.ErrInvalidEvent) {
			t.Errorf("Unmarshal(%s) error = %v", bad, err)
		}
	}
}
