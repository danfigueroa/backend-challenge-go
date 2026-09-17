//go:build integration

package sqsconsumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/sync/errgroup"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/sqsconsumer"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/apptest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/lstest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
)

var (
	pg *pgtest.Instance
	ls *lstest.Instance
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	var g errgroup.Group
	g.Go(func() (err error) { pg, err = pgtest.Start(ctx); return err })
	g.Go(func() (err error) { ls, err = lstest.Start(ctx); return err })
	err := g.Wait()
	defer func() {
		stop, cancelStop := context.WithTimeout(context.Background(), time.Minute)
		defer cancelStop()
		if pg != nil {
			_ = pg.Terminate(stop)
		}
		if ls != nil {
			_ = ls.Terminate(stop)
		}
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

type rig struct {
	*apptest.Harness
	queue   lstest.QueuePair
	metrics *metrics.Metrics
}

func newRig(t *testing.T, visibility time.Duration, maxReceive int) *rig {
	t.Helper()
	return &rig{Harness: apptest.New(t, pg), queue: ls.CreateQueuePair(t, visibility, maxReceive), metrics: metrics.New()}
}

func (r *rig) start(t *testing.T, processor sqsconsumer.Processor, hooks sqsconsumer.Hooks) *worker.Runner {
	t.Helper()
	if processor == nil {
		processor = r.Wagering
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	consumer, err := sqsconsumer.New(ls.SQS, processor, sqsconsumer.Settings{
		ConsumerName: "wager-transactions-consumer", QueueURL: r.queue.URL, DeadLetterURL: r.queue.DeadLetterURL,
		Workers: 2, MaxMessages: 10, WaitTime: time.Second, ProcessingTimeout: 10 * time.Second,
		RetryBaseDelay: time.Second, RetryMaxDelay: time.Second, ErrorBackoff: 500 * time.Millisecond,
	}, hooks, r.metrics, logger)
	if err != nil {
		t.Fatal(err)
	}
	runner := worker.NewRunner(consumer, logger)
	if err := runner.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, runner) })
	return runner
}

func stop(t *testing.T, runner *worker.Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := runner.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("stop consumer: %v", err)
	}
}

func message(messageID string, w walletapp.WalletView, kind, externalID, amount string) string {
	body := map[string]any{
		"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"data": map[string]any{
			"providerId": "provider-a", "externalTransactionId": externalID, "idempotencyKey": "provider-a:" + externalID,
			"playerId": w.PlayerID.String(), "walletId": w.ID.String(), "roundId": "round-987", "gameId": "fortune-chimp",
			"kind": kind, "money": map[string]string{"amount": amount, "currency": "BRL"},
		},
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func eventually(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestProvisionedMessagingTopology(t *testing.T) {
	ctx := context.Background()
	attrs, err := ls.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(ls.QueueURL("wager-transactions.fifo")),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Attributes["FifoQueue"] != "true" || attrs.Attributes["VisibilityTimeout"] != "30" ||
		!strings.Contains(attrs.Attributes["RedrivePolicy"], "wager-transactions-dlq.fifo") ||
		!strings.Contains(attrs.Attributes["RedrivePolicy"], `"maxReceiveCount": "5"`) ||
		!strings.Contains(attrs.Attributes["Policy"], "wager-transactions-consumer") {
		t.Errorf("input queue attributes = %v", attrs.Attributes)
	}
	if _, err := ls.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(ls.QueueURL("wager-transactions-dlq.fifo"))}); err != nil {
		t.Errorf("dead-letter queue missing: %v", err)
	}
	subs, err := ls.SNS.ListSubscriptionsByTopic(ctx, &sns.ListSubscriptionsByTopicInput{
		TopicArn: aws.String("arn:aws:sns:us-east-1:000000000000:wallet-events.fifo"),
	})
	if err != nil || len(subs.Subscriptions) != 1 || !strings.HasSuffix(aws.ToString(subs.Subscriptions[0].Endpoint), "wallet-events-audit.fifo") {
		t.Errorf("events topic subscriptions = %+v, %v", subs, err)
	}
}

