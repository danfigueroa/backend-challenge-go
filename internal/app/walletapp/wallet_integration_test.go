//go:build integration

package walletapp_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/apptest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var pg *pgtest.Instance

func TestMain(m *testing.M) {
	os.Exit(pgtest.RunMain(m, &pg))
}

func TestOpenWalletWithPositiveBalance(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	playerID := uuid.NewString()

	view, err := h.Wallets.OpenWallet(ctx, walletapp.OpenWalletCommand{
		Actor: apptest.Internal, Meta: app.Metadata{CorrelationID: "corr-open"}, PlayerID: playerID, Amount: "1000.00", Currency: "BRL",
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.PlayerID.String() != playerID || view.Balance.String() != "1000.00 BRL" || view.Version != 1 {
		t.Errorf("view = %+v", view)
	}

	if n := h.Count(t, "SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING' AND status = 'PROCESSED' AND origin = 'INTERNAL' AND provider_id IS NULL", view.ID); n != 1 {
		t.Errorf("opening transactions = %d", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT' AND amount_minor = 100000 AND wallet_version = 1", view.ID); n != 1 {
		t.Errorf("opening ledger entries = %d", n)
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE partition_key = $1 AND correlation_id = 'corr-open' AND event_type IN ('WagerTransactionProcessed', 'WalletBalanceChanged')", view.ID.String()); n != 2 {
		t.Errorf("opening events = %d", n)
	}
}

func TestOpenWalletWithZeroBalance(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)

	view := h.OpenWallet(t, "0.00")
	if !view.Balance.IsZero() || view.Version != 1 {
		t.Errorf("view = %+v", view)
	}
	for table, query := range map[string]string{
		"transactions": "SELECT count(*) FROM wager_transactions WHERE wallet_id = $1",
		"ledger":       "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1",
	} {
		if n := h.Count(t, query, view.ID); n != 0 {
			t.Errorf("%s = %d, want 0", table, n)
		}
	}
	if n := h.Count(t, "SELECT count(*) FROM outbox_events WHERE partition_key = $1", view.ID.String()); n != 0 {
		t.Errorf("events = %d, want 0", n)
	}
}

func TestOpenWalletConflictAndValidation(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	playerID := uuid.NewString()

	open := func(actor app.Actor, player, amount, currency string) error {
		_, err := h.Wallets.OpenWallet(ctx, walletapp.OpenWalletCommand{Actor: actor, PlayerID: player, Amount: amount, Currency: currency})
		return err
	}

	if err := open(apptest.Internal, playerID, "10.00", "BRL"); err != nil {
		t.Fatal(err)
	}
	if kind, ok := app.ConflictKindOf(open(apptest.Internal, playerID, "5.00", "BRL")); !ok || kind != app.ConflictWalletExists {
		t.Errorf("duplicate wallet conflict kind = %v", kind)
	}
	if err := open(apptest.Internal, playerID, "5.00", "USD"); err != nil {
		t.Errorf("same player with another currency must be allowed: %v", err)
	}

	for name, err := range map[string]error{
		"provider actor":  open(apptest.ProviderA, uuid.NewString(), "1.00", "BRL"),
		"broker actor":    open(apptest.Broker, uuid.NewString(), "1.00", "BRL"),
		"anonymous actor": open(app.Actor{}, uuid.NewString(), "1.00", "BRL"),
	} {
		if !errors.Is(err, app.ErrForbidden) {
			t.Errorf("%s: error = %v, want ErrForbidden", name, err)
		}
	}

	invalid := map[string]struct {
		player, amount, currency, code string
	}{
		"missing player":   {"", "1.00", "BRL", "MISSING_FIELD"},
		"uppercase player": {"0192F28F-5DC0-7D58-BDB2-814AD6A0F4A1", "1.00", "BRL", "INVALID_FIELD"},
		"negative amount":  {uuid.NewString(), "-1.00", "BRL", "INVALID_AMOUNT"},
		"float amount":     {uuid.NewString(), "1.5", "BRL", "INVALID_AMOUNT"},
		"scientific":       {uuid.NewString(), "1e3", "BRL", "INVALID_AMOUNT"},
		"unknown currency": {uuid.NewString(), "1.00", "XYZ", "INVALID_CURRENCY"},
		"missing currency": {uuid.NewString(), "1.00", "", "MISSING_FIELD"},
	}
	for name, tc := range invalid {
		err := open(apptest.Internal, tc.player, tc.amount, tc.currency)
		verr, ok := errors.AsType[*app.ValidationError](err)
		if !ok || verr.Code != tc.code {
			t.Errorf("%s: error = %v, want %s", name, err, tc.code)
		}
	}
	if n := h.Count(t, "SELECT count(*) FROM wallets"); n != 2 {
		t.Errorf("wallets persisted = %d, want 2", n)
	}
}

func TestGetWalletAndLedgerPagination(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")
	for i := range 7 {
		h.Process(t, apptest.Input(w, "BET", fmt.Sprintf("bet-%d", i), "1.00", ""))
	}

	got, err := h.Wallets.GetWallet(ctx, apptest.Internal, w.ID)
	if err != nil || got.Balance.Amount() != "93.00" || got.Version != 8 {
		t.Fatalf("wallet = %+v, %v", got, err)
	}
	if _, err := h.Wallets.GetWallet(ctx, apptest.ProviderA, w.ID); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provider read wallet: %v", err)
	}
	if _, err := h.Wallets.GetWallet(ctx, apptest.Internal, uuid.New()); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("missing wallet: %v", err)
	}

	var (
		versions []int64
		cursor   string
		pages    int
	)
	for {
		page, err := h.Wallets.ListLedger(ctx, walletapp.LedgerQuery{Actor: apptest.Internal, WalletID: w.ID, Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range page.Entries {
			versions = append(versions, e.WalletVersion())
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 3 || len(versions) != 8 {
		t.Fatalf("pages = %d, entries = %d", pages, len(versions))
	}
	for i, v := range versions {
		if v != int64(i+1) {
			t.Errorf("entry %d has version %d", i, v)
		}
	}

	exact, err := h.Wallets.ListLedger(ctx, walletapp.LedgerQuery{Actor: apptest.Internal, WalletID: w.ID, Limit: 8})
	if err != nil || len(exact.Entries) != 8 || exact.NextCursor != "" {
		t.Errorf("exact page = %d entries, cursor %q, %v", len(exact.Entries), exact.NextCursor, err)
	}

	other := h.OpenWallet(t, "1.00")
	first, err := h.Wallets.ListLedger(ctx, walletapp.LedgerQuery{Actor: apptest.Internal, WalletID: w.ID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]walletapp.LedgerQuery{
		"foreign cursor": {Actor: apptest.Internal, WalletID: other.ID, Cursor: first.NextCursor},
		"garbage cursor": {Actor: apptest.Internal, WalletID: w.ID, Cursor: "garbage!"},
		"limit too big":  {Actor: apptest.Internal, WalletID: w.ID, Limit: 1000},
		"negative limit": {Actor: apptest.Internal, WalletID: w.ID, Limit: -1},
	} {
		if _, err := h.Wallets.ListLedger(ctx, q); !errors.Is(err, app.ErrInvalidInput) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
	if _, err := h.Wallets.ListLedger(ctx, walletapp.LedgerQuery{Actor: apptest.Internal, WalletID: uuid.New()}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("missing wallet ledger: %v", err)
	}
}

func TestReconciliationMatchesSpecificationExample(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "1000.00")
	h.Process(t, apptest.Input(w, "BET", "transaction-123", "25.00", ""))

	report, err := h.Wallets.Reconcile(ctx, apptest.Internal, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.StoredBalance.Amount() != "975.00" || report.CalculatedBalance.Amount() != "975.00" ||
		report.Difference.Amount() != "0.00" || !report.Consistent || report.CheckedEntries != 2 {
		t.Errorf("report = %+v", report)
	}
	if len(h.Observer.Reconciled) != 1 || !h.Observer.Reconciled[0].Consistent {
		t.Errorf("observer = %+v", h.Observer.Reconciled)
	}
	if _, err := h.Wallets.Reconcile(ctx, apptest.ProviderA, w.ID); !errors.Is(err, app.ErrForbidden) {
		t.Errorf("provider reconciliation: %v", err)
	}
}

func TestReconciliationDetectsDivergenceWithoutChangingBalance(t *testing.T) {
	t.Parallel()
	h := apptest.New(t, pg)
	ctx := context.Background()
	w := h.OpenWallet(t, "100.00")
	h.Process(t, apptest.Input(w, "BET", "bet-1", "30.00", ""))

	conn, err := h.DB.OwnerPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "UPDATE wallets SET balance_minor = balance_minor + 500 WHERE id = $1", w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "RESET session_replication_role"); err != nil {
		t.Fatal(err)
	}
	conn.Release()

	report, err := h.Wallets.Reconcile(ctx, apptest.Internal, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Consistent || report.StoredBalance.Amount() != "75.00" || report.CalculatedBalance.Amount() != "70.00" || report.Difference.Amount() != "5.00" {
		t.Errorf("report = %+v", report)
	}
	if len(h.Observer.Reconciled) != 1 || h.Observer.Reconciled[0].Consistent || h.Observer.Reconciled[0].Difference.Amount() != "5.00" {
		t.Errorf("divergence not reported to observer: %+v", h.Observer.Reconciled)
	}

	after, err := h.Wallets.GetWallet(ctx, apptest.Internal, w.ID)
	if err != nil || after.Balance.Amount() != "75.00" {
		t.Errorf("reconciliation must not change the balance: %+v, %v", after, err)
	}
}
