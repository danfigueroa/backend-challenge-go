//go:build integration

package wageringapp_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/apptest"
)

func walletappOpen(playerID, currency string) walletapp.OpenWalletCommand {
	return walletapp.OpenWalletCommand{Actor: apptest.Internal, PlayerID: playerID, Amount: "10.00", Currency: currency}
}

func TestSameBetFiftyTimesInParallelDebitsOnce(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "1000.00")
	in := apptest.Input(w, "BET", "bet-50x", "25.00", "")

	const requests = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     = map[string]int{}
		fresh   int
		replays int
		start   = make(chan struct{})
	)
	for range requests {
		wg.Go(func() {
			<-start
			res, err := h.Wagering.Process(context.Background(), wageringapp.ProcessCommand{
				Actor: apptest.ProviderA, Meta: app.Metadata{Channel: app.ChannelHTTP}, Input: in,
			})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[res.Transaction.ID().String()]++
			if res.IdempotentReplay {
				replays++
			} else {
				fresh++
			}
			if r, _ := res.Transaction.Result(); r.Balance.Amount() != "975.00" {
				t.Errorf("response balance = %s, want 975.00", r.Balance.Amount())
			}
		})
	}
	close(start)
	wg.Wait()

	if len(ids) != 1 || fresh != 1 || replays != requests-1 {
		t.Errorf("distinct transactions = %d, fresh = %d, replays = %d", len(ids), fresh, replays)
	}
	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Errorf("debits = %d, want exactly 1", n)
	}
	if n := h.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 97500 {
		t.Errorf("balance = %d, want 97500", n)
	}
	h.AssertAllWalletsReconcile(t)
}

func TestTwoConcurrentBetsOfEightyOnHundred(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "100.00")

	inputs := []wagering.RequestInput{
		apptest.Input(w, "BET", "bet-80-a", "80.00", ""),
		apptest.Input(w, "BET", "bet-80-b", "80.00", ""),
	}
	results := make([]wageringapp.ProcessResult, len(inputs))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, in := range inputs {
		wg.Go(func() {
			<-start
			res, err := h.Wagering.Process(context.Background(), wageringapp.ProcessCommand{Actor: apptest.ProviderA, Input: in})
			if err != nil {
				t.Error(err)
				return
			}
			results[i] = res
		})
	}
	close(start)
	wg.Wait()

	statuses := map[wagering.Status]int{}
	for _, r := range results {
		statuses[r.Transaction.Status()]++
		if r.Transaction.Status() == wagering.StatusRejected && r.Transaction.FailureCode() != wagering.CodeInsufficientFunds {
			t.Errorf("rejection code = %s", r.Transaction.FailureCode())
		}
	}
	if statuses[wagering.StatusProcessed] != 1 || statuses[wagering.StatusRejected] != 1 {
		t.Fatalf("statuses = %v", statuses)
	}

	for _, in := range inputs {
		replay := h.Process(t, in)
		if !replay.IdempotentReplay {
			t.Errorf("resend of %s was not a replay", in.ExternalTransactionID)
		}
	}
	if n := h.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 2000 {
		t.Errorf("balance = %d, want 2000", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Errorf("debits = %d, want 1", n)
	}
	h.AssertAllWalletsReconcile(t)
}

