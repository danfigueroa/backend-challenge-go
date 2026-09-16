package wagering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

func TestNewExternalStartsPending(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	req := newRequest(t, op("REFUND", "refund-1", "25.00", withReference("bet-1")))
	tx, err := wagering.NewExternal(id, req, t0)
	if err != nil {
		t.Fatal(err)
	}
	if tx.ID() != id || tx.Origin() != wagering.OriginExternal || tx.Kind() != wagering.KindRefund ||
		tx.Status() != wagering.StatusPending || tx.WalletID() != walletID || tx.PlayerID() != playerID ||
		tx.ProviderID() != "provider-a" || tx.ExternalTransactionID() != "refund-1" ||
		tx.IdempotencyKey() != "provider-a:refund-1" || tx.PayloadHash() != req.PayloadHash() ||
		tx.RoundID() != "round-987" || tx.GameID() != "fortune-chimp" ||
		tx.ReferenceExternalTransactionID() != "bet-1" || tx.Money().String() != "25.00 BRL" {
		t.Errorf("transaction does not reflect request: %+v", tx)
	}
	if tx.ReferenceTransactionID() != uuid.Nil || tx.FailureCode() != "" || tx.Attempts() != 0 ||
		!tx.CompletedAt().IsZero() || !tx.CreatedAt().Equal(t0) || !tx.UpdatedAt().Equal(t0) {
		t.Errorf("unexpected initial state: %+v", tx)
	}
	if _, ok := tx.Result(); ok {
		t.Error("pending transaction has a result")
	}
	if !tx.MatchesPayload(req.PayloadHash()) {
		t.Error("MatchesPayload(own hash) = false")
	}
	other := newRequest(t, op("REFUND", "refund-1", "25.01", withReference("bet-1")))
	if tx.MatchesPayload(other.PayloadHash()) {
		t.Error("MatchesPayload(different payload) = true")
	}
}

func TestNewExternalRejectsUnbuiltRequest(t *testing.T) {
	t.Parallel()

	if _, err := wagering.NewExternal(uuid.New(), wagering.Request{}, t0); !errors.Is(err, wagering.ErrInvalidState) {
		t.Errorf("error = %v, want ErrInvalidState", err)
	}
	if _, err := wagering.NewExternal(uuid.Nil, newRequest(t, sampleInput), t0); !errors.Is(err, wagering.ErrInvalidState) {
		t.Errorf("nil id error = %v", err)
	}
	if _, err := wagering.NewExternal(uuid.New(), newRequest(t, sampleInput), time.Time{}); !errors.Is(err, wagering.ErrInvalidState) {
		t.Errorf("zero time error = %v", err)
	}
}

func TestNewOpening(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	tx, err := wagering.NewOpening(wagering.OpeningParams{ID: id, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "1000.00"), Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Origin() != wagering.OriginInternal || tx.Kind() != wagering.KindOpening || tx.Status() != wagering.StatusPending {
		t.Errorf("opening = %s %s %s", tx.Origin(), tx.Kind(), tx.Status())
	}
	if tx.ProviderID() != "" || tx.ExternalTransactionID() != "" || tx.IdempotencyKey() != "" || !tx.PayloadHash().IsZero() ||
		tx.RoundID() != "" || tx.GameID() != "" || tx.ReferenceExternalTransactionID() != "" {
		t.Errorf("opening carries external metadata: %+v", tx)
	}

	if err := tx.MarkProcessed(wagering.Result{Balance: brl(t, "1000.00"), WalletVersion: 1}, uuid.Nil, t0); err != nil {
		t.Fatal(err)
	}
	result, ok := tx.Result()
	if !ok || tx.Status() != wagering.StatusProcessed || result.Balance.Amount() != "1000.00" || result.WalletVersion != 1 {
		t.Errorf("processed opening = %s %+v", tx.Status(), result)
	}

	invalid := []wagering.OpeningParams{
		{ID: uuid.Nil, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "1.00"), Now: t0},
		{ID: id, WalletID: uuid.Nil, PlayerID: playerID, Amount: brl(t, "1.00"), Now: t0},
		{ID: id, WalletID: walletID, PlayerID: uuid.Nil, Amount: brl(t, "1.00"), Now: t0},
		{ID: id, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "0.00"), Now: t0},
		{ID: id, WalletID: walletID, PlayerID: playerID, Amount: money.Money{}, Now: t0},
		{ID: id, WalletID: walletID, PlayerID: playerID, Amount: brl(t, "1.00")},
	}
	for i, p := range invalid {
		if _, err := wagering.NewOpening(p); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("case %d: error = %v, want ErrInvalidState", i, err)
		}
	}
}

