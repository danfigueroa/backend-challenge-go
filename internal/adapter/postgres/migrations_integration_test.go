//go:build integration

package postgres_test

import (
	"testing"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
)

var managedTables = []string{"wallets", "wager_transactions", "ledger_entries", "inbox_messages", "outbox_events"}

func TestMigrationsApplyRevertAndReapply(t *testing.T) {
	t.Parallel()

	db := pg.NewEmptyDatabase(t)
	ctx := ctxT(t)

	migrator, err := postgres.NewMigrator(db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrator.Close() })

	assertTables := func(want bool) {
		t.Helper()
		for _, table := range managedTables {
			var exists bool
			if err := db.OwnerPool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "public."+table).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != want {
				t.Errorf("table %s exists = %v, want %v", table, exists, want)
			}
		}
	}

	status, err := migrator.Status()
	if err != nil || !status.Empty {
		t.Fatalf("initial status = %+v, %v", status, err)
	}
	assertTables(false)

	if err := migrator.Up(); err != nil {
		t.Fatal(err)
	}
	status, err = migrator.Status()
	if err != nil || status.Version != 7 || status.Dirty {
		t.Fatalf("status after up = %+v, %v", status, err)
	}
	assertTables(true)

	if err := migrator.Up(); err != nil {
		t.Fatalf("second up must be a no-op: %v", err)
	}

	if err := migrator.Down(1); err != nil {
		t.Fatal(err)
	}
	status, err = migrator.Status()
	if err != nil || status.Version != 6 {
		t.Fatalf("status after down 1 = %+v, %v", status, err)
	}

	if err := migrator.Down(6); err != nil {
		t.Fatal(err)
	}
	status, err = migrator.Status()
	if err != nil || !status.Empty {
		t.Fatalf("status after full down = %+v, %v", status, err)
	}
	assertTables(false)

	if err := migrator.Up(); err != nil {
		t.Fatalf("reapply: %v", err)
	}
	assertTables(true)

	if err := migrator.Down(0); err == nil {
		t.Error("Down(0) must be rejected")
	}
}

func TestMigrationsGrantLeastPrivilegeToApplication(t *testing.T) {
	t.Parallel()

	db := pg.NewDatabase(t)
	ctx := ctxT(t)

	tests := []struct {
		table, privilege string
		want             bool
	}{
		{"wallets", "SELECT", true},
		{"wallets", "INSERT", true},
		{"wallets", "DELETE", false},
		{"wallets", "TRUNCATE", false},
		{"wager_transactions", "INSERT", true},
		{"wager_transactions", "DELETE", false},
		{"ledger_entries", "SELECT", true},
		{"ledger_entries", "INSERT", true},
		{"ledger_entries", "UPDATE", false},
		{"ledger_entries", "DELETE", false},
		{"ledger_entries", "TRUNCATE", false},
		{"inbox_messages", "DELETE", false},
		{"outbox_events", "DELETE", true},
	}
	for _, tc := range tests {
		var got bool
		if err := db.OwnerPool.QueryRow(ctx, "SELECT has_table_privilege($1, $2, $3)", "wallet_service", tc.table, tc.privilege).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("wallet_service %s on %s = %v, want %v", tc.privilege, tc.table, got, tc.want)
		}
	}

	columns := []struct {
		table, column string
		want          bool
	}{
		{"wallets", "balance_minor", true},
		{"wallets", "player_id", false},
		{"wallets", "currency", false},
		{"wager_transactions", "status", true},
		{"wager_transactions", "amount_minor", false},
		{"wager_transactions", "payload_hash", false},
		{"outbox_events", "published_at", true},
		{"outbox_events", "payload", false},
		{"inbox_messages", "payload_hash", false},
	}
	for _, tc := range columns {
		var got bool
		if err := db.OwnerPool.QueryRow(ctx, "SELECT has_column_privilege($1, $2, $3, 'UPDATE')", "wallet_service", tc.table, tc.column).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("wallet_service UPDATE %s.%s = %v, want %v", tc.table, tc.column, got, tc.want)
		}
	}
}
