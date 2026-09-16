//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/event"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

func TestWalletRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	opened := f.openWallet(t, "1000.00")

	t.Run("round trip", func(t *testing.T) {
		got, err := f.wallets.Get(ctx, opened.ID())
		if err != nil {
			t.Fatal(err)
		}
		if got.ID() != opened.ID() || got.PlayerID() != opened.PlayerID() || !got.Balance().Equal(opened.Balance()) ||
			got.Version() != 1 || got.LoadedVersion() != 1 || !got.CreatedAt().Equal(opened.CreatedAt()) {
			t.Errorf("loaded wallet = %+v, want %+v", got, opened)
		}
	})

	t.Run("zero balance wallet has no ledger", func(t *testing.T) {
		empty := f.openWallet(t, "0.00")
		summary, err := f.ledger.Summarize(ctx, empty.ID())
		if err != nil || summary.Entries != 0 {
			t.Errorf("summary = %+v, %v", summary, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		if _, err := f.wallets.Get(ctx, uuid.New()); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("lock requires transaction", func(t *testing.T) {
		if _, err := f.wallets.GetForUpdate(ctx, opened.ID()); !errors.Is(err, app.ErrTxRequired) {
			t.Errorf("error = %v, want ErrTxRequired", err)
		}
	})

	t.Run("duplicate player and currency is a wallet conflict", func(t *testing.T) {
		dup, err := wallet.Open(wallet.OpenParams{ID: uuid.New(), PlayerID: opened.PlayerID(), InitialBalance: brl(t, "0.00"), Now: now()})
		if err != nil {
			t.Fatal(err)
		}
		err = f.txm.WithinTx(ctx, func(ctx context.Context) error { return f.wallets.Insert(ctx, dup.Wallet) })
		if kind, ok := app.ConflictKindOf(err); !ok || kind != app.ConflictWalletExists {
			t.Errorf("error = %v, want wallet conflict", err)
		}
	})

	t.Run("stale version is a concurrent update", func(t *testing.T) {
		stale, err := f.wallets.Get(ctx, opened.ID())
		if err != nil {
			t.Fatal(err)
		}
		f.mustProcess(t, opened, opInput{kind: "BET", externalID: "bet-stale", amount: "10.00"})

		if _, err := stale.Debit(wallet.Movement{EntryID: uuid.New(), TransactionID: uuid.New(), Amount: brl(t, "1.00"), Now: now()}); err != nil {
			t.Fatal(err)
		}
		err = f.txm.WithinTx(ctx, func(ctx context.Context) error { return f.wallets.Update(ctx, stale) })
		if !errors.Is(err, app.ErrConcurrentUpdate) {
			t.Errorf("error = %v, want ErrConcurrentUpdate", err)
		}
	})
}

func TestTransactionRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	w := f.openWallet(t, "100.00")

	bet := f.mustProcess(t, w, opInput{kind: "BET", externalID: "bet-1", amount: "25.00"})

	t.Run("round trip processed", func(t *testing.T) {
		got, err := f.txs.Get(ctx, bet.ID())
		if err != nil {
			t.Fatal(err)
		}
		result, ok := got.Result()
		if got.Status() != wagering.StatusProcessed || !ok || result.Balance.Amount() != "75.00" || result.WalletVersion != 2 ||
			got.PayloadHash() != bet.PayloadHash() || got.IdempotencyKey() != "provider-a:bet-1" || !got.CompletedAt().Equal(bet.CompletedAt()) {
			t.Errorf("loaded = %+v", got)
		}
	})

	t.Run("lookup by idempotency key and external id", func(t *testing.T) {
		byKey, err := f.txs.GetByIdempotencyKey(ctx, "provider-a", "provider-a:bet-1")
		if err != nil || byKey.ID() != bet.ID() {
			t.Errorf("by key = %v, %v", byKey, err)
		}
		byExternal, err := f.txs.GetByExternalID(ctx, "provider-a", "bet-1")
		if err != nil || byExternal.ID() != bet.ID() {
			t.Errorf("by external = %v, %v", byExternal, err)
		}
		if _, err := f.txs.GetByExternalID(ctx, "provider-b", "bet-1"); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("other provider must not see the transaction: %v", err)
		}
		if _, err := f.txs.GetByExternalIDForUpdate(ctx, "provider-a", "bet-1"); !errors.Is(err, app.ErrTxRequired) {
			t.Errorf("lock outside tx = %v", err)
		}
	})

	t.Run("duplicate keys are classified", func(t *testing.T) {
		_, _, err := f.process(ctx, t, w.ID(), f.request(t, w, opInput{kind: "BET", externalID: "bet-1", amount: "25.00"}))
		if kind, ok := app.ConflictKindOf(err); !ok || kind != app.ConflictExternalTransaction {
			t.Errorf("same external id error = %v", err)
		}

		req, err := wagering.NewRequest(wagering.RequestInput{
			ProviderID: "provider-a", ExternalTransactionID: "bet-2", IdempotencyKey: "provider-a:bet-1",
			PlayerID: w.PlayerID().String(), WalletID: w.ID().String(), RoundID: "round-1", GameID: "game-1",
			Kind: "BET", Amount: "1.00", Currency: "BRL",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = f.process(ctx, t, w.ID(), req)
		if kind, ok := app.ConflictKindOf(err); !ok || kind != app.ConflictIdempotencyKey {
			t.Errorf("reused idempotency key error = %v", err)
		}
	})

	t.Run("pending reference lifecycle", func(t *testing.T) {
		refund, decision, err := f.process(ctx, t, w.ID(), f.request(t, w, opInput{kind: "REFUND", externalID: "refund-late", amount: "10.00", reference: "bet-late"}))
		if err != nil || decision.Outcome != wagering.OutcomeAwaitingReference {
			t.Fatalf("pending refund = %v, %v", decision, err)
		}
		loaded, err := f.txs.Get(ctx, refund.ID())
		if err != nil || loaded.Status() != wagering.StatusPendingReference || loaded.Attempts() != 1 ||
			!loaded.ExpiresAt().Equal(refund.ExpiresAt()) || !loaded.NextAttemptAt().Equal(refund.NextAttemptAt()) {
			t.Fatalf("loaded pending = %+v, %v", loaded, err)
		}

		f.mustProcess(t, w, opInput{kind: "BET", externalID: "bet-late", amount: "10.00"})
		woken, err := f.txs.WakeWaitingOn(ctx, "provider-a", "bet-late", now())
		if err != nil || woken != 1 {
			t.Fatalf("woken = %d, %v", woken, err)
		}

		var resolved *wagering.Transaction
		err = f.txm.WithinTx(ctx, func(ctx context.Context) error {
			current, err := f.wallets.GetForUpdate(ctx, w.ID())
			if err != nil {
				return err
			}
			pending, err := f.txs.GetForUpdate(ctx, refund.ID())
			if err != nil {
				return err
			}
			ref, err := f.txs.GetByExternalIDForUpdate(ctx, "provider-a", "bet-late")
			if err != nil {
				return err
			}
			at := now()
			d, err := wagering.Process(wagering.ProcessParams{
				Transaction: pending, Wallet: current, Reference: &wagering.Reference{Transaction: ref},
				EntryID: uuid.New(), Now: at, Policy: testPolicy,
			})
			if err != nil {
				return err
			}
			if err := f.txs.Update(ctx, pending); err != nil {
				return err
			}
			if err := f.wallets.Update(ctx, current); err != nil {
				return err
			}
			resolved = pending
			return f.ledger.Insert(ctx, *d.Entry)
		})
		if err != nil {
			t.Fatal(err)
		}
		reloaded, err := f.txs.Get(ctx, resolved.ID())
		if err != nil || reloaded.Status() != wagering.StatusProcessed || reloaded.ReferenceTransactionID() == uuid.Nil {
			t.Errorf("resolved = %+v, %v", reloaded, err)
		}

		reversed, err := f.txs.HasProcessedReversal(ctx, reloaded.ReferenceTransactionID())
		if err != nil || !reversed {
			t.Errorf("HasProcessedReversal = %v, %v", reversed, err)
		}
		notReversed, err := f.txs.HasProcessedReversal(ctx, bet.ID())
		if err != nil || notReversed {
			t.Errorf("bet-1 HasProcessedReversal = %v, %v", notReversed, err)
		}

		err = f.txm.WithinTx(ctx, func(ctx context.Context) error { return f.txs.Update(ctx, reloaded) })
		if err == nil {
			t.Error("updating a terminal transaction must fail")
		}
	})
}

func TestClaimDuePendingIsExclusiveAcrossWorkers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	const total = 40
	for i := range total {
		w := f.openWallet(t, "10.00")
		if _, d, err := f.process(ctx, t, w.ID(), f.request(t, w, opInput{
			kind: "ROLLBACK", externalID: fmt.Sprintf("rb-%d", i), amount: "1.00", reference: fmt.Sprintf("missing-%d", i),
		})); err != nil || d.Outcome != wagering.OutcomeAwaitingReference {
			t.Fatalf("setup %d: %v %v", i, d, err)
		}
	}

	future := now().Add(2 * time.Hour)
	var (
		mu      sync.Mutex
		claimed = map[uuid.UUID]int{}
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for worker := range 4 {
		wg.Go(func() {
			<-start
			for {
				var batch []app.PendingClaim
				err := f.txm.WithinTx(ctx, func(ctx context.Context) error {
					var err error
					batch, err = f.txs.ClaimDuePending(ctx, future, time.Minute, 3)
					return err
				})
				if err != nil {
					t.Errorf("worker %d: %v", worker, err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, c := range batch {
					claimed[c.TransactionID]++
				}
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	if len(claimed) != total {
		t.Errorf("claimed %d distinct transactions, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("transaction %s claimed %d times within the lease", id, n)
		}
	}

	again, err := f.txs.ClaimDuePending(ctx, future, time.Minute, 100)
	if err != nil || len(again) != 0 {
		t.Errorf("leased transactions were claimable again: %d, %v", len(again), err)
	}
	afterLease, err := f.txs.ClaimDuePending(ctx, future.Add(2*time.Minute), time.Minute, 100)
	if err != nil || len(afterLease) != total {
		t.Errorf("after lease expiry claimed %d, want %d (%v)", len(afterLease), total, err)
	}
}

func TestLedgerRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	w := f.openWallet(t, "100.00")

	amounts := []struct{ kind, amount string }{
		{"BET", "10.00"}, {"WIN", "25.50"}, {"BET", "5.25"}, {"LOSS", "0.00"}, {"BET", "0.25"}, {"WIN", "1.00"},
	}
	for i, op := range amounts {
		f.mustProcess(t, w, opInput{kind: op.kind, externalID: fmt.Sprintf("op-%d", i), amount: op.amount})
	}
	current, err := f.wallets.Get(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if current.Balance().Amount() != "111.00" || current.Version() != 6 {
		t.Fatalf("wallet = %s v%d, want 111.00 v6", current.Balance(), current.Version())
	}

	t.Run("pagination by wallet version", func(t *testing.T) {
		var (
			all   []wallet.LedgerEntry
			after int64
		)
		for {
			page, err := f.ledger.List(ctx, w.ID(), after, 4)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, page...)
			if len(page) < 4 {
				break
			}
			after = page[len(page)-1].WalletVersion()
		}
		if len(all) != 6 {
			t.Fatalf("entries = %d, want 6", len(all))
		}
		for i, e := range all {
			if e.WalletVersion() != int64(i+1) {
				t.Errorf("entry %d version = %d", i, e.WalletVersion())
			}
			if i > 0 && !e.BalanceBefore().Equal(all[i-1].BalanceAfter()) {
				t.Errorf("entry %d does not chain from previous", i)
			}
		}
		if all[0].Direction() != wallet.DirectionCredit || all[0].Amount().Amount() != "100.00" {
			t.Errorf("first entry should be the opening credit, got %s %s", all[0].Direction(), all[0].Amount())
		}
	})

	t.Run("summary matches stored balance", func(t *testing.T) {
		summary, err := f.ledger.Summarize(ctx, w.ID())
		if err != nil {
			t.Fatal(err)
		}
		want := app.LedgerSummary{Entries: 6, NetMinor: 11100, LastVersion: 6, LastBalanceMinor: 11100, ChainBreaks: 0}
		if summary != want {
			t.Errorf("summary = %+v, want %+v", summary, want)
		}
	})
}

func TestInboxRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	at := now()
	msg := app.InboxMessage{ConsumerName: "wager-consumer", MessageID: "msg-1", PayloadHash: [32]byte{1, 2, 3}, ReceivedAt: at}

	existing, err := f.inbox.InsertIfAbsent(ctx, msg)
	if err != nil || existing != nil {
		t.Fatalf("first insert = %+v, %v", existing, err)
	}
	if err := f.inbox.MarkProcessed(ctx, msg.ConsumerName, msg.MessageID, uuid.Nil, at); err != nil {
		t.Fatal(err)
	}

	redelivery := msg
	redelivery.ReceivedAt = at.Add(time.Minute)
	redelivery.PayloadHash = [32]byte{9}
	existing, err = f.inbox.InsertIfAbsent(ctx, redelivery)
	if err != nil || existing == nil {
		t.Fatalf("redelivery = %+v, %v", existing, err)
	}
	if existing.PayloadHash != msg.PayloadHash || !existing.ReceivedAt.Equal(at) || !existing.ProcessedAt.Equal(at) {
		t.Errorf("existing = %+v, want original record", existing)
	}

	if err := f.inbox.MarkProcessed(ctx, msg.ConsumerName, msg.MessageID, uuid.Nil, at); !errors.Is(err, app.ErrConcurrentUpdate) {
		t.Errorf("second MarkProcessed = %v", err)
	}

	other := msg
	other.ConsumerName = "another-consumer"
	if existing, err := f.inbox.InsertIfAbsent(ctx, other); err != nil || existing != nil {
		t.Errorf("same message id for another consumer = %+v, %v", existing, err)
	}
}

func TestInboxConcurrentDeliveriesInsertOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	const deliveries = 20
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		inserted int
	)
	for range deliveries {
		wg.Go(func() {
			err := f.txm.WithinTx(ctx, func(ctx context.Context) error {
				existing, err := f.inbox.InsertIfAbsent(ctx, app.InboxMessage{
					ConsumerName: "c", MessageID: "same", PayloadHash: [32]byte{7}, ReceivedAt: now(), ProcessedAt: now(),
				})
				if err != nil {
					return err
				}
				if existing == nil {
					mu.Lock()
					inserted++
					mu.Unlock()
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if inserted != 1 {
		t.Errorf("inserted %d times, want exactly once", inserted)
	}
}

func outboxEvents(t *testing.T, n int) []event.Event {
	t.Helper()
	events := make([]event.Event, 0, n)
	base := now()
	for i := range n {
		opening, err := wagering.NewOpening(wagering.OpeningParams{
			ID: uuid.New(), WalletID: uuid.New(), PlayerID: uuid.New(), Amount: brl(t, "1.00"), Now: base,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := opening.MarkProcessed(wagering.Result{Balance: brl(t, "1.00"), WalletVersion: 1}, uuid.Nil, base); err != nil {
			t.Fatal(err)
		}
		e, err := event.NewWagerTransactionProcessed(event.Metadata{
			EventID: uuid.New(), CorrelationID: "corr", OccurredAt: base.Add(time.Duration(i) * time.Millisecond),
		}, opening)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	return events
}

func TestOutboxRepository(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	at := now()

	events := outboxEvents(t, 3)
	if err := f.outbox.Append(ctx, at, events...); err != nil {
		t.Fatal(err)
	}

	backlog, err := f.outbox.Backlog(ctx)
	if err != nil || backlog.Pending != 3 || !backlog.OldestOccurredAt.Equal(events[0].OccurredAt()) {
		t.Fatalf("backlog = %+v, %v", backlog, err)
	}

	claimed, err := f.outbox.Claim(ctx, "publisher-a", at, 30*time.Second, 10)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	for i, rec := range claimed {
		if rec.EventID != events[i].ID() || rec.Attempts != 1 || rec.PartitionKey != events[i].PartitionKey() {
			t.Errorf("record %d = %+v", i, rec)
		}
		want, _ := events[i].MarshalJSON()
		if string(rec.Payload) != string(want) {
			t.Errorf("record %d payload changed:\n got %s\nwant %s", i, rec.Payload, want)
		}
	}

	if again, err := f.outbox.Claim(ctx, "publisher-b", at, 30*time.Second, 10); err != nil || len(again) != 0 {
		t.Errorf("locked events claimable by another publisher: %d, %v", len(again), err)
	}

	ok, err := f.outbox.MarkPublished(ctx, events[0].ID(), "publisher-b", at)
	if err != nil || ok {
		t.Errorf("non-owner MarkPublished = %v, %v", ok, err)
	}
	ok, err = f.outbox.MarkPublished(ctx, events[0].ID(), "publisher-a", at)
	if err != nil || !ok {
		t.Errorf("owner MarkPublished = %v, %v", ok, err)
	}

	retryAt := at.Add(time.Minute)
	ok, err = f.outbox.ScheduleRetry(ctx, events[1].ID(), "publisher-a", retryAt, "sns unavailable")
	if err != nil || !ok {
		t.Errorf("ScheduleRetry = %v, %v", ok, err)
	}

	abandoned, err := f.outbox.Claim(ctx, "publisher-b", at.Add(31*time.Second), 30*time.Second, 10)
	if err != nil || len(abandoned) != 1 || abandoned[0].EventID != events[2].ID() || abandoned[0].Attempts != 2 {
		t.Fatalf("recovery of abandoned lease = %+v, %v", abandoned, err)
	}

	retried, err := f.outbox.Claim(ctx, "publisher-c", retryAt, 30*time.Second, 10)
	if err != nil || len(retried) != 1 || retried[0].EventID != events[1].ID() {
		t.Fatalf("claim after backoff = %+v, %v", retried, err)
	}

	backlog, err = f.outbox.Backlog(ctx)
	if err != nil || backlog.Pending != 2 {
		t.Errorf("backlog after publish = %+v, %v", backlog, err)
	}
}

func TestOutboxConcurrentPublishersNeverShareRecords(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	at := now()

	const total = 60
	if err := f.outbox.Append(ctx, at, outboxEvents(t, total)...); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		owners = map[uuid.UUID]string{}
		wg     sync.WaitGroup
	)
	for p := range 3 {
		owner := fmt.Sprintf("publisher-%d", p)
		wg.Go(func() {
			for {
				batch, err := f.outbox.Claim(ctx, owner, at, time.Minute, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(batch) == 0 {
					return
				}
				for _, rec := range batch {
					mu.Lock()
					if previous, dup := owners[rec.EventID]; dup {
						t.Errorf("event %s claimed by %s and %s", rec.EventID, previous, owner)
					}
					owners[rec.EventID] = owner
					mu.Unlock()
					if ok, err := f.outbox.MarkPublished(ctx, rec.EventID, owner, at); err != nil || !ok {
						t.Errorf("publish %s by %s = %v, %v", rec.EventID, owner, ok, err)
					}
				}
			}
		})
	}
	wg.Wait()

	if len(owners) != total {
		t.Errorf("published %d events, want %d", len(owners), total)
	}
	if backlog, err := f.outbox.Backlog(ctx); err != nil || backlog.Pending != 0 {
		t.Errorf("backlog = %+v, %v", backlog, err)
	}
}
