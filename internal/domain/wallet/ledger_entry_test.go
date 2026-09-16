package wallet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

func validEntryParams(t *testing.T) wallet.LedgerEntryParams {
	t.Helper()
	return wallet.LedgerEntryParams{
		ID:            uuid.New(),
		WalletID:      uuid.New(),
		TransactionID: uuid.New(),
		Direction:     wallet.DirectionDebit,
		Amount:        brl(t, "25.00"),
		BalanceBefore: brl(t, "1000.00"),
		BalanceAfter:  brl(t, "975.00"),
		WalletVersion: 2,
		CreatedAt:     t0,
	}
}

func TestRehydrateLedgerEntry(t *testing.T) {
	t.Parallel()

	p := validEntryParams(t)
	e, err := wallet.RehydrateLedgerEntry(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID() != p.ID || e.WalletID() != p.WalletID || e.TransactionID() != p.TransactionID ||
		e.Direction() != wallet.DirectionDebit || !e.Amount().Equal(p.Amount) ||
		!e.BalanceBefore().Equal(p.BalanceBefore) || !e.BalanceAfter().Equal(p.BalanceAfter) ||
		e.WalletVersion() != 2 || !e.CreatedAt().Equal(t0) {
		t.Errorf("entry does not reflect params: %+v", e)
	}

	credit := validEntryParams(t)
	credit.Direction = wallet.DirectionCredit
	credit.BalanceAfter = brl(t, "1025.00")
	if _, err := wallet.RehydrateLedgerEntry(credit); err != nil {
		t.Errorf("valid credit rejected: %v", err)
	}
}

func TestLedgerEntryInvariants(t *testing.T) {
	t.Parallel()

	usd, err := money.Parse("975.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	negative, err := money.New(-2500, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*testing.T, *wallet.LedgerEntryParams)
	}{
		{"nil id", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.ID = uuid.Nil }},
		{"nil wallet", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.WalletID = uuid.Nil }},
		{"nil transaction", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.TransactionID = uuid.Nil }},
		{"unknown direction", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.Direction = "TRANSFER" }},
		{"empty direction", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.Direction = "" }},
		{"version zero", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.WalletVersion = 0 }},
		{"missing timestamp", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.CreatedAt = time.Time{} }},
		{"zero amount", func(t *testing.T, p *wallet.LedgerEntryParams) {
			p.Amount = brl(t, "0.00")
			p.BalanceAfter = p.BalanceBefore
		}},
		{"negative amount", func(t *testing.T, p *wallet.LedgerEntryParams) {
			p.Amount = negative
			p.BalanceAfter = brl(t, "1025.00")
		}},
		{"uninitialized amount", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.Amount = money.Money{} }},
		{"uninitialized balance", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.BalanceBefore = money.Money{} }},
		{"negative balance after", func(t *testing.T, p *wallet.LedgerEntryParams) {
			p.BalanceBefore = brl(t, "10.00")
			after, err := money.New(-1500, money.BRL)
			if err != nil {
				t.Fatal(err)
			}
			p.BalanceAfter = after
		}},
		{"debit arithmetic mismatch", func(t *testing.T, p *wallet.LedgerEntryParams) { p.BalanceAfter = brl(t, "975.01") }},
		{"credit arithmetic mismatch", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.Direction = wallet.DirectionCredit }},
		{"currency mismatch after", func(_ *testing.T, p *wallet.LedgerEntryParams) { p.BalanceAfter = usd }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := validEntryParams(t)
			tc.mutate(t, &p)
			e, err := wallet.RehydrateLedgerEntry(p)
			if !errors.Is(err, wallet.ErrInvalidLedgerEntry) {
				t.Fatalf("error = %v, want ErrInvalidLedgerEntry", err)
			}
			if e != (wallet.LedgerEntry{}) {
				t.Error("entry returned alongside error")
			}
		})
	}
}

func TestParseDirection(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"DEBIT", "CREDIT"} {
		d, err := wallet.ParseDirection(s)
		if err != nil || d.String() != s {
			t.Errorf("ParseDirection(%q) = %v, %v", s, d, err)
		}
	}
	for _, s := range []string{"", "debit", "Credit", "REFUND"} {
		if _, err := wallet.ParseDirection(s); !errors.Is(err, wallet.ErrInvalidLedgerEntry) {
			t.Errorf("ParseDirection(%q) error = %v", s, err)
		}
	}
}