func TestDistinctWalletsAreProcessedInParallel(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)

	const wallets, betsPerWallet = 10, 10
	views := make([]walletapp.WalletView, wallets)
	for i := range views {
		views[i] = h.OpenWallet(t, "100.00")
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, w := range views {
		for b := range betsPerWallet {
			wg.Go(func() {
				<-start
				_, err := h.Wagering.Process(context.Background(), wageringapp.ProcessCommand{
					Actor: apptest.ProviderA, Input: apptest.Input(w, "BET", fmt.Sprintf("w%d-b%d", i, b), "1.00", ""),
				})
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
	close(start)
	wg.Wait()

	if n := h.Count(t, "SELECT count(*) FROM wallets WHERE balance_minor = 9000 AND version = 11"); n != wallets {
		t.Errorf("wallets with expected final state = %d, want %d", n, wallets)
	}
	h.AssertAllWalletsReconcile(t)
}

func TestReversalArrivingBeforeReferenceIsResolvedLater(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")

	refund := h.Process(t, apptest.Input(w, "REFUND", "refund-early", "40.00", "bet-late"))
	if refund.Transaction.Status() != wagering.StatusPendingReference {
		t.Fatalf("refund status = %s", refund.Transaction.Status())
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionPendingReference'", refund.Transaction.ID()); n != 1 {
		t.Errorf("pending reference events = %d", n)
	}
	replay := h.Process(t, apptest.Input(w, "REFUND", "refund-early", "40.00", "bet-late"))
	if !replay.IdempotentReplay || replay.Transaction.Status() != wagering.StatusPendingReference {
		t.Errorf("replay of pending = %+v", replay)
	}

	h.Process(t, apptest.Input(w, "BET", "bet-late", "40.00", ""))

	stats, err := h.Wagering.ResolveDuePending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 1 || stats.Processed != 1 {
		t.Fatalf("stats = %+v (the bet should have woken the refund immediately)", stats)
	}

	resolved, err := h.Wagering.GetByExternalID(ctx, apptest.ProviderA, "provider-a", "refund-early")
	if err != nil || resolved.Status() != wagering.StatusProcessed || resolvedBalance(resolved) != "100.00" {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1", refund.Transaction.ID()); n != 2 {
		t.Errorf("refund events = %d, want pending + processed", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE partition_key = $1 AND event_type = 'WalletBalanceChanged'", w.ID.String()); n != 3 {
		t.Errorf("balance events = %d, want opening + bet + refund", n)
	}
	h.AssertAllWalletsReconcile(t)
}

func TestReversalWaitingOnRejectedReferenceIsWokenAndRejected(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "10.00")

	refund := h.Process(t, apptest.Input(w, "REFUND", "refund-early", "40.00", "bet-unfunded"))
	if refund.Transaction.Status() != wagering.StatusPendingReference {
		t.Fatalf("refund status = %s", refund.Transaction.Status())
	}
	bet := h.Process(t, apptest.Input(w, "BET", "bet-unfunded", "40.00", ""))
	if bet.Transaction.FailureCode() != wagering.CodeInsufficientFunds {
		t.Fatalf("bet = %s %s", bet.Transaction.Status(), bet.Transaction.FailureCode())
	}

	stats, err := h.Wagering.ResolveDuePending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Claimed != 1 || stats.Rejected != 1 {
		t.Fatalf("stats = %+v (the rejected bet should have woken the refund immediately)", stats)
	}
	resolved, err := h.Wagering.GetByExternalID(ctx, apptest.ProviderA, "provider-a", "refund-early")
	if err != nil || resolved.Status() != wagering.StatusRejected || resolved.FailureCode() != wagering.CodeReferenceNotProcessed {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
	h.AssertAllWalletsReconcile(t)
}

func resolvedBalance(tx *wagering.Transaction) string {
	r, _ := tx.Result()
	return r.Balance.Amount()
}

func TestPendingReferenceExpiresWithRejection(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")

	rb := h.Process(t, apptest.Input(w, "ROLLBACK", "rollback-orphan", "10.00", "never-arrives"))
	if rb.Transaction.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s", rb.Transaction.Status())
	}

	h.Clock.Advance(5 * time.Second)
	stats, err := h.Wagering.ResolveDuePending(ctx, 10)
	if err != nil || stats.StillPending != 1 {
		t.Fatalf("before expiry stats = %+v, %v", stats, err)
	}

	h.Clock.Advance(apptest.Pending.TTL)
	stats, err = h.Wagering.ResolveDuePending(ctx, 10)
	if err != nil || stats.Rejected != 1 {
		t.Fatalf("after expiry stats = %+v, %v", stats, err)
	}

	final, err := h.Wagering.GetTransaction(ctx, apptest.ProviderA, rb.Transaction.ID())
	if err != nil || final.Status() != wagering.StatusRejected || final.FailureCode() != wagering.CodeReferenceNotFound {
		t.Fatalf("final = %s %s, %v", final.Status(), final.FailureCode(), err)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionRejected'", rb.Transaction.ID()); n != 1 {
		t.Errorf("rejection events = %d", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'WagerTransactionPendingReference'", rb.Transaction.ID()); n != 1 {
		t.Errorf("pending events = %d, retries must not republish", n)
	}

	if stats, err := h.Wagering.ResolveDuePending(ctx, 10); err != nil || stats.Claimed != 0 {
		t.Errorf("terminal transactions must not be claimed again: %+v, %v", stats, err)
	}
}

func TestPendingSurvivesRestartAndIsResumedByAnotherInstance(t *testing.T) {
	t.Parallel()
	db := pg.NewDatabase(t)
	crashed := apptest.Attach(t, db)
	w := crashed.OpenWallet(t, "100.00")
	crashed.Process(t, apptest.Input(w, "REFUND", "refund-restart", "15.00", "bet-restart"))
	crashed.Pool.Close()

	survivor := apptest.Attach(t, db)
	survivor.Process(t, apptest.Input(w, "BET", "bet-restart", "15.00", ""))
	stats, err := survivor.Wagering.ResolveDuePending(context.Background(), 10)
	if err != nil || stats.Processed != 1 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if n := survivor.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 10000 {
		t.Errorf("balance = %d, want 10000", n)
	}
	survivor.AssertAllWalletsReconcile(t)
}

func TestConcurrentWorkersResolveEachPendingOnce(t *testing.T) {
	t.Parallel()
	db := pg.NewDatabase(t)
	instances := []*apptest.Harness{apptest.Attach(t, db), apptest.Attach(t, db), apptest.Attach(t, db)}
	h := instances[0]

	const pendings = 30
	w := h.OpenWallet(t, "1000.00")
	for i := range pendings {
		h.Process(t, apptest.Input(w, "REFUND", fmt.Sprintf("refund-%d", i), "1.00", fmt.Sprintf("bet-%d", i)))
	}
	for i := range pendings {
		h.Process(t, apptest.Input(w, "BET", fmt.Sprintf("bet-%d", i), "1.00", ""))
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		processed int
	)
	for _, instance := range instances {
		wg.Go(func() {
			for {
				stats, err := instance.Wagering.ResolveDuePending(context.Background(), 4)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				processed += stats.Processed
				mu.Unlock()
				if stats.Claimed == 0 {
					return
				}
			}
		})
	}
	wg.Wait()

	if processed != pendings {
		t.Errorf("processed = %d, want %d", processed, pendings)
	}
	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT'", w.ID); n != pendings+1 {
		t.Errorf("credits = %d, want %d", n, pendings+1)
	}
	if n := h.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 100000 {
		t.Errorf("balance = %d, want 100000", n)
	}
	h.AssertAllWalletsReconcile(t)
}

func TestReversalCombinations(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "100.00")

	h.Process(t, apptest.Input(w, "BET", "bet-1", "40.00", ""))
	refund := h.Process(t, apptest.Input(w, "REFUND", "refund-1", "40.00", "bet-1"))
	if refund.Transaction.Status() != wagering.StatusProcessed {
		t.Fatalf("refund = %s", refund.Transaction.Status())
	}

	expectRejected := func(in wagering.RequestInput, code wagering.FailureCode) {
		t.Helper()
		res := h.Process(t, in)
		if res.Transaction.Status() != wagering.StatusRejected || res.Transaction.FailureCode() != code {
			t.Errorf("%s: %s %s, want REJECTED %s", in.ExternalTransactionID, res.Transaction.Status(), res.Transaction.FailureCode(), code)
		}
	}
	expectRejected(apptest.Input(w, "REFUND", "refund-2", "40.00", "bet-1"), wagering.CodeReferenceAlreadyReversed)
	expectRejected(apptest.Input(w, "ROLLBACK", "rollback-bet", "40.00", "bet-1"), wagering.CodeReferenceAlreadyReversed)
	expectRejected(apptest.Input(w, "REFUND", "refund-partial", "39.99", "bet-1"), wagering.CodeReferenceAmountMismatch)

	rollbackRefund := h.Process(t, apptest.Input(w, "ROLLBACK", "rollback-refund", "40.00", "refund-1"))
	if rollbackRefund.Transaction.Status() != wagering.StatusProcessed || resolvedBalance(rollbackRefund.Transaction) != "60.00" {
		t.Errorf("rollback of refund = %s %s", rollbackRefund.Transaction.Status(), resolvedBalance(rollbackRefund.Transaction))
	}
	expectRejected(apptest.Input(w, "REFUND", "refund-3", "40.00", "bet-1"), wagering.CodeReferenceAlreadyReversed)

	h.Process(t, apptest.Input(w, "WIN", "win-1", "50.00", "bet-1"))
	h.Process(t, apptest.Input(w, "BET", "bet-2", "100.00", ""))
	expectRejected(apptest.Input(w, "ROLLBACK", "rollback-win", "50.00", "win-1"), wagering.CodeReversalInsufficientFunds)

	if n := h.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 1000 {
		t.Errorf("balance = %d, want 1000", n)
	}
	h.AssertAllWalletsReconcile(t)
}

func delivery(messageID, body string) *wageringapp.Delivery {
	return &wageringapp.Delivery{
		ConsumerName: "wager-transactions", MessageID: messageID, PayloadHash: sha256.Sum256([]byte(body)), ReceivedAt: time.Now().UTC(),
	}
}

func TestSQSDeliveriesAreDeduplicatedByInboxAndIdempotency(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")
	in := apptest.Input(w, "BET", "bet-sqs", "10.00", "")

	sqs := func(d *wageringapp.Delivery, input wagering.RequestInput) (wageringapp.ProcessResult, error) {
		return h.Wagering.Process(ctx, wageringapp.ProcessCommand{
			Actor: apptest.Broker, Meta: app.Metadata{Channel: app.ChannelSQS, CausationID: d.MessageID}, Input: input, Delivery: d,
		})
	}

	first, err := sqs(delivery("msg-1", "body-1"), in)
	if err != nil || first.IdempotentReplay || first.Transaction.Status() != wagering.StatusProcessed {
		t.Fatalf("first delivery = %+v, %v", first, err)
	}
	redelivery, err := sqs(delivery("msg-1", "body-1"), in)
	if err != nil || !redelivery.DuplicateDelivery || !redelivery.IdempotentReplay || redelivery.Transaction.ID() != first.Transaction.ID() {
		t.Errorf("redelivery = %+v, %v", redelivery, err)
	}
	if _, err := sqs(delivery("msg-1", "tampered-body"), in); !errors.Is(err, app.ErrInboxMismatch) {
		t.Errorf("same message id with different payload: %v", err)
	}

	resent, err := sqs(delivery("msg-2", "body-2"), in)
	if err != nil || resent.DuplicateDelivery || !resent.IdempotentReplay {
		t.Errorf("new message id for the same operation = %+v, %v", resent, err)
	}

	httpReplay := h.Process(t, in)
	if !httpReplay.IdempotentReplay || httpReplay.Transaction.ID() != first.Transaction.ID() {
		t.Errorf("HTTP after SQS = %+v", httpReplay)
	}

	if n := h.Count(t, "SELECT count(*) FROM inbox_messages WHERE processed_at IS NOT NULL AND transaction_id = $1", first.Transaction.ID()); n != 2 {
		t.Errorf("processed inbox messages = %d, want 2 (msg-1 and msg-2)", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Errorf("debits = %d", n)
	}

	httpFirst := h.Process(t, apptest.Input(w, "BET", "bet-http", "5.00", ""))
	sqsAfterHTTP, err := sqs(delivery("msg-3", "body-3"), apptest.Input(w, "BET", "bet-http", "5.00", ""))
	if err != nil || !sqsAfterHTTP.IdempotentReplay || sqsAfterHTTP.Transaction.ID() != httpFirst.Transaction.ID() {
		t.Errorf("SQS after HTTP = %+v, %v", sqsAfterHTTP, err)
	}

	_, err = sqs(delivery("msg-invalid", "body-invalid"), apptest.Input(w, "OPENING", "opening", "1.00", ""))
	if _, ok := errors.AsType[*wagering.ValidationError](err); !ok {
		t.Errorf("invalid SQS payload: %v", err)
	}
	if n := h.Count(t, "SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-invalid'"); n != 0 {
		t.Errorf("invalid message must not be recorded as handled (it goes to the DLQ)")
	}
	h.AssertAllWalletsReconcile(t)
}

func TestConcurrentHTTPAndSQSForTheSameOperation(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	w := h.OpenWallet(t, "100.00")
	in := apptest.Input(w, "BET", "bet-race", "30.00", "")

	const each = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range each {
		wg.Go(func() {
			<-start
			if _, err := h.Wagering.Process(context.Background(), wageringapp.ProcessCommand{Actor: apptest.ProviderA, Input: in}); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			<-start
			d := delivery(fmt.Sprintf("msg-%d", i), "same-body")
			if _, err := h.Wagering.Process(context.Background(), wageringapp.ProcessCommand{Actor: apptest.Broker, Input: in, Delivery: d}); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()

	if n := h.Count(t, "SELECT count(*) FROM wager_transactions WHERE external_transaction_id = 'bet-race'"); n != 1 {
		t.Errorf("transactions = %d", n)
	}
	if n := h.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 7000 {
		t.Errorf("balance = %d, want 7000", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM inbox_messages WHERE processed_at IS NOT NULL"); n != each {
		t.Errorf("inbox messages = %d, want %d", n, each)
	}
	h.AssertAllWalletsReconcile(t)
}
