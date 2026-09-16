package wagering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

func assertProcessed(t *testing.T, tx *wagering.Transaction, d wagering.Decision, balance string, version int64) {
	t.Helper()
	if d.Outcome != wagering.OutcomeProcessed || tx.Status() != wagering.StatusProcessed {
		t.Fatalf("outcome = %s/%s (%s), want PROCESSED", d.Outcome, tx.Status(), d.FailureCode)
	}
	result, ok := tx.Result()
	if !ok || result.Balance.Amount() != balance || result.WalletVersion != version {
		t.Errorf("result = %+v, want %s v%d", result, balance, version)
	}
}

func assertRejected(t *testing.T, tx *wagering.Transaction, w *wallet.Wallet, d wagering.Decision, code wagering.FailureCode, balance string, version int64) {
	t.Helper()
	if d.Outcome != wagering.OutcomeRejected || tx.Status() != wagering.StatusRejected || d.FailureCode != code || tx.FailureCode() != code {
		t.Fatalf("outcome = %s/%s code %s/%s, want REJECTED %s", d.Outcome, tx.Status(), d.FailureCode, tx.FailureCode(), code)
	}
	if d.Entry != nil {
		t.Error("rejection produced a ledger entry")
	}
	if w.Balance().Amount() != balance || w.Version() != version {
		t.Errorf("wallet = %s v%d, want %s v%d", w.Balance(), w.Version(), balance, version)
	}
}

func TestProcessBet(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "1000.00")
	tx := newTransaction(t, sampleInput)
	d := process(t, tx, w, nil, t0)

	assertProcessed(t, tx, d, "975.00", 2)
	if d.Entry == nil || d.Entry.Direction() != wallet.DirectionDebit || d.Entry.Amount().Amount() != "25.00" ||
		d.Entry.TransactionID() != tx.ID() || d.Entry.BalanceAfter().Amount() != "975.00" || d.Entry.WalletVersion() != 2 {
		t.Errorf("entry = %+v", d.Entry)
	}
}

func TestProcessBetInsufficientFunds(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	first := newTransaction(t, op("BET", "bet-80-a", "80.00"))
	second := newTransaction(t, op("BET", "bet-80-b", "80.00"))

	assertProcessed(t, first, process(t, first, w, nil, t0), "20.00", 2)
	d := process(t, second, w, nil, t0)
	assertRejected(t, second, w, d, wagering.CodeInsufficientFunds, "20.00", 2)

	result, ok := second.Result()
	if !ok || result.Balance.Amount() != "20.00" || result.WalletVersion != 2 {
		t.Errorf("rejection should record the observed balance, got %+v", result)
	}
}

func TestProcessWin(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "10.00")
	tx := newTransaction(t, op("WIN", "win-1", "50.00"))
	d := process(t, tx, w, nil, t0)
	assertProcessed(t, tx, d, "60.00", 2)
	if d.Entry == nil || d.Entry.Direction() != wallet.DirectionCredit {
		t.Errorf("entry = %+v", d.Entry)
	}
}

func TestProcessWinWithReference(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	bet := processed(t, w, op("BET", "bet-1", "10.00"), nil)
	win := newTransaction(t, op("WIN", "win-1", "30.00", withReference("bet-1")))
	d := process(t, win, w, &wagering.Reference{Transaction: bet}, t0)
	assertProcessed(t, win, d, "120.00", 3)
	if win.ReferenceTransactionID() != bet.ID() {
		t.Errorf("resolved reference = %s, want %s", win.ReferenceTransactionID(), bet.ID())
	}
}

func TestProcessLoss(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	tx := newTransaction(t, op("LOSS", "loss-1", "0.00"))
	d := process(t, tx, w, nil, t0)
	assertProcessed(t, tx, d, "100.00", 1)
	if d.Entry != nil || w.HasChanges() || w.Version() != 1 {
		t.Errorf("LOSS must not create ledger entry nor bump version: entry=%v version=%d", d.Entry, w.Version())
	}
}

func TestProcessRefund(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	bet := processed(t, w, op("BET", "bet-1", "40.00"), nil)
	refund := newTransaction(t, op("REFUND", "refund-1", "40.00", withReference("bet-1")))
	d := process(t, refund, w, &wagering.Reference{Transaction: bet}, t0)

	assertProcessed(t, refund, d, "100.00", 3)
	if d.Entry == nil || d.Entry.Direction() != wallet.DirectionCredit || refund.ReferenceTransactionID() != bet.ID() {
		t.Errorf("entry = %+v ref = %s", d.Entry, refund.ReferenceTransactionID())
	}
}

