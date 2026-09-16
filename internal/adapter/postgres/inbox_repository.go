package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
)

type InboxRepository struct {
	conn
}

var _ app.InboxRepository = (*InboxRepository)(nil)

func NewInboxRepository(pool *pgxpool.Pool) *InboxRepository {
	return &InboxRepository{conn{pool: pool}}
}

func (r *InboxRepository) InsertIfAbsent(ctx context.Context, msg app.InboxMessage) (*app.InboxMessage, error) {
	q := r.q(ctx)
	var inserted bool
	err := q.QueryRow(ctx,
		`INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, transaction_id, received_at, processed_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (consumer_name, message_id) DO NOTHING
		 RETURNING true`,
		msg.ConsumerName, msg.MessageID, msg.PayloadHash[:], nullUUID(msg.TransactionID), msg.ReceivedAt, nullTime(msg.ProcessedAt)).
		Scan(&inserted)
	switch {
	case err == nil:
		return nil, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, translate(err)
	}

	var (
		existing    app.InboxMessage
		hash        []byte
		txID        *uuid.UUID
		processedAt *time.Time
	)
	err = q.QueryRow(ctx,
		`SELECT consumer_name, message_id, payload_hash, transaction_id, received_at, processed_at
		 FROM inbox_messages WHERE consumer_name = $1 AND message_id = $2`,
		msg.ConsumerName, msg.MessageID).
		Scan(&existing.ConsumerName, &existing.MessageID, &hash, &txID, &existing.ReceivedAt, &processedAt)
	if err != nil {
		return nil, translate(err)
	}
	if len(hash) != len(existing.PayloadHash) {
		return nil, fmt.Errorf("%w: inbox message %s has malformed hash", app.ErrIntegrityViolation, msg.MessageID)
	}
	copy(existing.PayloadHash[:], hash)
	existing.TransactionID = valueOr(txID)
	existing.ProcessedAt = valueOr(processedAt)
	return &existing, nil
}

func (r *InboxRepository) MarkProcessed(ctx context.Context, consumerName, messageID string, transactionID uuid.UUID, at time.Time) error {
	tag, err := r.q(ctx).Exec(ctx,
		`UPDATE inbox_messages SET transaction_id = $3, processed_at = $4
		 WHERE consumer_name = $1 AND message_id = $2 AND processed_at IS NULL`,
		consumerName, messageID, nullUUID(transactionID), at)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: inbox message %s/%s is missing or already processed", app.ErrConcurrentUpdate, consumerName, messageID)
	}
	return nil
}