func TestConsumesMessageAndDeletesAfterCommit(t *testing.T) {
	r := newRig(t, 30*time.Second, 5)
	w := r.OpenWallet(t, "1000.00")
	r.start(t, nil, sqsconsumer.Hooks{})

	ls.Send(t, r.queue.URL, w.ID.String(), "msg-123", message("msg-123", w, "BET", "transaction-123", "25.00"), map[string]string{"correlationId": "corr-sqs"})

	eventually(t, 15*time.Second, "message processed", func() bool {
		return r.Count(t, "SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-123' AND processed_at IS NOT NULL") == 1
	})
	eventually(t, 10*time.Second, "message deleted", func() bool { return ls.ApproximateMessages(t, r.queue.URL) == 0 })

	if n := r.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 97500 {
		t.Errorf("balance = %d", n)
	}
	if n := r.Count(t, "SELECT count(*) FROM outbox_events WHERE correlation_id = 'corr-sqs' AND causation_id = 'msg-123'"); n != 2 {
		t.Errorf("events with SQS correlation = %d", n)
	}
	if v := testutil.ToFloat64(r.metrics.SQSMessagesTotal.WithLabelValues("wager-transactions-consumer", "PROCESSED")); v != 1 {
		t.Errorf("processed metric = %v", v)
	}
}

func TestRedeliveredMessageIsDeduplicated(t *testing.T) {
	r := newRig(t, 30*time.Second, 5)
	w := r.OpenWallet(t, "100.00")
	body := message("msg-dup", w, "BET", "bet-dup", "10.00")

	ls.Send(t, r.queue.URL, w.ID.String(), "delivery-1", body, nil)
	ls.Send(t, r.queue.URL, w.ID.String(), "delivery-2", body, nil)
	r.start(t, nil, sqsconsumer.Hooks{})

	eventually(t, 15*time.Second, "both deliveries handled", func() bool {
		return ls.ApproximateMessages(t, r.queue.URL) == 0 &&
			testutil.ToFloat64(r.metrics.SQSMessagesTotal.WithLabelValues("wager-transactions-consumer", "DUPLICATE_DELIVERY")) == 1
	})
	if n := r.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Errorf("debits = %d", n)
	}
	if n := r.Count(t, "SELECT count(*) FROM inbox_messages"); n != 1 {
		t.Errorf("inbox rows = %d", n)
	}
}

func TestSameOperationThroughHTTPAndSQS(t *testing.T) {
	r := newRig(t, 30*time.Second, 5)
	w := r.OpenWallet(t, "100.00")

	httpResult := r.Process(t, apptest.Input(w, "BET", "bet-both", "30.00", ""))
	r.start(t, nil, sqsconsumer.Hooks{})
	ls.Send(t, r.queue.URL, w.ID.String(), "msg-both", message("msg-both", w, "BET", "bet-both", "30.00"), nil)

	eventually(t, 15*time.Second, "replay through SQS", func() bool {
		return testutil.ToFloat64(r.metrics.SQSMessagesTotal.WithLabelValues("wager-transactions-consumer", "IDEMPOTENT_REPLAY")) == 1
	})
	var inboxTx uuid.UUID
	if err := r.DB.OwnerPool.QueryRow(context.Background(), "SELECT transaction_id FROM inbox_messages WHERE message_id = 'msg-both'").Scan(&inboxTx); err != nil {
		t.Fatal(err)
	}
	if inboxTx != httpResult.Transaction.ID() {
		t.Errorf("inbox references %s, want the HTTP transaction %s", inboxTx, httpResult.Transaction.ID())
	}
	if n := r.Count(t, "SELECT balance_minor FROM wallets WHERE id = $1", w.ID); n != 7000 {
		t.Errorf("balance = %d", n)
	}
}