func TestProcessRollback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		original    string
		setupAmount string
		direction   wallet.Direction
		balance     string
	}{
		{"of bet credits", "BET", "40.00", wallet.DirectionCredit, "100.00"},
		{"of win debits", "WIN", "40.00", wallet.DirectionDebit, "100.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWallet(t, "100.00")
			original := processed(t, w, op(tc.original, "orig-1", tc.setupAmount), nil)
			rb := newTransaction(t, op("ROLLBACK", "rb-1", tc.setupAmount, withReference("orig-1")))
			d := process(t, rb, w, &wagering.Reference{Transaction: original}, t0)
			assertProcessed(t, rb, d, tc.balance, 3)
			if d.Entry == nil || d.Entry.Direction() != tc.direction {
				t.Errorf("entry direction = %v, want %s", d.Entry, tc.direction)
			}
		})
	}

	t.Run("of refund debits", func(t *testing.T) {
		t.Parallel()
		w := newWallet(t, "100.00")
		bet := processed(t, w, op("BET", "bet-1", "40.00"), nil)
		refund := processed(t, w, op("REFUND", "refund-1", "40.00", withReference("bet-1")), &wagering.Reference{Transaction: bet})
		rb := newTransaction(t, op("ROLLBACK", "rb-1", "40.00", withReference("refund-1")))
		d := process(t, rb, w, &wagering.Reference{Transaction: refund}, t0)
		assertProcessed(t, rb, d, "60.00", 4)
		if d.Entry.Direction() != wallet.DirectionDebit {
			t.Errorf("direction = %s", d.Entry.Direction())
		}
	})
}

func TestReversalInsufficientFundsUsesDistinctCode(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "0.00")
	win := processed(t, w, op("WIN", "win-1", "50.00"), nil)
	processed(t, w, op("BET", "bet-2", "30.00"), nil)

	rb := newTransaction(t, op("ROLLBACK", "rb-1", "50.00", withReference("win-1")))
	d := process(t, rb, w, &wagering.Reference{Transaction: win}, t0)
	assertRejected(t, rb, w, d, wagering.CodeReversalInsufficientFunds, "20.00", 3)
	if rb.ReferenceTransactionID() != win.ID() {
		t.Errorf("rejected reversal should keep resolved reference for audit")
	}
}

