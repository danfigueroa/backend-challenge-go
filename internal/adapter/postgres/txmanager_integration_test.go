//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

func TestTxManagerRollsBackOnError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	opened, err := wallet.Open(wallet.OpenParams{ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: now()})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err = f.txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.wallets.Insert(ctx, opened.Wallet); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want boom", err)
	}
	if _, err := f.wallets.Get(ctx, opened.Wallet.ID()); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("wallet persisted despite rollback: %v", err)
	}
}

func TestTxManagerReportsCommitTimeIntegrityViolations(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	w := f.openWallet(t, "50.00")

	err := f.txm.WithinTx(ctx, func(ctx context.Context) error {
		loaded, err := f.wallets.GetForUpdate(ctx, w.ID())
		if err != nil {
			return err
		}
		if _, err := loaded.Debit(wallet.Movement{EntryID: uuid.New(), TransactionID: uuid.New(), Amount: brl(t, "20.00"), Now: now()}); err != nil {
			return err
		}
		return f.wallets.Update(ctx, loaded)
	})
	if !errors.Is(err, app.ErrIntegrityViolation) {
		t.Fatalf("balance change without ledger committed or misclassified: %v", err)
	}
	reloaded, err := f.wallets.Get(ctx, w.ID())
	if err != nil || reloaded.Balance().Amount() != "50.00" || reloaded.Version() != 1 {
		t.Errorf("wallet after failed commit = %v, %v", reloaded, err)
	}
}

func TestTxManagerJoinsOuterTransaction(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	opened, err := wallet.Open(wallet.OpenParams{ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: now()})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("outer failure")
	err = f.txm.WithinTx(ctx, func(ctx context.Context) error {
		if err := f.txm.WithinTx(ctx, func(ctx context.Context) error { return f.wallets.Insert(ctx, opened.Wallet) }); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := f.wallets.Get(ctx, opened.Wallet.ID()); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("inner work must roll back with the outer transaction: %v", err)
	}
}

func TestTxManagerLockTimeoutIsTransient(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	w := f.openWallet(t, "10.00")

	shortTimeouts := postgres.NewTxManager(f.db.AppPool, postgres.Config{
		DSN: "unused", MaxConns: 10, LockTimeout: 200 * time.Millisecond, StatementTimeout: 5 * time.Second,
	})

	locked := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		err := f.txm.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := f.wallets.GetForUpdate(ctx, w.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})
	<-locked

	err := shortTimeouts.WithinTx(ctx, func(ctx context.Context) error {
		_, err := f.wallets.GetForUpdate(ctx, w.ID())
		return err
	})
	close(release)
	wg.Wait()

	if !errors.Is(err, app.ErrTransient) {
		t.Errorf("lock timeout error = %v, want ErrTransient", err)
	}
}

func TestTxManagerSnapshotIsReadOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	opened, err := wallet.Open(wallet.OpenParams{ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: now()})
	if err != nil {
		t.Fatal(err)
	}
	err = f.txm.WithinSnapshot(ctx, func(ctx context.Context) error { return f.wallets.Insert(ctx, opened.Wallet) })
	if err == nil {
		t.Error("write inside read-only snapshot succeeded")
	}
}

func TestSameWalletSerializesAndDistinctWalletsProceedInParallel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	busy := f.openWallet(t, "100.00")
	free := f.openWallet(t, "100.00")

	locked := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		err := f.txm.WithinTx(ctx, func(ctx context.Context) error {
			if _, err := f.wallets.GetForUpdate(ctx, busy.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})
	<-locked

	started := time.Now()
	f.mustProcess(t, free, opInput{kind: "BET", externalID: "free-bet", amount: "10.00"})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("independent wallet waited %v behind a lock on another wallet", elapsed)
	}

	blocked := make(chan error, 1)
	go func() {
		_, _, err := f.process(ctx, t, busy.ID(), f.request(t, busy, opInput{kind: "BET", externalID: "busy-bet", amount: "10.00"}))
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("operation on locked wallet finished before release: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if err := <-blocked; err != nil {
		t.Fatalf("operation after release: %v", err)
	}
}

func TestConcurrentBetsOnSameWalletAtRepositoryLevel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)
	w := f.openWallet(t, "100.00")

	var wg sync.WaitGroup
	results := make(chan wagering.Decision, 2)
	for _, id := range []string{"bet-80-a", "bet-80-b"} {
		wg.Go(func() {
			_, d, err := f.process(ctx, t, w.ID(), f.request(t, w, opInput{kind: "BET", externalID: id, amount: "80.00"}))
			if err != nil {
				t.Error(err)
				return
			}
			results <- d
		})
	}
	wg.Wait()
	close(results)

	outcomes := map[wagering.Outcome]int{}
	for d := range results {
		outcomes[d.Outcome]++
		if d.Outcome == wagering.OutcomeRejected && d.FailureCode != wagering.CodeInsufficientFunds {
			t.Errorf("rejection code = %s", d.FailureCode)
		}
	}
	if outcomes[wagering.OutcomeProcessed] != 1 || outcomes[wagering.OutcomeRejected] != 1 {
		t.Fatalf("outcomes = %v, want one processed and one rejected", outcomes)
	}

	final, err := f.wallets.Get(ctx, w.ID())
	if err != nil || final.Balance().Amount() != "20.00" || final.Version() != 2 {
		t.Fatalf("final wallet = %v, %v", final, err)
	}
	summary, err := f.ledger.Summarize(ctx, w.ID())
	if err != nil || summary.Entries != 2 || summary.NetMinor != final.Balance().Minor() || summary.ChainBreaks != 0 {
		t.Errorf("ledger summary = %+v, %v", summary, err)
	}
}