func TestStatusTransitions(t *testing.T) {
	t.Parallel()

	statuses := []wagering.Status{
		wagering.StatusPending, wagering.StatusPendingReference, wagering.StatusProcessed, wagering.StatusRejected, wagering.StatusFailed,
	}
	allowed := map[wagering.Status]map[wagering.Status]bool{
		wagering.StatusPending: {
			wagering.StatusPendingReference: true, wagering.StatusProcessed: true, wagering.StatusRejected: true, wagering.StatusFailed: true,
		},
		wagering.StatusPendingReference: {
			wagering.StatusPendingReference: true, wagering.StatusProcessed: true, wagering.StatusRejected: true, wagering.StatusFailed: true,
		},
	}
	for _, from := range statuses {
		if from.IsTerminal() != (allowed[from] == nil) {
			t.Errorf("%s.IsTerminal() = %v", from, from.IsTerminal())
		}
		for _, to := range statuses {
			if got := from.CanTransitionTo(to); got != allowed[from][to] {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, allowed[from][to])
			}
		}
	}
}

func TestTerminalTransactionsRejectTransitions(t *testing.T) {
	t.Parallel()

	result := wagering.Result{Balance: brl(t, "975.00"), WalletVersion: 2}
	terminal := map[string]func(*wagering.Transaction) error{
		"processed": func(tx *wagering.Transaction) error { return tx.MarkProcessed(result, uuid.Nil, t0) },
		"rejected": func(tx *wagering.Transaction) error {
			return tx.Reject(wagering.Rejection{Code: wagering.CodeInsufficientFunds}, t0)
		},
		"failed": func(tx *wagering.Transaction) error { return tx.Fail(wagering.CodeInternalProcessingFailed, t0) },
	}
	for name, finish := range terminal {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := newTransaction(t, op("WIN", "win-"+name, "25.00"))
			if err := finish(tx); err != nil {
				t.Fatal(err)
			}
			status, code, completed := tx.Status(), tx.FailureCode(), tx.CompletedAt()
			later := t0.Add(time.Hour)

			attempts := map[string]error{
				"MarkProcessed":  tx.MarkProcessed(result, uuid.New(), later),
				"Reject":         tx.Reject(wagering.Rejection{Code: wagering.CodeReferenceNotFound}, later),
				"Fail":           tx.Fail(wagering.CodeInternalProcessingFailed, later),
				"AwaitReference": tx.AwaitReference(later, later.Add(time.Hour), later),
			}
			for op, err := range attempts {
				if !errors.Is(err, wagering.ErrInvalidTransition) {
					t.Errorf("%s on %s: error = %v, want ErrInvalidTransition", op, status, err)
				}
			}
			if tx.Status() != status || tx.FailureCode() != code || !tx.CompletedAt().Equal(completed) {
				t.Errorf("terminal transaction mutated: %s/%s/%v", tx.Status(), tx.FailureCode(), tx.CompletedAt())
			}
		})
	}
}

