package walletapp

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()

	walletID := uuid.New()
	cursor := encodeCursor(walletID, 42)
	if strings.ContainsAny(cursor, "+/=") {
		t.Errorf("cursor must be URL safe: %q", cursor)
	}
	after, err := decodeCursor(cursor, walletID)
	if err != nil || after != 42 {
		t.Errorf("decode = %d, %v", after, err)
	}
	if after, err := decodeCursor("", walletID); err != nil || after != 0 {
		t.Errorf("empty cursor = %d, %v", after, err)
	}
}

func TestCursorRejectsTampering(t *testing.T) {
	t.Parallel()

	walletID := uuid.New()
	for name, cursor := range map[string]string{
		"not base64":       "%%%",
		"not json":         "bm90LWpzb24",
		"other wallet":     encodeCursor(uuid.New(), 3),
		"negative version": encodeCursor(walletID, -1),
		"zero version":     encodeCursor(walletID, 0),
		"unknown format":   "eyJ2Ijo5LCJ3IjoieCIsImEiOjF9",
	} {
		if _, err := decodeCursor(cursor, walletID); !errors.Is(err, app.ErrInvalidInput) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestBuildReport(t *testing.T) {
	t.Parallel()

	brl := func(minor int64) money.Money {
		m, err := money.New(minor, money.BRL)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	now := time.Now()
	id := uuid.New()

	tests := []struct {
		name       string
		stored     int64
		version    int64
		summary    app.LedgerSummary
		consistent bool
		difference string
	}{
		{"opening and bet", 97500, 2, app.LedgerSummary{Entries: 2, NetMinor: 97500, LastVersion: 2, LastBalanceMinor: 97500}, true, "0.00"},
		{"zero wallet", 0, 1, app.LedgerSummary{}, true, "0.00"},
		{"stored above ledger", 98000, 2, app.LedgerSummary{Entries: 2, NetMinor: 97500, LastVersion: 2, LastBalanceMinor: 97500}, false, "5.00"},
		{"stored below ledger", 97000, 2, app.LedgerSummary{Entries: 2, NetMinor: 97500, LastVersion: 2, LastBalanceMinor: 97500}, false, "-5.00"},
		{"chain break", 97500, 2, app.LedgerSummary{Entries: 2, NetMinor: 97500, LastVersion: 2, LastBalanceMinor: 97500, ChainBreaks: 1}, false, "0.00"},
		{"version ahead of ledger", 97500, 3, app.LedgerSummary{Entries: 2, NetMinor: 97500, LastVersion: 2, LastBalanceMinor: 97500}, false, "0.00"},
		{"balance without entries", 100, 1, app.LedgerSummary{}, false, "1.00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := buildReport(id, brl(tc.stored), tc.version, tc.summary, now)
			if err != nil {
				t.Fatal(err)
			}
			if r.Consistent != tc.consistent || r.Difference.Amount() != tc.difference || r.CheckedEntries != tc.summary.Entries {
				t.Errorf("report = consistent %v difference %s entries %d", r.Consistent, r.Difference.Amount(), r.CheckedEntries)
			}
			if !r.Difference.Equal(mustSub(t, r.StoredBalance, r.CalculatedBalance)) {
				t.Error("difference must be stored minus calculated")
			}
		})
	}
}

func mustSub(t *testing.T, a, b money.Money) money.Money {
	t.Helper()
	d, err := a.Sub(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