func TestReferenceRules(t *testing.T) {
	t.Parallel()

	otherPlayer := uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a9")
	otherWallet := uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef39")

	type setup func(t *testing.T, w *wallet.Wallet) *wagering.Reference
	processedRef := func(in wagering.RequestInput) setup {
		return func(t *testing.T, w *wallet.Wallet) *wagering.Reference {
			return &wagering.Reference{Transaction: processed(t, w, in, nil)}
		}
	}
	foreignRef := func(mutate func(*wagering.RequestInput), balance string) setup {
		return func(t *testing.T, _ *wallet.Wallet) *wagering.Reference {
			in := op("BET", "orig", "25.00", mutate)
			foreignWallet, err := wallet.Rehydrate(wallet.RehydrateParams{
				ID: uuid.MustParse(in.WalletID), PlayerID: uuid.MustParse(in.PlayerID), Balance: brl(t, balance),
				Version: 1, CreatedAt: t0, UpdatedAt: t0,
			})
			if err != nil {
				t.Fatal(err)
			}
			return &wagering.Reference{Transaction: processed(t, foreignWallet, in, nil)}
		}
	}

	tests := []struct {
		name  string
		ref   setup
		op    wagering.RequestInput
		code  wagering.FailureCode
		await bool
	}{
		{"refund of win", processedRef(op("WIN", "orig", "25.00")), op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceKindInvalid, false},
		{"refund of loss", processedRef(op("LOSS", "orig", "0.00")), op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceKindInvalid, false},
		{"rollback of loss", processedRef(op("LOSS", "orig", "0.00")), op("ROLLBACK", "rev", "25.00", withReference("orig")), wagering.CodeReferenceKindInvalid, false},
		{"win referencing win", processedRef(op("WIN", "orig", "25.00")), op("WIN", "rev", "25.00", withReference("orig")), wagering.CodeReferenceKindInvalid, false},
		{"provider mismatch", foreignRef(func(in *wagering.RequestInput) { in.ProviderID = "provider-b" }, "100.00"), op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceProviderMismatch, false},
		{"player mismatch", foreignRef(func(in *wagering.RequestInput) { in.PlayerID = otherPlayer.String() }, "100.00"), op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferencePlayerMismatch, false},
		{"wallet mismatch", foreignRef(func(in *wagering.RequestInput) { in.WalletID = otherWallet.String() }, "100.00"), op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceWalletMismatch, false},
		{"round mismatch", processedRef(op("BET", "orig", "25.00", func(in *wagering.RequestInput) { in.RoundID = "round-1" })), op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceRoundMismatch, false},
		{"amount mismatch refund", processedRef(op("BET", "orig", "25.00")), op("REFUND", "rev", "24.99", withReference("orig")), wagering.CodeReferenceAmountMismatch, false},
		{"amount mismatch rollback", processedRef(op("BET", "orig", "25.00")), op("ROLLBACK", "rev", "25.01", withReference("orig")), wagering.CodeReferenceAmountMismatch, false},
		{"already reversed", func(t *testing.T, w *wallet.Wallet) *wagering.Reference {
			return &wagering.Reference{Transaction: processed(t, w, op("BET", "orig", "25.00"), nil), AlreadyReversed: true}
		}, op("ROLLBACK", "rev", "25.00", withReference("orig")), wagering.CodeReferenceAlreadyReversed, false},
		{"reference rejected", func(t *testing.T, _ *wallet.Wallet) *wagering.Reference {
			tx := newTransaction(t, op("BET", "orig", "25.00"))
			if err := tx.Reject(wagering.Rejection{Code: wagering.CodeInsufficientFunds}, t0); err != nil {
				t.Fatal(err)
			}
			return &wagering.Reference{Transaction: tx}
		}, op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceNotProcessed, false},
		{"reference failed", func(t *testing.T, _ *wallet.Wallet) *wagering.Reference {
			tx := newTransaction(t, op("BET", "orig", "25.00"))
			if err := tx.Fail(wagering.CodeInternalProcessingFailed, t0); err != nil {
				t.Fatal(err)
			}
			return &wagering.Reference{Transaction: tx}
		}, op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceNotProcessed, false},
		{"reference missing", func(*testing.T, *wallet.Wallet) *wagering.Reference { return nil }, op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceNotFound, true},
		{"reference pending", func(t *testing.T, _ *wallet.Wallet) *wagering.Reference {
			return &wagering.Reference{Transaction: newTransaction(t, op("BET", "orig", "25.00"))}
		}, op("REFUND", "rev", "25.00", withReference("orig")), wagering.CodeReferenceNotProcessed, true},
		{"reference pending reference", func(t *testing.T, _ *wallet.Wallet) *wagering.Reference {
			tx := newTransaction(t, op("REFUND", "orig", "25.00", withReference("bet-0")))
			if err := tx.AwaitReference(t0.Add(time.Second), t0.Add(time.Hour), t0); err != nil {
				t.Fatal(err)
			}
			return &wagering.Reference{Transaction: tx}
		}, op("ROLLBACK", "rev", "25.00", withReference("orig")), wagering.CodeReferenceNotProcessed, true},
		{"win reference missing", func(*testing.T, *wallet.Wallet) *wagering.Reference { return nil }, op("WIN", "rev", "25.00", withReference("orig")), wagering.CodeReferenceNotFound, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWallet(t, "100.00")
			ref := tc.ref(t, w)
			balance, version := w.Balance().Amount(), w.Version()

			tx := newTransaction(t, tc.op)
			d := process(t, tx, w, ref, t0.Add(time.Second))

			if tc.await {
				if d.Outcome != wagering.OutcomeAwaitingReference || tx.Status() != wagering.StatusPendingReference || d.FailureCode != tc.code {
					t.Fatalf("outcome = %s/%s (%s), want AWAITING %s", d.Outcome, tx.Status(), d.FailureCode, tc.code)
				}
				if d.Entry != nil || w.Balance().Amount() != balance || w.Version() != version {
					t.Error("awaiting reference must not move the wallet")
				}
				return
			}
			assertRejected(t, tx, w, d, tc.code, balance, version)
		})
	}
}

func TestPendingReferenceLifecycle(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	refund := newTransaction(t, op("REFUND", "refund-1", "40.00", withReference("bet-1")))

	now := t0
	for attempt := 1; attempt <= 4; attempt++ {
		d := process(t, refund, w, nil, now)
		if d.Outcome != wagering.OutcomeAwaitingReference || refund.Attempts() != attempt {
			t.Fatalf("attempt %d: outcome %s attempts %d", attempt, d.Outcome, refund.Attempts())
		}
		wantDelay := testPolicy.Delay(attempt)
		if got := refund.NextAttemptAt().Sub(now); got != wantDelay {
			t.Errorf("attempt %d: delay %v, want %v", attempt, got, wantDelay)
		}
		if !refund.ExpiresAt().Equal(t0.Add(testPolicy.TTL)) {
			t.Errorf("expires at = %v", refund.ExpiresAt())
		}
		now = refund.NextAttemptAt()
	}

	bet := processed(t, w, op("BET", "bet-1", "40.00"), nil)
	d := process(t, refund, w, &wagering.Reference{Transaction: bet}, now)
	assertProcessed(t, refund, d, "100.00", 3)
}