func TestPermanentFailuresAreDeadLettered(t *testing.T) {
	r := newRig(t, 30*time.Second, 5)
	w := r.OpenWallet(t, "100.00")

	valid := message("msg-valid", w, "BET", "bet-valid", "10.00")
	cases := map[string]struct {
		body   string
		reason string
		code   string
	}{
		"malformed":        {`{not json`, sqsconsumer.ReasonMalformed, "MALFORMED_REQUEST"},
		"opening kind":     {message("msg-opening", w, "OPENING", "op", "10.00"), sqsconsumer.ReasonValidation, "OPENING_NOT_ALLOWED"},
		"unknown wallet":   {strings.Replace(message("msg-nowallet", w, "BET", "bet-nowallet", "10.00"), w.ID.String(), uuid.NewString(), 1), sqsconsumer.ReasonValidation, "WALLET_NOT_FOUND"},
		"message id reuse": {strings.Replace(valid, `"10.00"`, `"11.00"`, 1), sqsconsumer.ReasonInbox, sqsconsumer.ReasonInbox},
	}

	r.start(t, nil, sqsconsumer.Hooks{})
	ls.Send(t, r.queue.URL, w.ID.String(), "valid", valid, nil)
	eventually(t, 15*time.Second, "valid message processed", func() bool {
		return r.Count(t, "SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-valid'") == 1
	})
	i := 0
	for _, tc := range cases {
		i++
		ls.Send(t, r.queue.URL, fmt.Sprintf("group-%d", i), fmt.Sprintf("bad-%d", i), tc.body, nil)
	}

	dead := ls.Drain(t, r.queue.DeadLetterURL, len(cases), 30*time.Second)
	if len(dead) != len(cases) {
		t.Fatalf("dead-lettered %d messages, want %d", len(dead), len(cases))
	}
	got := map[string]string{}
	for _, m := range dead {
		got[aws.ToString(m.MessageAttributes[sqsconsumer.AttributeReason].StringValue)+"/"+aws.ToString(m.MessageAttributes[sqsconsumer.AttributeErrorCode].StringValue)] = aws.ToString(m.Body)
		if aws.ToString(m.MessageAttributes[sqsconsumer.AttributeSourceQueue].StringValue) != r.queue.URL {
			t.Errorf("dead letter without source queue: %+v", m.MessageAttributes)
		}
	}
	for name, tc := range cases {
		body, ok := got[tc.reason+"/"+tc.code]
		if !ok {
			t.Errorf("%s: no dead letter with %s/%s (got %v)", name, tc.reason, tc.code, got)
			continue
		}
		if body != tc.body {
			t.Errorf("%s: dead letter body changed", name)
		}
	}
	eventually(t, 10*time.Second, "source queue empty", func() bool { return ls.ApproximateMessages(t, r.queue.URL) == 0 })
	if n := r.Count(t, "SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'"); n != 1 {
		t.Errorf("transactions = %d, permanent failures must not persist", n)
	}
}

type flakyProcessor struct {
	calls atomic.Int32
}

func (f *flakyProcessor) Process(context.Context, wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
	f.calls.Add(1)
	return wageringapp.ProcessResult{}, fmt.Errorf("%w: database unavailable", app.ErrTransient)
}

func TestTransientFailuresAreRetriedThenRedriven(t *testing.T) {
	r := newRig(t, 30*time.Second, 3)
	w := r.OpenWallet(t, "100.00")
	processor := &flakyProcessor{}
	r.start(t, processor, sqsconsumer.Hooks{})

	ls.Send(t, r.queue.URL, w.ID.String(), "transient", message("msg-transient", w, "BET", "bet-transient", "10.00"), nil)

	dead := ls.Drain(t, r.queue.DeadLetterURL, 1, 45*time.Second)
	if len(dead) != 1 {
		t.Fatalf("message was not redriven to the DLQ after exhausting receives (calls=%d)", processor.calls.Load())
	}
	if _, explicit := dead[0].MessageAttributes[sqsconsumer.AttributeReason]; explicit {
		t.Error("transient failures must reach the DLQ through redrive, not explicit dead-lettering")
	}
	if calls := processor.calls.Load(); calls != 3 {
		t.Errorf("processing attempts = %d, want maxReceiveCount (3)", calls)
	}
	if v := testutil.ToFloat64(r.metrics.SQSMessagesTotal.WithLabelValues("wager-transactions-consumer", "RETRY")); v != 3 {
		t.Errorf("retry metric = %v", v)
	}
}

