package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/logging"
)

const namespace = "wallet"

type Metrics struct {
	Registry *prometheus.Registry

	TransactionsTotal      *prometheus.CounterVec
	IdempotentReplaysTotal *prometheus.CounterVec
	IdempotencyConflicts   *prometheus.CounterVec
	ProcessingDuration     *prometheus.HistogramVec
	RetriesTotal           *prometheus.CounterVec
	ConcurrencyConflicts   *prometheus.CounterVec
	ReconciliationsTotal   *prometheus.CounterVec
	ReconciliationDiverged prometheus.Counter
	PendingCycles          *prometheus.CounterVec
	WorkerErrors           *prometheus.CounterVec
	SQSMessagesTotal       *prometheus.CounterVec
	SQSDeadLettered        *prometheus.CounterVec
	OutboxPublishedTotal   *prometheus.CounterVec
	OutboxPublishDuration  prometheus.Histogram
	HTTPRequestsTotal      *prometheus.CounterVec
	HTTPRequestDuration    *prometheus.HistogramVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		TransactionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "wagering_transactions_total",
			Help: "Completed wagering transactions by channel, kind, status and failure code (replays excluded).",
		}, []string{"channel", "kind", "status", "failure_code"}),
		IdempotentReplaysTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "wagering_idempotent_replays_total",
			Help: "Requests answered from a persisted result (duplicates).",
		}, []string{"channel", "kind", "status"}),
		IdempotencyConflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "wagering_idempotency_conflicts_total",
			Help: "Requests rejected because the idempotency key or external id was reused with different content.",
		}, []string{"channel", "code"}),
		ProcessingDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "wagering_processing_duration_seconds",
			Help:    "End-to-end processing latency of wagering transactions, including retries.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"channel", "kind", "replay"}),
		RetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "operation_retries_total",
			Help: "Retries of units of work after transient failures or races.",
		}, []string{"operation", "reason"}),
		ConcurrencyConflicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "concurrency_conflicts_total",
			Help: "Optimistic version conflicts and uniqueness races detected while writing.",
		}, []string{"operation"}),
		ReconciliationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "reconciliations_total",
			Help: "Wallet reconciliations by result.",
		}, []string{"result"}),
		ReconciliationDiverged: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "reconciliation_divergences_total",
			Help: "Reconciliations that found the stored balance diverging from the ledger.",
		}),
		PendingCycles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "pending_reference_resolutions_total",
			Help: "Outcomes of pending reference resolution attempts.",
		}, []string{"outcome"}),
		WorkerErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "worker_errors_total",
			Help: "Errors returned by background worker iterations.",
		}, []string{"worker"}),
		SQSMessagesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "sqs_messages_total",
			Help: "Messages handled by the SQS consumer by result.",
		}, []string{"queue", "result"}),
		SQSDeadLettered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "sqs_dead_lettered_total",
			Help: "Messages sent to the dead-letter queue by reason.",
		}, []string{"queue", "reason"}),
		OutboxPublishedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "outbox_publish_attempts_total",
			Help: "Outbox publication attempts by result.",
		}, []string{"event_type", "result"}),
		OutboxPublishDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "outbox_publish_duration_seconds",
			Help:    "Latency of publishing a single outbox event to the broker.",
			Buckets: prometheus.DefBuckets,
		}),
		HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "http_requests_total",
			Help: "HTTP requests by route, method and status code.",
		}, []string{"route", "method", "code"}),
		HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds",
			Help:    "HTTP request latency by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.TransactionsTotal, m.IdempotentReplaysTotal, m.IdempotencyConflicts, m.ProcessingDuration,
		m.RetriesTotal, m.ConcurrencyConflicts, m.ReconciliationsTotal, m.ReconciliationDiverged,
		m.PendingCycles, m.WorkerErrors, m.SQSMessagesTotal, m.SQSDeadLettered,
		m.OutboxPublishedTotal, m.OutboxPublishDuration, m.HTTPRequestsTotal, m.HTTPRequestDuration,
	)
	return m
}

func (m *Metrics) AddPendingDeferred(n int) {
	if n > 0 {
		m.PendingCycles.WithLabelValues("DEFERRED").Add(float64(n))
	}
}

type BacklogReader interface {
	Backlog(ctx context.Context) (app.OutboxBacklog, error)
}

type outboxCollector struct {
	reader  BacklogReader
	timeout time.Duration
	now     func() time.Time
	logger  *slog.Logger
	pending *prometheus.Desc
	lag     *prometheus.Desc
	up      *prometheus.Desc
}