func TestPendingReferenceExpires(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	rb := newTransaction(t, op("ROLLBACK", "rb-1", "40.00", withReference("bet-404")))

	if d := process(t, rb, w, nil, t0); d.Outcome != wagering.OutcomeAwaitingReference {
		t.Fatalf("outcome = %s", d.Outcome)
	}

	nearExpiry := t0.Add(testPolicy.TTL - time.Second)
	if d := process(t, rb, w, nil, nearExpiry); d.Outcome != wagering.OutcomeAwaitingReference {
		t.Fatalf("outcome near expiry = %s", d.Outcome)
	}
	if !rb.NextAttemptAt().Equal(rb.ExpiresAt()) {
		t.Errorf("next attempt %v should be clamped to expiry %v", rb.NextAttemptAt(), rb.ExpiresAt())
	}

	d := process(t, rb, w, nil, rb.ExpiresAt())
	assertRejected(t, rb, w, d, wagering.CodeReferenceNotFound, "100.00", 1)
}

func TestPendingReferenceExpiresWhileReferenceStillPending(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	pendingBet := newTransaction(t, op("BET", "bet-1", "40.00"))
	refund := newTransaction(t, op("REFUND", "refund-1", "40.00", withReference("bet-1")))
	ref := &wagering.Reference{Transaction: pendingBet}

	process(t, refund, w, ref, t0)
	d := process(t, refund, w, ref, t0.Add(testPolicy.TTL))
	assertRejected(t, refund, w, d, wagering.CodeReferenceNotProcessed, "100.00", 1)
}

func TestRefundAndRollbackCombinations(t *testing.T) {
	t.Parallel()

	w := newWallet(t, "100.00")
	bet := processed(t, w, op("BET", "bet-1", "40.00"), nil)
	refund := processed(t, w, op("REFUND", "refund-1", "40.00", withReference("bet-1")), &wagering.Reference{Transaction: bet})

	secondRefund := newTransaction(t, op("REFUND", "refund-2", "40.00", withReference("bet-1")))
	d := process(t, secondRefund, w, &wagering.Reference{Transaction: bet, AlreadyReversed: true}, t0)
	assertRejected(t, secondRefund, w, d, wagering.CodeReferenceAlreadyReversed, "100.00", 3)

	rollbackOfBet := newTransaction(t, op("ROLLBACK", "rb-bet", "40.00", withReference("bet-1")))
	d = process(t, rollbackOfBet, w, &wagering.Reference{Transaction: bet, AlreadyReversed: true}, t0)
	assertRejected(t, rollbackOfBet, w, d, wagering.CodeReferenceAlreadyReversed, "100.00", 3)

	rollbackOfRefund := processed(t, w, op("ROLLBACK", "rb-refund", "40.00", withReference("refund-1")), &wagering.Reference{Transaction: refund})
	if rollbackOfRefund.ReferenceTransactionID() != refund.ID() || w.Balance().Amount() != "60.00" {
		t.Errorf("rollback of refund: ref %s balance %s", rollbackOfRefund.ReferenceTransactionID(), w.Balance())
	}

	refundAfterRollback := newTransaction(t, op("REFUND", "refund-3", "40.00", withReference("bet-1")))
	d = process(t, refundAfterRollback, w, &wagering.Reference{Transaction: bet, AlreadyReversed: true}, t0)
	assertRejected(t, refundAfterRollback, w, d, wagering.CodeReferenceAlreadyReversed, "60.00", 4)
}