func TestCrashAfterCommitBeforeDeleteIsRedeliveredSafely(t *testing.T) {
	r := newRig(t, 2*time.Second, 5)
	w := r.OpenWallet(t, "100.00")

	var crashed atomic.Bool
	crashing := r.start(t, nil, sqsconsumer.Hooks{BeforeDelete: func(context.Context, string) error {
		crashed.Store(true)
		return errors.New("simulated crash between commit and delete")
	}})
	ls.Send(t, r.queue.URL, w.ID.String(), "crash", message("msg-crash", w, "BET", "bet-crash", "40.00"), nil)
	eventually(t, 15*time.Second, "first delivery committed", crashed.Load)
	stop(t, crashing)

	if n := r.Count(t, "SELECT count(*) FROM inbox_messages WHERE message_id = 'msg-crash' AND processed_at IS NOT NULL"); n != 1 {
		t.Fatalf("inbox not committed before the crash: %d", n)
	}
	if n := ls.ApproximateMessages(t, r.queue.URL); n != 1 {
		t.Fatalf("message should still be in the queue after the crash, got %d", n)
	}

	r.start(t, nil, sqsconsumer.Hooks{})
	eventually(t, 20*time.Second, "redelivery answered from inbox", func() bool {
		return testutil.ToFloat64(r.metrics.SQSMessagesTotal.WithLabelValues("wager-transactions-consumer", "DUPLICATE_DELIVERY")) == 1 &&
			ls.ApproximateMessages(t, r.queue.URL) == 0
	})
	if n := r.Count(t, "SELECT count(*) FROM ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'", w.ID); n != 1 {
		t.Errorf("debits after redelivery = %d", n)
	}
	r.AssertAllWalletsReconcile(t)
}

type slowProcessor struct {
	inner   sqsconsumer.Processor
	started chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (s *slowProcessor) Process(ctx context.Context, cmd wageringapp.ProcessCommand) (wageringapp.ProcessResult, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	time.Sleep(time.Second)
	return s.inner.Process(ctx, cmd)
}

func TestShutdownCompletesInFlightAndReleasesTheRest(t *testing.T) {
	r := newRig(t, 30*time.Second, 5)
	w := r.OpenWallet(t, "100.00")
	for i := range 3 {
		id := fmt.Sprintf("shutdown-%d", i)
		ls.Send(t, r.queue.URL, w.ID.String(), id, message("msg-"+id, w, "BET", "bet-"+id, "1.00"), nil)
	}

	processor := &slowProcessor{inner: r.Wagering, started: make(chan struct{})}
	runner := r.start(t, processor, sqsconsumer.Hooks{})
	<-processor.started
	stop(t, runner)

	if calls := processor.calls.Load(); calls != 1 {
		t.Fatalf("processed %d messages during shutdown, want only the in-flight one", calls)
	}
	if n := r.Count(t, "SELECT count(*) FROM inbox_messages WHERE processed_at IS NOT NULL"); n != 1 {
		t.Errorf("in-flight message was not completed: %d", n)
	}

	started := time.Now()
	released := ls.Drain(t, r.queue.URL, 2, 10*time.Second)
	if len(released) != 2 {
		t.Fatalf("released messages visible = %d, want 2", len(released))
	}
	if waited := time.Since(started); waited > 8*time.Second {
		t.Errorf("released messages took %v to become visible; visibility was not reset", waited)
	}
	if status := r.Count(t, "SELECT count(*) FROM wager_transactions WHERE status = 'PROCESSED' AND origin = 'EXTERNAL'"); status != 1 {
		t.Errorf("processed transactions = %d", status)
	}
}
