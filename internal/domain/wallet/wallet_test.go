package wallet_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

var t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openWallet(t *testing.T, initial string) *wallet.Wallet {
	t.Helper()
	opened, err := wallet.Open(wallet.OpenParams{
		ID:                   uuid.New(),
		PlayerID:             uuid.New(),
		InitialBalance:       brl(t, initial),
		OpeningTransactionID: uuid.New(),
		OpeningEntryID:       uuid.New(),
		Now:                  t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return opened.Wallet
}

func movement(amount money.Money, at time.Time) wallet.Movement {
	return wallet.Movement{EntryID: uuid.New(), TransactionID: uuid.New(), Amount: amount, Now: at}
}

func TestOpenWithPositiveBalance(t *testing.T) {
	t.Parallel()

	id, player, txID, entryID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	opened, err := wallet.Open(wallet.OpenParams{
		ID: id, PlayerID: player, InitialBalance: brl(t, "1000.00"),
		OpeningTransactionID: txID, OpeningEntryID: entryID,
		Now: t0.In(time.FixedZone("BRT", -3*3600)),
	})
	if err != nil {
		t.Fatal(err)
	}

	w := opened.Wallet
	if w.ID() != id || w.PlayerID() != player || w.Currency() != money.BRL {
		t.Errorf("identity = %s/%s/%s", w.ID(), w.PlayerID(), w.Currency())
	}
	if w.Balance().Amount() != "1000.00" || w.Version() != 1 {
		t.Errorf("balance/version = %s/%d, want 1000.00/1", w.Balance(), w.Version())
	}
	if !w.IsNew() || w.LoadedVersion() != 0 {
		t.Errorf("IsNew = %v, LoadedVersion = %d", w.IsNew(), w.LoadedVersion())
	}
	if w.CreatedAt().Location() != time.UTC || !w.CreatedAt().Equal(t0) || !w.UpdatedAt().Equal(t0) {
		t.Errorf("timestamps = %v/%v, want UTC %v", w.CreatedAt(), w.UpdatedAt(), t0)
	}

	e := opened.OpeningEntry
	if e == nil {
		t.Fatal("opening entry is nil for positive initial balance")
	}
	if e.ID() != entryID || e.TransactionID() != txID || e.WalletID() != id {
		t.Errorf("entry ids = %s/%s/%s", e.ID(), e.TransactionID(), e.WalletID())
	}
	if e.Direction() != wallet.DirectionCredit || e.Amount().Amount() != "1000.00" ||
		e.BalanceBefore().Amount() != "0.00" || e.BalanceAfter().Amount() != "1000.00" || e.WalletVersion() != 1 {
		t.Errorf("entry = %s %s %s→%s v%d", e.Direction(), e.Amount(), e.BalanceBefore(), e.BalanceAfter(), e.WalletVersion())
	}
}

func TestOpenWithZeroBalanceHasNoEntry(t *testing.T) {
	t.Parallel()

	opened, err := wallet.Open(wallet.OpenParams{
		ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "0.00"), Now: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened.OpeningEntry != nil {
		t.Errorf("opening entry = %+v, want nil", opened.OpeningEntry)
	}
	if opened.Wallet.Version() != 1 || !opened.Wallet.Balance().IsZero() {
		t.Errorf("wallet = %s v%d", opened.Wallet.Balance(), opened.Wallet.Version())
	}
}

func TestOpenValidation(t *testing.T) {
	t.Parallel()

	valid := func() wallet.OpenParams {
		return wallet.OpenParams{
			ID: uuid.New(), PlayerID: uuid.New(), InitialBalance: brl(t, "10.00"),
			OpeningTransactionID: uuid.New(), OpeningEntryID: uuid.New(), Now: t0,
		}
	}
	negative, err := money.New(-1, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*wallet.OpenParams)
		want   error
	}{
		{"nil id", func(p *wallet.OpenParams) { p.ID = uuid.Nil }, wallet.ErrInvalidWallet},
		{"nil player", func(p *wallet.OpenParams) { p.PlayerID = uuid.Nil }, wallet.ErrInvalidWallet},
		{"uninitialized balance", func(p *wallet.OpenParams) { p.InitialBalance = money.Money{} }, wallet.ErrInvalidWallet},
		{"negative balance", func(p *wallet.OpenParams) { p.InitialBalance = negative }, wallet.ErrInvalidAmount},
		{"zero time", func(p *wallet.OpenParams) { p.Now = time.Time{} }, wallet.ErrInvalidWallet},
		{"missing opening transaction", func(p *wallet.OpenParams) { p.OpeningTransactionID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
		{"missing opening entry", func(p *wallet.OpenParams) { p.OpeningEntryID = uuid.Nil }, wallet.ErrInvalidLedgerEntry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := valid()
			tc.mutate(&p)
			opened, err := wallet.Open(p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if opened.Wallet != nil {
				t.Error("wallet returned alongside error")
			}
		})
	}
}

func TestDebitAndCredit(t *testing.T) {
	t.Parallel()

	w := openWallet(t, "100.00")
	t1 := t0.Add(time.Minute)

	debit, err := w.Debit(movement(brl(t, "80.00"), t1))
	if err != nil {
		t.Fatal(err)
	}
	if debit.Direction() != wallet.DirectionDebit || debit.BalanceBefore().Amount() != "100.00" ||
		debit.BalanceAfter().Amount() != "20.00" || debit.WalletVersion() != 2 || debit.WalletID() != w.ID() {
		t.Errorf("debit entry = %s %s→%s v%d", debit.Direction(), debit.BalanceBefore(), debit.BalanceAfter(), debit.WalletVersion())
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 || !w.UpdatedAt().Equal(t1) {
		t.Errorf("wallet = %s v%d updated %v", w.Balance(), w.Version(), w.UpdatedAt())
	}

	credit, err := w.Credit(movement(brl(t, "5.50"), t1.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if credit.BalanceBefore().Amount() != "20.00" || credit.BalanceAfter().Amount() != "25.50" || credit.WalletVersion() != 3 {
		t.Errorf("credit entry = %s→%s v%d", credit.BalanceBefore(), credit.BalanceAfter(), credit.WalletVersion())
	}
	if w.Balance().Amount() != "25.50" || w.Version() != 3 {
		t.Errorf("wallet = %s v%d", w.Balance(), w.Version())
	}
}

func TestDebitExactBalanceReachesZero(t *testing.T) {
	t.Parallel()

	w := openWallet(t, "100.00")
	if _, err := w.Debit(movement(brl(t, "100.00"), t0)); err != nil {
		t.Fatal(err)
	}
	if !w.Balance().IsZero() {
		t.Errorf("balance = %s, want 0.00", w.Balance())
	}
}

func TestDebitInsufficientFundsLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	w := openWallet(t, "100.00")
	if _, err := w.Debit(movement(brl(t, "80.00"), t0)); err != nil {
		t.Fatal(err)
	}

	entry, err := w.Debit(movement(brl(t, "80.00"), t0.Add(time.Hour)))
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("error = %v, want ErrInsufficientFunds", err)
	}
	if entry != (wallet.LedgerEntry{}) {
		t.Error("ledger entry returned alongside error")
	}
	if w.Balance().Amount() != "20.00" || w.Version() != 2 || !w.UpdatedAt().Equal(t0) {
		t.Errorf("wallet mutated on rejection: %s v%d updated %v", w.Balance(), w.Version(), w.UpdatedAt())
	}

	ok, err := w.CanDebit(brl(t, "20.01"))
	if err != nil || ok {
		t.Errorf("CanDebit(20.01) = %v, %v", ok, err)
	}
	ok, err = w.CanDebit(brl(t, "20.00"))
	if err != nil || !ok {
		t.Errorf("CanDebit(20.00) = %v, %v", ok, err)
	}
}

func TestMovementValidation(t *testing.T) {
	t.Parallel()

	usd, err := money.Parse("1.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	negative, err := money.New(-100, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		m    func(t *testing.T) wallet.Movement
		want error
	}{
		{"zero amount", func(t *testing.T) wallet.Movement { return movement(brl(t, "0.00"), t0) }, wallet.ErrInvalidAmount},
		{"negative amount", func(*testing.T) wallet.Movement { return movement(negative, t0) }, wallet.ErrInvalidAmount},
		{"uninitialized amount", func(*testing.T) wallet.Movement { return movement(money.Money{}, t0) }, wallet.ErrInvalidAmount},
		{"currency mismatch", func(*testing.T) wallet.Movement { return movement(usd, t0) }, wallet.ErrCurrencyMismatch},
		{"missing entry id", func(t *testing.T) wallet.Movement {
			m := movement(brl(t, "1.00"), t0)
			m.EntryID = uuid.Nil
			return m
		}, wallet.ErrInvalidLedgerEntry},
		{"missing transaction id", func(t *testing.T) wallet.Movement {
			m := movement(brl(t, "1.00"), t0)
			m.TransactionID = uuid.Nil
			return m
		}, wallet.ErrInvalidLedgerEntry},
		{"missing timestamp", func(t *testing.T) wallet.Movement { return movement(brl(t, "1.00"), time.Time{}) }, wallet.ErrInvalidLedgerEntry},
	}
	for _, tc := range tests {
		for _, op := range []string{"debit", "credit"} {
			t.Run(op+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				w := openWallet(t, "10.00")
				var err error
				if op == "debit" {
					_, err = w.Debit(tc.m(t))
				} else {
					_, err = w.Credit(tc.m(t))
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
				if w.Balance().Amount() != "10.00" || w.Version() != 1 {
					t.Errorf("wallet mutated: %s v%d", w.Balance(), w.Version())
				}
			})
		}
	}
}

func TestCreditOverflow(t *testing.T) {
	t.Parallel()

	huge, err := money.New(math.MaxInt64, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: huge, Version: 7, CreatedAt: t0, UpdatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Credit(movement(brl(t, "0.01"), t0)); !errors.Is(err, wallet.ErrBalanceOverflow) {
		t.Fatalf("error = %v, want ErrBalanceOverflow", err)
	}
	if w.Version() != 7 || !w.Balance().Equal(huge) {
		t.Errorf("wallet mutated: %s v%d", w.Balance(), w.Version())
	}
}

func TestRehydrate(t *testing.T) {
	t.Parallel()

	id, player := uuid.New(), uuid.New()
	updated := t0.Add(time.Hour)
	w, err := wallet.Rehydrate(wallet.RehydrateParams{
		ID: id, PlayerID: player, Balance: brl(t, "975.00"), Version: 2, CreatedAt: t0, UpdatedAt: updated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.Balance().Amount() != "975.00" || w.Version() != 2 || w.LoadedVersion() != 2 {
		t.Errorf("rehydrated = %s v%d loaded v%d", w.Balance(), w.Version(), w.LoadedVersion())
	}
	if w.IsNew() || w.HasChanges() {
		t.Errorf("IsNew = %v, HasChanges = %v", w.IsNew(), w.HasChanges())
	}
	if !w.CreatedAt().Equal(t0) || !w.UpdatedAt().Equal(updated) {
		t.Errorf("timestamps = %v/%v", w.CreatedAt(), w.UpdatedAt())
	}

	if _, err := w.Debit(movement(brl(t, "75.00"), updated.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if w.Version() != 3 || w.LoadedVersion() != 2 || !w.HasChanges() {
		t.Errorf("after debit: v%d loaded v%d changes %v", w.Version(), w.LoadedVersion(), w.HasChanges())
	}
}

func TestRehydrateValidation(t *testing.T) {
	t.Parallel()

	negative, err := money.New(-1, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	valid := func() wallet.RehydrateParams {
		return wallet.RehydrateParams{
			ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, "1.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0,
		}
	}
	tests := []struct {
		name   string
		mutate func(*wallet.RehydrateParams)
	}{
		{"nil id", func(p *wallet.RehydrateParams) { p.ID = uuid.Nil }},
		{"nil player", func(p *wallet.RehydrateParams) { p.PlayerID = uuid.Nil }},
		{"uninitialized balance", func(p *wallet.RehydrateParams) { p.Balance = money.Money{} }},
		{"negative balance", func(p *wallet.RehydrateParams) { p.Balance = negative }},
		{"version zero", func(p *wallet.RehydrateParams) { p.Version = 0 }},
		{"missing created at", func(p *wallet.RehydrateParams) { p.CreatedAt = time.Time{} }},
		{"missing updated at", func(p *wallet.RehydrateParams) { p.UpdatedAt = time.Time{} }},
		{"updated before created", func(p *wallet.RehydrateParams) { p.UpdatedAt = t0.Add(-time.Second) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := valid()
			tc.mutate(&p)
			if _, err := wallet.Rehydrate(p); !errors.Is(err, wallet.ErrInvalidWallet) {
				t.Fatalf("error = %v, want ErrInvalidWallet", err)
			}
		})
	}
}

func TestUpdatedAtNeverMovesBackwards(t *testing.T) {
	t.Parallel()

	w := openWallet(t, "10.00")
	if _, err := w.Credit(movement(brl(t, "1.00"), t0.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if !w.UpdatedAt().Equal(t0) {
		t.Errorf("updated at = %v, want %v", w.UpdatedAt(), t0)
	}
}
