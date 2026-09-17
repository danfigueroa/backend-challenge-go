//go:build integration

package outboxpub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/danfigueroa/backend-challenge-go/internal/adapter/postgres"
	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/apptest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/lstest"
	"github.com/danfigueroa/backend-challenge-go/internal/testsupport/pgtest"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
	"github.com/danfigueroa/backend-challenge-go/internal/worker/outboxpub"
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

type countingSNS struct {
	inner    outboxpub.SNSAPI
	mu       sync.Mutex
	calls    map[string]int
	failNext atomic.Int32
	delay    time.Duration
}

func (c *countingSNS) Publish(ctx context.Context, in *sns.PublishInput, opts ...func(*sns.Options)) (*sns.PublishOutput, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[aws.ToString(in.MessageDeduplicationId)]++
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.failNext.Load() > 0 {
		c.failNext.Add(-1)
		return nil, errors.New("sns: service unavailable")
	}
	out, err := c.inner.Publish(ctx, in, opts...)
	if err != nil {
		return nil, fmt.Errorf("publish: %w", err)
	}
	return out, nil
}

func (c *countingSNS) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.calls))
	for k, v := range c.calls {
		out[k] = v
	}
	return out
}

type setup struct {
	*apptest.Harness
	topic lstest.Topic
	store *postgres.OutboxRepository
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	h := apptest.New(t, pg)
	return &setup{Harness: h, topic: ls.CreateTopicWithAudit(t), store: postgres.NewOutboxRepository(h.Pool)}
}

func (s *setup) publisher(t *testing.T, owner string, client outboxpub.SNSAPI, lease time.Duration, hooks outboxpub.Hooks) *outboxpub.Publisher {
	t.Helper()
	p, err := outboxpub.New(client, s.store, s.Clock, outboxpub.Settings{
		Owner: owner, TopicARN: s.topic.ARN, PollInterval: 50 * time.Millisecond, ErrorBackoff: 200 * time.Millisecond,
		BatchSize: 5, Lease: lease, PublishTimeout: 500 * time.Millisecond, RetryBaseDelay: 10 * time.Second, RetryMaxDelay: time.Minute,
	}, hooks, metrics.New(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func drainOutbox(t *testing.T, p *outboxpub.Publisher) {
	t.Helper()
	for range 100 {
		more, err := p.PublishBatch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("outbox did not drain")
}

type auditedEvent struct {
	EventID   string `json:"eventId"`
	EventType string `json:"eventType"`
}

func audited(t *testing.T, s *setup, want int) map[string]auditedEvent {
	t.Helper()
	messages := ls.Drain(t, s.topic.AuditURL, want, 20*time.Second)
	extra := ls.Drain(t, s.topic.AuditURL, -1, 2*time.Second)
	events := map[string]auditedEvent{}
	for _, m := range append(messages, extra...) {
		var e auditedEvent
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &e); err != nil {
			t.Fatalf("audit message is not an event envelope: %s", aws.ToString(m.Body))
		}
		if _, dup := events[e.EventID]; dup {
			t.Errorf("event %s delivered more than once", e.EventID)
		}
		if aws.ToString(m.MessageAttributes[outboxpub.AttributeEventType].StringValue) != e.EventType {
			t.Errorf("eventType attribute mismatch for %s", e.EventID)
		}
		events[e.EventID] = e
	}
	return events
}

func TestPublishesCommittedEventsWithTheirContract(t *testing.T) {
	s := newSetup(t)
	w := s.OpenWallet(t, "100.00")
	s.Process(t, apptest.Input(w, "BET", "bet-1", "10.00", ""))
	s.Process(t, apptest.Input(w, "REFUND", "refund-early", "5.00", "missing-bet"))

	client := &countingSNS{inner: ls.SNS}
	drainOutbox(t, s.publisher(t, "publisher-a", client, 30*time.Second, outboxpub.Hooks{}))

	events := audited(t, s, 5)
	types := map[string]int{}
	for _, e := range events {
		types[e.EventType]++
	}
	if len(events) != 5 || types["WagerTransactionProcessed"] != 2 || types["WalletBalanceChanged"] != 2 || types["WagerTransactionPendingReference"] != 1 {
		t.Errorf("published event types = %v", types)
	}
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); n != 0 {
		t.Errorf("unpublished events = %d", n)
	}
	var stored string
	var id uuid.UUID
	if err := s.DB.OwnerPool.QueryRow(context.Background(), "SELECT id, payload::text FROM outbox_events ORDER BY occurred_at LIMIT 1").Scan(&id, &stored); err != nil {
		t.Fatal(err)
	}
	if _, ok := events[id.String()]; !ok {
		t.Errorf("event %s not delivered", id)
	}
}

func TestEventsSurviveCrashBetweenCommitAndPublication(t *testing.T) {
	s := newSetup(t)
	for range 3 {
		s.OpenWallet(t, "10.00")
	}
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); n != 6 {
		t.Fatalf("committed but unpublished events = %d", n)
	}

	restarted := apptest.Attach(t, s.DB)
	p, err := outboxpub.New(ls.SNS, postgres.NewOutboxRepository(restarted.Pool), restarted.Clock, outboxpub.Settings{
		Owner: "publisher-after-restart", TopicARN: s.topic.ARN, PollInterval: 50 * time.Millisecond, ErrorBackoff: time.Second,
		BatchSize: 50, Lease: 30 * time.Second, PublishTimeout: time.Second, RetryBaseDelay: time.Second, RetryMaxDelay: time.Minute,
	}, outboxpub.Hooks{}, metrics.New(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	drainOutbox(t, p)

	if events := audited(t, s, 6); len(events) != 6 {
		t.Errorf("delivered %d events after restart, want 6", len(events))
	}
}

func TestRecoveryBetweenPublicationAndConfirmationKeepsEventID(t *testing.T) {
	s := newSetup(t)
	s.OpenWallet(t, "10.00")

	client := &countingSNS{inner: ls.SNS}
	var crashes atomic.Int32
	crashing := s.publisher(t, "publisher-crashing", client, 2*time.Second, outboxpub.Hooks{
		AfterPublish: func(context.Context, app.OutboxRecord) error {
			crashes.Add(1)
			return errors.New("simulated crash before confirming publication")
		},
	})
	if _, err := crashing.PublishBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if crashes.Load() != 2 {
		t.Fatalf("crash hook calls = %d", crashes.Load())
	}
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND locked_by = 'publisher-crashing'"); n != 2 {
		t.Fatalf("events should remain leased by the crashed publisher, got %d", n)
	}

	survivor := s.publisher(t, "publisher-survivor", client, 2*time.Second, outboxpub.Hooks{})
	if more, err := survivor.PublishBatch(context.Background()); err != nil || more {
		t.Fatalf("leased events must not be taken before the lease expires: %v", err)
	}
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); n != 2 {
		t.Fatalf("events published by the survivor before lease expiry")
	}

	s.Clock.Advance(3 * time.Second)
	drainOutbox(t, survivor)

	calls := client.snapshot()
	if len(calls) != 2 {
		t.Fatalf("distinct event ids published = %d", len(calls))
	}
	for eventID, n := range calls {
		if n != 2 {
			t.Errorf("event %s published %d times, want 2 (original + recovery with the same eventId)", eventID, n)
		}
	}
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL AND attempts = 2"); n != 2 {
		t.Errorf("events confirmed after recovery = %d", n)
	}
	events := audited(t, s, 2)
	if len(events) != 2 {
		t.Errorf("subscribers received %d events, want 2: SNS FIFO deduplicates the republication by eventId", len(events))
	}
	for eventID := range calls {
		if _, ok := events[eventID]; !ok {
			t.Errorf("event %s not delivered", eventID)
		}
	}
}

