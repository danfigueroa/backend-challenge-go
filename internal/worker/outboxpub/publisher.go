package outboxpub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
	"github.com/danfigueroa/backend-challenge-go/internal/worker"
)

const (
	Name                  = "outbox-publisher"
	AttributeEventType    = "eventType"
	AttributeEventID      = "eventId"
	AttributePartitionKey = "partitionKey"
	storeOperationTimeout = 5 * time.Second
)

type SNSAPI interface {
	Publish(ctx context.Context, in *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

type Store interface {
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]app.OutboxRecord, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID, owner string, at time.Time) (bool, error)
	ScheduleRetry(ctx context.Context, eventID uuid.UUID, owner string, next time.Time, reason string) (bool, error)
}

type Settings struct {
	Owner          string
	TopicARN       string
	PollInterval   time.Duration
	ErrorBackoff   time.Duration
	BatchSize      int
	Concurrency    int
	Lease          time.Duration
	PublishTimeout time.Duration
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
}

type Hooks struct {
	AfterPublish func(ctx context.Context, record app.OutboxRecord) error
}

type Publisher struct {
	client   SNSAPI
	store    Store
	clock    app.Clock
	settings Settings
	hooks    Hooks
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

func New(client SNSAPI, store Store, clock app.Clock, s Settings, hooks Hooks, m *metrics.Metrics, logger *slog.Logger) (*Publisher, error) {
	switch {
	case client == nil || store == nil || clock == nil || m == nil || logger == nil:
		return nil, errors.New("outboxpub: all dependencies are required")
	case s.Owner == "" || s.TopicARN == "":
		return nil, errors.New("outboxpub: owner and topic ARN are required")
	case s.BatchSize < 1 || s.Concurrency < 1 || s.PollInterval <= 0 || s.ErrorBackoff < s.PollInterval:
		return nil, errors.New("outboxpub: invalid batch size, concurrency, poll interval or error backoff")
	case s.PublishTimeout <= 0 || s.Lease <= s.PublishTimeout:
		return nil, errors.New("outboxpub: lease must be longer than the publish timeout")
	case s.RetryBaseDelay <= 0 || s.RetryMaxDelay < s.RetryBaseDelay:
		return nil, errors.New("outboxpub: invalid retry delays")
	}
	return &Publisher{
		client: client, store: store, clock: clock, settings: s, hooks: hooks, metrics: m,
		logger: logger.With(slog.String("worker", Name), slog.String("owner", s.Owner)),
	}, nil
}

func (p *Publisher) Name() string { return Name }

func (p *Publisher) Run(ctx context.Context) error {
	return worker.Loop(ctx, worker.LoopSettings{
		Interval:   p.settings.PollInterval,
		MaxBackoff: p.settings.ErrorBackoff,
		OnError: func(err error) {
			p.metrics.WorkerErrors.WithLabelValues(Name).Inc()
			p.logger.WarnContext(ctx, "outbox iteration failed", slog.Any("error", err))
		},
	}, p.PublishBatch)
}

func (p *Publisher) PublishBatch(ctx context.Context) (bool, error) {
	claimCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeOperationTimeout)
	records, err := p.store.Claim(claimCtx, p.settings.Owner, p.clock.Now(), p.settings.Lease, p.settings.BatchSize)
	cancel()
	if err != nil {
		return false, fmt.Errorf("claim outbox events: %w", err)
	}

	partitions := partitionByKey(records)
	queue := make(chan []app.OutboxRecord)
	var wg sync.WaitGroup
	for range min(p.settings.Concurrency, len(partitions)) {
		wg.Go(func() {
			for events := range queue {
				p.publishPartition(ctx, events)
			}
		})
	}
	for _, events := range partitions {
		queue <- events
	}
	close(queue)
	wg.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("publisher stopping: %w", ctxErr)
	}
	return len(records) == p.settings.BatchSize, nil
}

func partitionByKey(records []app.OutboxRecord) [][]app.OutboxRecord {
	index := make(map[string]int, len(records))
	var partitions [][]app.OutboxRecord
	for _, record := range records {
		i, ok := index[record.PartitionKey]
		if !ok {
			i = len(partitions)
			index[record.PartitionKey] = i
			partitions = append(partitions, nil)
		}
		partitions[i] = append(partitions[i], record)
	}
	return partitions
}

func (p *Publisher) publishPartition(ctx context.Context, events []app.OutboxRecord) {
	for i, record := range events {
		if ctx.Err() != nil {
			p.release(ctx, events[i:])
			return
		}
		retryAt, err := p.publish(ctx, record)
		if err != nil {
			p.postpone(ctx, record, events[i+1:], retryAt, err)
			return
		}
	}
}