func TestTransitionGuards(t *testing.T) {
	t.Parallel()

	t.Run("reject requires definitive code", func(t *testing.T) {
		t.Parallel()
		for _, code := range []wagering.FailureCode{wagering.CodeInvalidAmount, wagering.CodeInternalProcessingFailed, wagering.CodeIdempotencyKeyConflict, "UNKNOWN"} {
			tx := newTransaction(t, sampleInput)
			if err := tx.Reject(wagering.Rejection{Code: code}, t0); !errors.Is(err, wagering.ErrInvalidState) {
				t.Errorf("Reject(%s) error = %v", code, err)
			}
			if tx.Status() != wagering.StatusPending {
				t.Errorf("status = %s after invalid rejection", tx.Status())
			}
		}
	})

	t.Run("fail requires infrastructure code", func(t *testing.T) {
		t.Parallel()
		tx := newTransaction(t, sampleInput)
		if err := tx.Fail(wagering.CodeInsufficientFunds, t0); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("processed requires valid result", func(t *testing.T) {
		t.Parallel()
		usd, err := money.Parse("1.00", "USD")
		if err != nil {
			t.Fatal(err)
		}
		negative, err := money.New(-1, money.BRL)
		if err != nil {
			t.Fatal(err)
		}
		for name, r := range map[string]wagering.Result{
			"uninitialized": {WalletVersion: 1},
			"negative":      {Balance: negative, WalletVersion: 1},
			"currency":      {Balance: usd, WalletVersion: 1},
			"version":       {Balance: brl(t, "1.00"), WalletVersion: 0},
		} {
			tx := newTransaction(t, sampleInput)
			if err := tx.MarkProcessed(r, uuid.Nil, t0); !errors.Is(err, wagering.ErrInvalidState) {
				t.Errorf("%s: error = %v", name, err)
			}
		}
	})

	t.Run("processed reversal requires resolved reference", func(t *testing.T) {
		t.Parallel()
		tx := newTransaction(t, op("REFUND", "refund-x", "25.00", withReference("bet-x")))
		err := tx.MarkProcessed(wagering.Result{Balance: brl(t, "1.00"), WalletVersion: 2}, uuid.Nil, t0)
		if !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("await requires reference", func(t *testing.T) {
		t.Parallel()
		tx := newTransaction(t, sampleInput)
		if err := tx.AwaitReference(t0.Add(time.Second), t0.Add(time.Hour), t0); !errors.Is(err, wagering.ErrInvalidTransition) {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("await schedule bounds", func(t *testing.T) {
		t.Parallel()
		tx := newTransaction(t, op("REFUND", "refund-y", "25.00", withReference("bet-y")))
		if err := tx.AwaitReference(t0.Add(2*time.Hour), t0.Add(time.Hour), t0); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("next after expiry error = %v", err)
		}
		if err := tx.AwaitReference(t0.Add(time.Second), t0, t0); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("expiry not after creation error = %v", err)
		}
		if err := tx.AwaitReference(t0.Add(-time.Second), t0.Add(time.Hour), t0); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("next in the past error = %v", err)
		}
	})

	t.Run("zero timestamp", func(t *testing.T) {
		t.Parallel()
		tx := newTransaction(t, sampleInput)
		if err := tx.Fail(wagering.CodeInternalProcessingFailed, time.Time{}); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("error = %v", err)
		}
	})
}

func TestAwaitReferenceKeepsOriginalExpiration(t *testing.T) {
	t.Parallel()

	tx := newTransaction(t, op("ROLLBACK", "rb-1", "25.00", withReference("bet-1")))
	expires := t0.Add(30 * time.Minute)
	if err := tx.AwaitReference(t0.Add(time.Second), expires, t0); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != wagering.StatusPendingReference || tx.Attempts() != 1 || !tx.ExpiresAt().Equal(expires) {
		t.Fatalf("after first await: %s attempts=%d expires=%v", tx.Status(), tx.Attempts(), tx.ExpiresAt())
	}
	now := t0.Add(5 * time.Second)
	if err := tx.AwaitReference(now.Add(2*time.Second), t0.Add(24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if tx.Attempts() != 2 || !tx.ExpiresAt().Equal(expires) || !tx.NextAttemptAt().Equal(now.Add(2*time.Second)) || !tx.UpdatedAt().Equal(now) {
		t.Errorf("after retry: attempts=%d expires=%v next=%v updated=%v", tx.Attempts(), tx.ExpiresAt(), tx.NextAttemptAt(), tx.UpdatedAt())
	}
	if err := tx.MarkProcessed(wagering.Result{Balance: brl(t, "25.00"), WalletVersion: 3}, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	if !tx.NextAttemptAt().IsZero() || !tx.CompletedAt().Equal(now) {
		t.Errorf("completion did not clear schedule: next=%v completed=%v", tx.NextAttemptAt(), tx.CompletedAt())
	}
}

func rehydrateParams(t *testing.T) wagering.RehydrateParams {
	t.Helper()
	req := newRequest(t, sampleInput)
	return wagering.RehydrateParams{
		ID: uuid.New(), Origin: wagering.OriginExternal, Kind: wagering.KindBet, Status: wagering.StatusProcessed,
		WalletID: walletID, PlayerID: playerID, Money: req.Money(),
		ProviderID: req.ProviderID(), ExternalTransactionID: req.ExternalTransactionID(), IdempotencyKey: req.IdempotencyKey(),
		PayloadHash: req.PayloadHash(), RoundID: req.RoundID(), GameID: req.GameID(),
		Result:    wagering.Result{Balance: brl(t, "975.00"), WalletVersion: 2},
		CreatedAt: t0, UpdatedAt: t0, CompletedAt: t0,
	}
}

func TestRehydrate(t *testing.T) {
	t.Parallel()

	p := rehydrateParams(t)
	tx, err := wagering.Rehydrate(p)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := tx.Result()
	if !ok || tx.Status() != wagering.StatusProcessed || result.Balance.Amount() != "975.00" || result.WalletVersion != 2 {
		t.Errorf("rehydrated = %s %+v", tx.Status(), result)
	}
	if err := tx.MarkProcessed(result, uuid.Nil, t0.Add(time.Hour)); !errors.Is(err, wagering.ErrInvalidTransition) {
		t.Errorf("rehydrated terminal accepted a transition: %v", err)
	}

	opening, err := wagering.Rehydrate(wagering.RehydrateParams{
		ID: uuid.New(), Origin: wagering.OriginInternal, Kind: wagering.KindOpening, Status: wagering.StatusProcessed,
		WalletID: walletID, PlayerID: playerID, Money: brl(t, "1000.00"),
		Result:    wagering.Result{Balance: brl(t, "1000.00"), WalletVersion: 1},
		CreatedAt: t0, UpdatedAt: t0, CompletedAt: t0,
	})
	if err != nil || opening.Origin() != wagering.OriginInternal {
		t.Errorf("rehydrate opening = %v, %v", opening, err)
	}
}

func TestRehydrateValidation(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*wagering.RehydrateParams){
		"nil id":                      func(p *wagering.RehydrateParams) { p.ID = uuid.Nil },
		"unknown kind":                func(p *wagering.RehydrateParams) { p.Kind = "JACKPOT" },
		"unknown status":              func(p *wagering.RehydrateParams) { p.Status = "DONE" },
		"unknown origin":              func(p *wagering.RehydrateParams) { p.Origin = "PARTNER" },
		"uninitialized money":         func(p *wagering.RehydrateParams) { p.Money = money.Money{} },
		"missing created at":          func(p *wagering.RehydrateParams) { p.CreatedAt = time.Time{} },
		"updated before created":      func(p *wagering.RehydrateParams) { p.UpdatedAt = t0.Add(-time.Second) },
		"negative attempts":           func(p *wagering.RehydrateParams) { p.Attempts = -1 },
		"external opening":            func(p *wagering.RehydrateParams) { p.Kind = wagering.KindOpening },
		"internal with metadata":      func(p *wagering.RehydrateParams) { p.Origin = wagering.OriginInternal; p.Kind = wagering.KindOpening },
		"internal non opening":        func(p *wagering.RehydrateParams) { p.Origin = wagering.OriginInternal },
		"external without provider":   func(p *wagering.RehydrateParams) { p.ProviderID = "" },
		"external without hash":       func(p *wagering.RehydrateParams) { p.PayloadHash = wagering.PayloadHash{} },
		"refund without reference":    func(p *wagering.RehydrateParams) { p.Kind = wagering.KindRefund },
		"bet with reference":          func(p *wagering.RehydrateParams) { p.ReferenceExternalTransactionID = "bet-0" },
		"loss with amount":            func(p *wagering.RehydrateParams) { p.Kind = wagering.KindLoss },
		"processed without result":    func(p *wagering.RehydrateParams) { p.Result = wagering.Result{} },
		"processed with failure code": func(p *wagering.RehydrateParams) { p.FailureCode = wagering.CodeInsufficientFunds },
		"processed without completion": func(p *wagering.RehydrateParams) {
			p.CompletedAt = time.Time{}
		},
		"rejected with correctable code": func(p *wagering.RehydrateParams) {
			p.Status = wagering.StatusRejected
			p.FailureCode = wagering.CodeInvalidAmount
		},
		"failed with definitive code": func(p *wagering.RehydrateParams) {
			p.Status = wagering.StatusFailed
			p.FailureCode = wagering.CodeInsufficientFunds
		},
		"pending with result": func(p *wagering.RehydrateParams) { p.Status = wagering.StatusPending },
		"pending reference without schedule": func(p *wagering.RehydrateParams) {
			p.Status = wagering.StatusPendingReference
			p.Result = wagering.Result{}
			p.CompletedAt = time.Time{}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := rehydrateParams(t)
			mutate(&p)
			if _, err := wagering.Rehydrate(p); !errors.Is(err, wagering.ErrInvalidState) {
				t.Fatalf("error = %v, want ErrInvalidState", err)
			}
		})
	}
}

func TestParsers(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"OPENING", "BET", "WIN", "LOSS", "REFUND", "ROLLBACK"} {
		if k, err := wagering.ParseKind(s); err != nil || k.String() != s {
			t.Errorf("ParseKind(%q) = %v, %v", s, k, err)
		}
	}
	for _, s := range []string{"PENDING", "PENDING_REFERENCE", "PROCESSED", "REJECTED", "FAILED"} {
		if st, err := wagering.ParseStatus(s); err != nil || st.String() != s {
			t.Errorf("ParseStatus(%q) = %v, %v", s, st, err)
		}
	}
	for _, s := range []string{"INTERNAL", "EXTERNAL"} {
		if o, err := wagering.ParseOrigin(s); err != nil || string(o) != s {
			t.Errorf("ParseOrigin(%q) = %v, %v", s, o, err)
		}
	}
	for code, category := range wagering.FailureCodes() {
		parsed, err := wagering.ParseFailureCode(code.String())
		if err != nil || parsed != code || parsed.Category() != category {
			t.Errorf("ParseFailureCode(%s) = %v, %v", code, parsed, err)
		}
	}
	for _, bad := range []string{"", "bet", "UNKNOWN"} {
		if _, err := wagering.ParseKind(bad); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("ParseKind(%q) error = %v", bad, err)
		}
		if _, err := wagering.ParseStatus(bad); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("ParseStatus(%q) error = %v", bad, err)
		}
		if _, err := wagering.ParseOrigin(bad); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("ParseOrigin(%q) error = %v", bad, err)
		}
		if _, err := wagering.ParseFailureCode(bad); !errors.Is(err, wagering.ErrInvalidState) {
			t.Errorf("ParseFailureCode(%q) error = %v", bad, err)
		}
	}
}

func TestReversalAndInsufficientFundsCodesAreDistinct(t *testing.T) {
	t.Parallel()

	if wagering.CodeInsufficientFunds == wagering.CodeReversalInsufficientFunds {
		t.Fatal("reversal and bet insufficient funds codes must differ")
	}
	for _, code := range []wagering.FailureCode{wagering.CodeInsufficientFunds, wagering.CodeReversalInsufficientFunds, wagering.CodeReferenceNotFound} {
		if code.Category() != wagering.CategoryDefinitive {
			t.Errorf("%s category = %s", code, code.Category())
		}
	}
}
