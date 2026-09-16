package metrics_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestObserverRecordsTransactionOutcomes(t *testing.T) {
	t.Parallel()
	m := metrics.New()
	o := metrics.NewObserver(m, quietLogger())
	ctx := context.Background()

	o.TransactionCompleted(ctx, app.TransactionObservation{Channel: app.ChannelHTTP, Kind: wagering.KindBet, Status: wagering.StatusProcessed, Duration: 5 * time.Millisecond})
	o.TransactionCompleted(ctx, app.TransactionObservation{Channel: app.ChannelHTTP, Kind: wagering.KindBet, Status: wagering.StatusRejected, FailureCode: wagering.CodeInsufficientFunds, Duration: time.Millisecond})
	o.TransactionCompleted(ctx, app.TransactionObservation{Channel: app.ChannelSQS, Kind: wagering.KindBet, Status: wagering.StatusProcessed, IdempotentReplay: true, Duration: time.Millisecond})
	o.TransactionCompleted(ctx, app.TransactionObservation{Channel: app.ChannelWorker, Kind: wagering.KindRefund, Status: wagering.StatusProcessed})

	if v := testutil.ToFloat64(m.TransactionsTotal.WithLabelValues("HTTP", "BET", "PROCESSED", "")); v != 1 {
		t.Errorf("processed = %v", v)
	}
	if v := testutil.ToFloat64(m.TransactionsTotal.WithLabelValues("HTTP", "BET", "REJECTED", "INSUFFICIENT_FUNDS")); v != 1 {
		t.Errorf("rejected = %v", v)
	}
	if v := testutil.ToFloat64(m.IdempotentReplaysTotal.WithLabelValues("SQS", "BET", "PROCESSED")); v != 1 {
		t.Errorf("replays = %v", v)
	}
	if v := testutil.ToFloat64(m.TransactionsTotal.WithLabelValues("SQS", "BET", "PROCESSED", "")); v != 0 {
		t.Errorf("replays must not count as new transactions: %v", v)
	}
	if v := testutil.ToFloat64(m.PendingCycles.WithLabelValues("PROCESSED")); v != 1 {
		t.Errorf("pending resolutions = %v", v)
	}
	if n := testutil.CollectAndCount(m.ProcessingDuration); n != 2 {
		t.Errorf("latency series = %d", n)
	}
}

func TestObserverRecordsRetriesAndConflicts(t *testing.T) {
	t.Parallel()
	m := metrics.New()
	o := metrics.NewObserver(m, quietLogger())
	ctx := context.Background()

	o.OperationRetried(ctx, "process_transaction", app.ErrTransient)
	o.OperationRetried(ctx, "process_transaction", app.ErrConcurrentUpdate)
	o.OperationRetried(ctx, "process_transaction", &app.ConflictError{Kind: app.ConflictIdempotencyKey})
	o.TransactionConflict(ctx, app.ChannelHTTP, wagering.CodeIdempotencyKeyConflict)

	if v := testutil.ToFloat64(m.RetriesTotal.WithLabelValues("process_transaction", "transient")); v != 1 {
		t.Errorf("transient retries = %v", v)
	}
	if v := testutil.ToFloat64(m.RetriesTotal.WithLabelValues("process_transaction", "concurrency")); v != 2 {
		t.Errorf("concurrency retries = %v", v)
	}
	if v := testutil.ToFloat64(m.ConcurrencyConflicts.WithLabelValues("process_transaction")); v != 2 {
		t.Errorf("concurrency conflicts = %v", v)
	}
	if v := testutil.ToFloat64(m.IdempotencyConflicts.WithLabelValues("HTTP", "IDEMPOTENCY_KEY_CONFLICT")); v != 1 {
		t.Errorf("idempotency conflicts = %v", v)
	}
}

func TestObserverReportsReconciliationDivergence(t *testing.T) {
	t.Parallel()
	m := metrics.New()
	var logs bytes.Buffer
	o := metrics.NewObserver(m, slog.New(slog.NewJSONHandler(&logs, nil)))
	walletID := uuid.New()
	diff, err := money.New(500, money.BRL)
	if err != nil {
		t.Fatal(err)
	}

	o.ReconciliationCompleted(context.Background(), app.ReconciliationObservation{WalletID: walletID, Consistent: true, Entries: 2})
	o.ReconciliationCompleted(context.Background(), app.ReconciliationObservation{WalletID: walletID, Consistent: false, Difference: diff, Entries: 2})

	if v := testutil.ToFloat64(m.ReconciliationDiverged); v != 1 {
		t.Errorf("divergences = %v", v)
	}
	if v := testutil.ToFloat64(m.ReconciliationsTotal.WithLabelValues("consistent")); v != 1 {
		t.Errorf("consistent = %v", v)
	}
	if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), walletID.String()) || !strings.Contains(logs.String(), "5.00 BRL") {
		t.Errorf("divergence not logged: %s", logs.String())
	}
}

type backlogStub struct {
	backlog app.OutboxBacklog
	err     error
}

func (b backlogStub) Backlog(context.Context) (app.OutboxBacklog, error) { return b.backlog, b.err }

func TestOutboxBacklogCollector(t *testing.T) {
	t.Parallel()

	m := metrics.New()
	oldest := time.Now().Add(-90 * time.Second)
	if err := m.RegisterOutboxBacklog(backlogStub{backlog: app.OutboxBacklog{Pending: 7, OldestOccurredAt: oldest}}, time.Second, quietLogger()); err != nil {
		t.Fatal(err)
	}
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, f := range families {
		if strings.HasPrefix(f.GetName(), "wallet_outbox_") {
			values[f.GetName()] = f.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if values["wallet_outbox_pending_events"] != 7 || values["wallet_outbox_backlog_scrape_success"] != 1 {
		t.Errorf("gauges = %v", values)
	}
	if lag := values["wallet_outbox_lag_seconds"]; lag < 89 || lag > 120 {
		t.Errorf("lag = %v, want about 90s", lag)
	}

	failing := metrics.New()
	if err := failing.RegisterOutboxBacklog(backlogStub{err: errors.New("db down")}, time.Second, quietLogger()); err != nil {
		t.Fatal(err)
	}
	families, err = failing.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "wallet_outbox_backlog_scrape_success" && f.GetMetric()[0].GetGauge().GetValue() != 0 {
			t.Error("scrape failure not reported")
		}
		if f.GetName() == "wallet_outbox_pending_events" {
			t.Error("stale backlog value exported after failure")
		}
	}
}
