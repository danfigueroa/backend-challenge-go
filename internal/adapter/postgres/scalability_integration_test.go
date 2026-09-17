//go:build integration

package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
)

func TestIntegrityTriggersReadLedgerByIndexedLookups(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := ctxT(t)

	const history = 300
	for i := range history {
		w := f.openWallet(t, "10.00")
		f.mustProcess(t, w, opInput{kind: "BET", externalID: fmt.Sprintf("history-%d", i), amount: "1.00"})
	}
	target := f.openWallet(t, "100.00")

	cfg, err := pgxpool.ParseConfig(f.db.AppDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	scoped := &fixture{
		db: f.db, txm: postgres.NewTxManager(pool, testConfig()),
		wallets: postgres.NewWalletRepository(pool), txs: postgres.NewTransactionRepository(pool),
		ledger: postgres.NewLedgerRepository(pool), inbox: postgres.NewInboxRepository(pool), outbox: postgres.NewOutboxRepository(pool),
	}

	f.db.AppPool.Reset()
	before := settledLedgerTuplesRead(t, f, -1)
	scoped.mustProcess(t, target, opInput{kind: "BET", externalID: "measured-bet", amount: "10.00"})
	scoped.mustProcess(t, target, opInput{kind: "REFUND", externalID: "measured-refund", amount: "10.00", reference: "measured-bet"})
	pool.Close()

	read := settledLedgerTuplesRead(t, f, before) - before
	if read >= history/10 {
		t.Fatalf("writing two transactions read %d ledger tuples with %d unrelated wallets in the table; integrity checks must not scan the ledger", read, history)
	}
}

func settledLedgerTuplesRead(t *testing.T, f *fixture, previous int) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := ledgerTuplesRead(t, f)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		current := ledgerTuplesRead(t, f)
		if current == last && current != previous {
			return current
		}
		last = current
	}
	t.Fatalf("ledger statistics did not settle (last %d, previous %d)", last, previous)
	return 0
}

func ledgerTuplesRead(t *testing.T, f *fixture) int {
	t.Helper()
	var read int
	err := f.db.OwnerPool.QueryRow(ctxT(t), `
		SELECT COALESCE(t.seq_tup_read, 0) + COALESCE(sum(i.idx_tup_read), 0)
		FROM pg_stat_user_tables t
		LEFT JOIN pg_stat_user_indexes i ON i.relid = t.relid
		WHERE t.relname = 'ledger_entries'
		GROUP BY t.seq_tup_read`).Scan(&read)
	if err != nil {
		t.Fatal(err)
	}
	return read
}