func (p *Publisher) publish(parent context.Context, record app.OutboxRecord) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), p.settings.PublishTimeout)
	defer cancel()
	log := p.logger.With(slog.String("eventId", record.EventID.String()), slog.String("eventType", string(record.EventType)),
		slog.String("partitionKey", record.PartitionKey), slog.Int("attempt", record.Attempts))

	started := time.Now()
	_, err := p.client.Publish(ctx, &sns.PublishInput{
		TopicArn:               aws.String(p.settings.TopicARN),
		Message:                aws.String(string(record.Payload)),
		MessageGroupId:         aws.String(record.PartitionKey),
		MessageDeduplicationId: aws.String(record.EventID.String()),
		MessageAttributes: map[string]types.MessageAttributeValue{
			AttributeEventType:    {DataType: aws.String("String"), StringValue: aws.String(string(record.EventType))},
			AttributeEventID:      {DataType: aws.String("String"), StringValue: aws.String(record.EventID.String())},
			AttributePartitionKey: {DataType: aws.String("String"), StringValue: aws.String(record.PartitionKey)},
		},
	})
	p.metrics.OutboxPublishDuration.Observe(time.Since(started).Seconds())
	if err != nil {
		return p.scheduleRetry(ctx, log, record, err), err
	}

	if p.hooks.AfterPublish != nil {
		if err := p.hooks.AfterPublish(ctx, record); err != nil {
			log.WarnContext(ctx, "event published but not confirmed; the lease will expire and it will be republished with the same eventId", slog.Any("error", err))
			return time.Time{}, nil
		}
	}

	markCtx, cancelMark := context.WithTimeout(context.WithoutCancel(parent), storeOperationTimeout)
	defer cancelMark()
	marked, err := p.store.MarkPublished(markCtx, record.EventID, p.settings.Owner, p.clock.Now())
	switch {
	case err != nil:
		p.metrics.OutboxPublishedTotal.WithLabelValues(string(record.EventType), "unconfirmed").Inc()
		log.WarnContext(ctx, "event published but confirmation failed; it will be republished with the same eventId", slog.Any("error", err))
	case !marked:
		p.metrics.OutboxPublishedTotal.WithLabelValues(string(record.EventType), "lease_lost").Inc()
		log.WarnContext(ctx, "event published after the lease was taken by another publisher")
	default:
		p.metrics.OutboxPublishedTotal.WithLabelValues(string(record.EventType), "published").Inc()
		log.DebugContext(ctx, "event published")
	}
	return time.Time{}, nil
}

func (p *Publisher) scheduleRetry(ctx context.Context, log *slog.Logger, record app.OutboxRecord, cause error) time.Time {
	delay := p.RetryDelay(record.Attempts)
	retryAt := p.clock.Now().Add(delay)
	p.metrics.OutboxPublishedTotal.WithLabelValues(string(record.EventType), "failed").Inc()
	p.metrics.RetriesTotal.WithLabelValues("outbox_publish", "transient").Inc()
	log.WarnContext(ctx, "event publication failed; scheduling retry", slog.Duration("delay", delay), slog.Any("error", cause))

	retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeOperationTimeout)
	defer cancel()
	if _, err := p.store.ScheduleRetry(retryCtx, record.EventID, p.settings.Owner, retryAt, cause.Error()); err != nil {
		log.WarnContext(ctx, "could not schedule retry; the lease will expire and the event will be retried", slog.Any("error", err))
	}
	return retryAt
}

func (p *Publisher) postpone(parent context.Context, failed app.OutboxRecord, followers []app.OutboxRecord, retryAt time.Time, cause error) {
	if len(followers) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), storeOperationTimeout)
	defer cancel()
	reason := fmt.Sprintf("waiting for preceding event %s: %v", failed.EventID, cause)
	for _, record := range followers {
		p.metrics.OutboxPublishedTotal.WithLabelValues(string(record.EventType), "postponed").Inc()
		if _, err := p.store.ScheduleRetry(ctx, record.EventID, p.settings.Owner, retryAt, reason); err != nil {
			p.logger.WarnContext(ctx, "could not postpone event; its lease will expire", slog.String("eventId", record.EventID.String()), slog.Any("error", err))
		}
	}
	p.logger.InfoContext(ctx, "postponed events behind a failed publication to keep partition order",
		slog.String("partitionKey", failed.PartitionKey), slog.Int("count", len(followers)))
}

func (p *Publisher) RetryDelay(attempts int) time.Duration {
	delay := p.settings.RetryBaseDelay
	for i := 1; i < attempts && delay < p.settings.RetryMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, p.settings.RetryMaxDelay)
}

func (p *Publisher) release(parent context.Context, records []app.OutboxRecord) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), storeOperationTimeout)
	defer cancel()
	now := p.clock.Now()
	for _, record := range records {
		if _, err := p.store.ScheduleRetry(ctx, record.EventID, p.settings.Owner, now, "released during shutdown"); err != nil {
			p.logger.WarnContext(ctx, "could not release event during shutdown; its lease will expire", slog.Any("error", err))
		}
	}
	p.logger.InfoContext(ctx, "released unpublished events during shutdown", slog.Int("count", len(records)))
}
