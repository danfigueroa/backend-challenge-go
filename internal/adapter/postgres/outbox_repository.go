package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/event"
)

const maxErrorLength = 1000

type OutboxRepository struct {
	conn
}

var _ app.OutboxRepository = (*OutboxRepository)(nil)

func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{conn{pool: pool}}
}

func (r *OutboxRepository) Append(ctx context.Context, now time.Time, events ...event.Event) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("outbox: serialize %s %s: %w", e.Type(), e.ID(), err)
		}
		batch.Queue(
			`INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, event_version, partition_key,
				correlation_id, causation_id, payload, occurred_at, next_attempt_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			e.ID(), string(e.AggregateType()), e.AggregateID(), string(e.Type()), e.Version(), e.PartitionKey(),
			e.CorrelationID(), nullString(e.CausationID()), string(payload), e.OccurredAt(), now)
	}
	return translate(r.q(ctx).SendBatch(ctx, batch).Close())
}

func (r *OutboxRepository) Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]app.OutboxRecord, error) {
	rows, err := r.q(ctx).Query(ctx,
		`WITH candidates AS (
			SELECT id FROM outbox_events
			WHERE published_at IS NULL
			  AND next_attempt_at <= $2
			  AND (locked_until IS NULL OR locked_until < $2)
			ORDER BY occurred_at, id
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		 )
		 UPDATE outbox_events o
		 SET locked_by = $1, locked_until = $3, attempts = o.attempts + 1
		 FROM candidates
		 WHERE o.id = candidates.id
		 RETURNING o.id, o.event_type, o.partition_key, o.payload::text, o.attempts, o.occurred_at`,
		owner, now, now.Add(lease), limit)
	if err != nil {
		return nil, translate(err)
	}
	records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.OutboxRecord, error) {
		var (
			rec       app.OutboxRecord
			eventType string
			payload   string
		)
		if err := row.Scan(&rec.EventID, &eventType, &rec.PartitionKey, &payload, &rec.Attempts, &rec.OccurredAt); err != nil {
			return app.OutboxRecord{}, fmt.Errorf("scan outbox record: %w", err)
		}
		rec.EventType = event.Type(eventType)
		rec.Payload = []byte(payload)
		return rec, nil
	})
	if err != nil {
		return nil, translate(err)
	}
	slices.SortFunc(records, func(a, b app.OutboxRecord) int {
		if c := a.OccurredAt.Compare(b.OccurredAt); c != 0 {
			return c
		}
		return slices.Compare(a.EventID[:], b.EventID[:])
	})
	return records, nil
}

func (r *OutboxRepository) MarkPublished(ctx context.Context, eventID uuid.UUID, owner string, at time.Time) (bool, error) {
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE outbox_events
		 SET published_at = $3, locked_by = NULL, locked_until = NULL, last_error = NULL
		 WHERE id = $1 AND locked_by = $2 AND published_at IS NULL`,
		eventID, owner, at)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *OutboxRepository) ScheduleRetry(ctx context.Context, eventID uuid.UUID, owner string, next time.Time, reason string) (bool, error) {
	if len(reason) > maxErrorLength {
		reason = reason[:maxErrorLength]
	}
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE outbox_events
		 SET next_attempt_at = $3, locked_by = NULL, locked_until = NULL, last_error = $4
		 WHERE id = $1 AND locked_by = $2 AND published_at IS NULL`,
		eventID, owner, next, reason)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *OutboxRepository) Backlog(ctx context.Context) (app.OutboxBacklog, error) {
	var (
		b      app.OutboxBacklog
		oldest *time.Time
	)
	err := r.q(ctx).QueryRow(ctx,
		`SELECT count(*), min(occurred_at) FROM outbox_events WHERE published_at IS NULL`).
		Scan(&b.Pending, &oldest)
	if err != nil {
		return app.OutboxBacklog{}, translate(err)
	}
	b.OldestOccurredAt = valueOr(oldest)
	return b, nil
}