func TestProcessGuards(t *testing.T) {
	t.Parallel()

	t.Run("wallet player mismatch", func(t *testing.T) {
		t.Parallel()
		w, err := wallet.Rehydrate(wallet.RehydrateParams{
			ID: walletID, PlayerID: uuid.New(), Balance: brl(t, "10.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = wagering.Process(wagering.ProcessParams{Transaction: newTransaction(t, sampleInput), Wallet: w, EntryID: uuid.New(), Now: t0, Policy: testPolicy})
		var verr *wagering.ValidationError
		if !errors.As(err, &verr) || verr.Code != wagering.CodeWalletPlayerMismatch {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("wallet currency mismatch", func(t *testing.T) {
		t.Parallel()
		usd, err := money.Parse("10.00", "USD")
		if err != nil {
			t.Fatal(err)
		}
		w, err := wallet.Rehydrate(wallet.RehydrateParams{
			ID: walletID, PlayerID: playerID, Balance: usd, Version: 1, CreatedAt: t0, UpdatedAt: t0,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = wagering.Process(wagering.ProcessParams{Transaction: newTransaction(t, op("LOSS", "loss-1", "0.00")), Wallet: w, EntryID: uuid.New(), Now: t0, Policy: testPolicy})
		var verr *wagering.ValidationError
		if !errors.As(err, &verr) || verr.Code != wagering.CodeWalletCurrencyMismatch {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("different wallet", func(t *testing.T) {
		t.Parallel()
		w, err := wallet.Rehydrate(wallet.RehydrateParams{
			ID: uuid.New(), PlayerID: playerID, Balance: brl(t, "10.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = wagering.Process(wagering.ProcessParams{Transaction: newTransaction(t, sampleInput), Wallet: w, EntryID: uuid.New(), Now: t0, Policy: testPolicy})
		if !errors.Is(err, wagering.ErrWalletMismatch) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("terminal transaction", func(t *testing.T) {
		t.Parallel()
		w := newWallet(t, "100.00")
		tx := processed(t, w, sampleInput, nil)
		_, err := wagering.Process(wagering.ProcessParams{Transaction: tx, Wallet: w, EntryID: uuid.New(), Now: t0, Policy: testPolicy})
		if !errors.Is(err, wagering.ErrInvalidTransition) || w.Balance().Amount() != "75.00" {
			t.Errorf("replaying a processed transaction must fail without moving money: %v, %s", err, w.Balance())
		}
	})

	t.Run("invalid policy", func(t *testing.T) {
		t.Parallel()
		_, err := wagering.Process(wagering.ProcessParams{
			Transaction: newTransaction(t, sampleInput), Wallet: newWallet(t, "100.00"), EntryID: uuid.New(), Now: t0,
		})
		if !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("missing entry id leaves wallet untouched", func(t *testing.T) {
		t.Parallel()
		w := newWallet(t, "100.00")
		tx := newTransaction(t, sampleInput)
		_, err := wagering.Process(wagering.ProcessParams{Transaction: tx, Wallet: w, Now: t0, Policy: testPolicy})
		if !errors.Is(err, wallet.ErrInvalidLedgerEntry) || w.Version() != 1 || tx.Status() != wagering.StatusPending {
			t.Errorf("error = %v, version %d, status %s", err, w.Version(), tx.Status())
		}
	})

	t.Run("balance limit", func(t *testing.T) {
		t.Parallel()
		huge, err := money.Parse("92233720368547758.00", "BRL")
		if err != nil {
			t.Fatal(err)
		}
		w, err := wallet.Rehydrate(wallet.RehydrateParams{ID: walletID, PlayerID: playerID, Balance: huge, Version: 1, CreatedAt: t0, UpdatedAt: t0})
		if err != nil {
			t.Fatal(err)
		}
		tx := newTransaction(t, op("WIN", "win-big", "1.00"))
		d := process(t, tx, w, nil, t0)
		assertRejected(t, tx, w, d, wagering.CodeBalanceLimitExceeded, "92233720368547758.00", 1)
	})
}

func TestPendingPolicyDelay(t *testing.T) {
	t.Parallel()

	p := wagering.PendingPolicy{TTL: time.Hour, BaseDelay: time.Second, MaxDelay: 10 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, w := range want {
		if got := p.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := p.Delay(10_000); got != 10*time.Second {
		t.Errorf("Delay(huge) = %v", got)
	}

	p.Jitter = func(d time.Duration) time.Duration { return d / 2 }
	if got := p.Delay(2); got != 3*time.Second {
		t.Errorf("Delay with jitter = %v", got)
	}
	p.Jitter = func(time.Duration) time.Duration { return -time.Hour }
	if got := p.Delay(1); got != time.Second {
		t.Errorf("negative jitter must be ignored, got %v", got)
	}

	for _, bad := range []wagering.PendingPolicy{
		{TTL: 0, BaseDelay: time.Second, MaxDelay: time.Second},
		{TTL: time.Hour, BaseDelay: 0, MaxDelay: time.Second},
		{TTL: time.Hour, BaseDelay: 2 * time.Second, MaxDelay: time.Second},
	} {
		if err := bad.Validate(); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("Validate(%+v) = %v", bad, err)
		}
	}
}