func TestConcurrentPublishersPublishEachEventOnce(t *testing.T) {
	s := newSetup(t)
	const wallets = 20
	for range wallets {
		s.OpenWallet(t, "1.00")
	}

	client := &countingSNS{inner: ls.SNS}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	runners := make([]*worker.Runner, 0, 3)
	for i := range 3 {
		runner := worker.NewRunner(s.publisher(t, fmt.Sprintf("publisher-%d", i), client, 30*time.Second, outboxpub.Hooks{}), logger)
		if err := runner.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		runners = append(runners, runner)
	}

	deadline := time.Now().Add(30 * time.Second)
	for s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") > 0 {
		if time.Now().After(deadline) {
			t.Fatal("publishers did not drain the outbox")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, r := range runners {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
		cancel()
	}

	calls := client.snapshot()
	if len(calls) != wallets*2 {
		t.Errorf("distinct events published = %d, want %d", len(calls), wallets*2)
	}
	for eventID, n := range calls {
		if n != 1 {
			t.Errorf("event %s published %d times by concurrent publishers", eventID, n)
		}
	}
	owners := s.Count(t, "SELECT count(DISTINCT locked_by) FROM outbox_events")
	if owners != 0 {
		t.Errorf("published events still hold locks: %d owners", owners)
	}
	if events := audited(t, s, wallets*2); len(events) != wallets*2 {
		t.Errorf("subscribers received %d events", len(events))
	}
}

func TestFailedPublicationIsRetriedWithBackoff(t *testing.T) {
	s := newSetup(t)
	s.OpenWallet(t, "10.00")

	client := &countingSNS{inner: ls.SNS}
	client.failNext.Store(2)
	p := s.publisher(t, "publisher-retry", client, 30*time.Second, outboxpub.Hooks{})

	if _, err := p.PublishBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND locked_by IS NULL AND last_error LIKE '%service unavailable%' AND next_attempt_at > now() + interval '5 seconds'"); n != 2 {
		t.Fatalf("failed events were not rescheduled with backoff: %d", n)
	}
	if more, err := p.PublishBatch(context.Background()); err != nil || more {
		t.Fatalf("events retried before their backoff elapsed: %v", err)
	}

	s.Clock.Advance(11 * time.Second)
	drainOutbox(t, p)
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL AND attempts = 2 AND last_error IS NULL"); n != 2 {
		t.Errorf("events published on retry = %d", n)
	}
	if got := p.RetryDelay(1); got != 10*time.Second {
		t.Errorf("RetryDelay(1) = %v", got)
	}
	if got := p.RetryDelay(10); got != time.Minute {
		t.Errorf("RetryDelay(10) = %v", got)
	}
}

func TestShutdownReleasesClaimedEvents(t *testing.T) {
	s := newSetup(t)
	for range 3 {
		s.OpenWallet(t, "1.00")
	}
	client := &countingSNS{inner: ls.SNS, delay: 300 * time.Millisecond}
	p := s.publisher(t, "publisher-stopping", client, 30*time.Second, outboxpub.Hooks{})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := p.PublishBatch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stopping batch = %v, want cancellation", err)
	}

	published := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NOT NULL")
	released := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND locked_by IS NULL AND last_error = 'released during shutdown'")
	if published != 1 || released != 4 {
		t.Errorf("published = %d, released = %d; want the in-flight event finished and the rest released", published, released)
	}

	other := s.publisher(t, "publisher-other", ls.SNS, 30*time.Second, outboxpub.Hooks{})
	drainOutbox(t, other)
	if n := s.Count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL"); n != 0 {
		t.Errorf("released events were not taken over immediately: %d left", n)
	}
}
