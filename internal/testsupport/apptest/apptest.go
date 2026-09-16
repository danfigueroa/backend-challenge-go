//go:build integration

package apptest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
)

var (
	Internal  = app.ServiceActor("wallet-internal")
	ProviderA = app.ProviderActor("provider-a-client", "provider-a")
	ProviderB = app.ProviderActor("provider-b-client", "provider-b")
	Broker    = app.BrokerActor("wager-transactions-consumer")

	Pending = wagering.PendingPolicy{TTL: 30 * time.Minute, BaseDelay: time.Second, MaxDelay: time.Minute}
	Retry   = app.RetryPolicy{Attempts: 8, BaseDelay: 5 * time.Millisecond, MaxDelay: 100 * time.Millisecond}
)

type Clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.offset).Truncate(time.Microsecond)
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

type Observer struct {
	mu         sync.Mutex
	Completed  []app.TransactionObservation
	Conflicts  []wagering.FailureCode
	Retries    map[string]int
	Reconciled []app.ReconciliationObservation
}

func (o *Observer) TransactionCompleted(_ context.Context, obs app.TransactionObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Completed = append(o.Completed, obs)
}

func (o *Observer) TransactionConflict(_ context.Context, _ app.Channel, code wagering.FailureCode) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Conflicts = append(o.Conflicts, code)
}

func (o *Observer) OperationRetried(_ context.Context, operation string, _ error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.Retries == nil {
		o.Retries = map[string]int{}
	}
	o.Retries[operation]++
}

func (o *Observer) ReconciliationCompleted(_ context.Context, obs app.ReconciliationObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Reconciled = append(o.Reconciled, obs)
}

func (o *Observer) Replays() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, c := range o.Completed {
		if c.IdempotentReplay {
			n++
		}
	}
	return n
}

type Harness struct {
	DB       *pgtest.Database
	Pool     *pgxpool.Pool
	Clock    *Clock
	Observer *Observer
	Wallets  *walletapp.Service
	Wagering *wageringapp.Service
}

func New(t *testing.T, pg *pgtest.Instance) *Harness {
	t.Helper()
	return Attach(t, pg.NewDatabase(t))
}

func Attach(t *testing.T, db *pgtest.Database) *Harness {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cfg := postgres.Config{DSN: db.AppDSN, MaxConns: 16, LockTimeout: 5 * time.Second, StatementTimeout: 10 * time.Second}
	pool, err := postgres.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	clock := &Clock{}
	observer := &Observer{}
	txm := postgres.NewTxManager(pool, cfg)
	wallets := postgres.NewWalletRepository(pool)
	transactions := postgres.NewTransactionRepository(pool)
	ledger := postgres.NewLedgerRepository(pool)
	inbox := postgres.NewInboxRepository(pool)
	outbox := postgres.NewOutboxRepository(pool)
	ids := app.UUIDv7Generator{}

	walletSvc, err := walletapp.NewService(walletapp.Deps{
		Tx: txm, Wallets: wallets, Transactions: transactions, Ledger: ledger, Outbox: outbox,
		Clock: clock, IDs: ids, Observer: observer, Retry: Retry,
	})
	if err != nil {
		t.Fatal(err)
	}
	wageringSvc, err := wageringapp.NewService(wageringapp.Deps{
		Tx: txm, Wallets: wallets, Transactions: transactions, Ledger: ledger, Inbox: inbox, Outbox: outbox,
		Clock: clock, IDs: ids, Observer: observer, Retry: Retry, Pending: Pending, ClaimLease: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Harness{DB: db, Pool: pool, Clock: clock, Observer: observer, Wallets: walletSvc, Wagering: wageringSvc}
}

func (h *Harness) OpenWallet(t *testing.T, amount string) walletapp.WalletView {
	t.Helper()
	view, err := h.Wallets.OpenWallet(context.Background(), walletapp.OpenWalletCommand{
		Actor: Internal, PlayerID: newPlayerID(), Amount: amount, Currency: "BRL",
	})
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	return view
}

func Input(w walletapp.WalletView, kind, externalID, amount, reference string) wagering.RequestInput {
	return wagering.RequestInput{
		ProviderID: "provider-a", ExternalTransactionID: externalID, IdempotencyKey: "provider-a:" + externalID,
		PlayerID: w.PlayerID.String(), WalletID: w.ID.String(), RoundID: "round-987", GameID: "fortune-chimp",
		Kind: kind, Amount: amount, Currency: "BRL", ReferenceExternalTransactionID: reference,
	}
}

func (h *Harness) Process(t *testing.T, in wagering.RequestInput) wageringapp.ProcessResult {
	t.Helper()
	res, err := h.Wagering.Process(context.Background(), wageringapp.ProcessCommand{
		Actor: ProviderA, Meta: app.Metadata{Channel: app.ChannelHTTP}, Input: in,
	})
	if err != nil {
		t.Fatalf("process %s %s: %v", in.Kind, in.ExternalTransactionID, err)
	}
	return res
}

func (h *Harness) Count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.DB.OwnerPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (h *Harness) AssertAllWalletsReconcile(t *testing.T) {
	t.Helper()
	rows, err := h.DB.OwnerPool.Query(context.Background(), "SELECT id FROM wallets")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		var stored, calculated int64
		err := h.DB.OwnerPool.QueryRow(context.Background(),
			`SELECT w.balance_minor,
			        COALESCE(sum(CASE WHEN l.direction = 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0)::BIGINT
			 FROM wallets w LEFT JOIN ledger_entries l ON l.wallet_id = w.id
			 WHERE w.id = $1 GROUP BY w.balance_minor`, id).Scan(&stored, &calculated)
		if err != nil {
			t.Fatal(err)
		}
		if stored != calculated {
			t.Errorf("wallet %s stored %d but ledger sums to %d", id, stored, calculated)
		}
	}
}

func newPlayerID() string {
	return uuid.New().String()
}