func (m *Metrics) RegisterOutboxBacklog(reader BacklogReader, timeout time.Duration, logger *slog.Logger) error {
	c := &outboxCollector{
		reader: reader, timeout: timeout, now: time.Now, logger: logger,
		pending: prometheus.NewDesc(namespace+"_outbox_pending_events", "Outbox events not yet published.", nil, nil),
		lag:     prometheus.NewDesc(namespace+"_outbox_lag_seconds", "Age of the oldest unpublished outbox event.", nil, nil),
		up:      prometheus.NewDesc(namespace+"_outbox_backlog_scrape_success", "Whether reading the outbox backlog succeeded.", nil, nil),
	}
	if err := m.Registry.Register(c); err != nil {
		return fmt.Errorf("metrics: register outbox backlog collector: %w", err)
	}
	return nil
}

func (c *outboxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.pending
	ch <- c.lag
	ch <- c.up
}

func (c *outboxCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	backlog, err := c.reader.Backlog(ctx)
	if err != nil {
		c.logger.WarnContext(ctx, "outbox backlog scrape failed", slog.Any("error", err))
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	lag := 0.0
	if backlog.Pending > 0 && !backlog.OldestOccurredAt.IsZero() {
		lag = max(c.now().Sub(backlog.OldestOccurredAt).Seconds(), 0)
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(backlog.Pending))
	ch <- prometheus.MustNewConstMetric(c.lag, prometheus.GaugeValue, lag)
}

type Observer struct {
	metrics *Metrics
	logger  *slog.Logger
}

var _ app.Observer = (*Observer)(nil)

func NewObserver(m *Metrics, logger *slog.Logger) *Observer {
	return &Observer{metrics: m, logger: logger}
}

func (o *Observer) TransactionCompleted(ctx context.Context, obs app.TransactionObservation) {
	kind, status := string(obs.Kind), string(obs.Status)
	if obs.IdempotentReplay {
		o.metrics.IdempotentReplaysTotal.WithLabelValues(string(obs.Channel), kind, status).Inc()
	} else {
		o.metrics.TransactionsTotal.WithLabelValues(string(obs.Channel), kind, status, string(obs.FailureCode)).Inc()
	}
	if obs.Channel == app.ChannelWorker {
		o.metrics.PendingCycles.WithLabelValues(status).Inc()
	}
	if obs.Duration > 0 {
		o.metrics.ProcessingDuration.WithLabelValues(string(obs.Channel), kind, boolLabel(obs.IdempotentReplay)).Observe(obs.Duration.Seconds())
	}
	o.logger.InfoContext(ctx, "wagering transaction completed",
		slog.String("channel", string(obs.Channel)), slog.String("kind", kind), slog.String("status", status),
		slog.String("failureCode", string(obs.FailureCode)), slog.Bool("idempotentReplay", obs.IdempotentReplay),
		slog.Duration("duration", obs.Duration))
}

func (o *Observer) TransactionConflict(ctx context.Context, channel app.Channel, code wagering.FailureCode) {
	o.metrics.IdempotencyConflicts.WithLabelValues(string(channel), string(code)).Inc()
	o.logger.WarnContext(ctx, "idempotency conflict", slog.String("channel", string(channel)), slog.String("failureCode", string(code)))
}

func (o *Observer) OperationRetried(ctx context.Context, operation string, err error) {
	reason := "transient"
	if errors.Is(err, app.ErrConcurrentUpdate) || errors.Is(err, app.ErrConflict) {
		reason = "concurrency"
		o.metrics.ConcurrencyConflicts.WithLabelValues(operation).Inc()
	}
	o.metrics.RetriesTotal.WithLabelValues(operation, reason).Inc()
	o.logger.DebugContext(ctx, "retrying operation", slog.String("operation", operation), slog.String("reason", reason), slog.Any("error", err))
}

func (o *Observer) ReconciliationCompleted(ctx context.Context, obs app.ReconciliationObservation) {
	if obs.Consistent {
		o.metrics.ReconciliationsTotal.WithLabelValues("consistent").Inc()
		o.logger.InfoContext(ctx, "wallet reconciled", slog.String(logging.KeyWalletID, obs.WalletID.String()), slog.Int64("checkedEntries", obs.Entries))
		return
	}
	o.metrics.ReconciliationsTotal.WithLabelValues("divergent").Inc()
	o.metrics.ReconciliationDiverged.Inc()
	o.logger.ErrorContext(ctx, "wallet reconciliation divergence",
		slog.String(logging.KeyWalletID, obs.WalletID.String()),
		slog.String("difference", obs.Difference.String()),
		slog.Int64("checkedEntries", obs.Entries))
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
