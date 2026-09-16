package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/event"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
	WithinSnapshot(ctx context.Context, fn func(ctx context.Context) error) error
}

type WalletRepository interface {
	Insert(ctx context.Context, w *wallet.Wallet) error
	Get(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	GetForUpdate(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	Update(ctx context.Context, w *wallet.Wallet) error
}

type TransactionRepository interface {
	Insert(ctx context.Context, tx *wagering.Transaction) error
	Update(ctx context.Context, tx *wagering.Transaction) error
	Get(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error)
	GetForUpdate(ctx context.Context, id uuid.UUID) (*wagering.Transaction, error)
	GetByIdempotencyKey(ctx context.Context, providerID, idempotencyKey string) (*wagering.Transaction, error)
	GetByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.Transaction, error)
	GetByExternalIDForUpdate(ctx context.Context, providerID, externalTransactionID string) (*wagering.Transaction, error)
	HasProcessedReversal(ctx context.Context, referenceTransactionID uuid.UUID) (bool, error)
	WakeWaitingOn(ctx context.Context, providerID, externalTransactionID string, now time.Time) (int64, error)
	ClaimDuePending(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]PendingClaim, error)
}

type PendingClaim struct {
	TransactionID uuid.UUID
	WalletID      uuid.UUID
}

type LedgerRepository interface {
	Insert(ctx context.Context, entry wallet.LedgerEntry) error
	List(ctx context.Context, walletID uuid.UUID, afterVersion int64, limit int) ([]wallet.LedgerEntry, error)
	Summarize(ctx context.Context, walletID uuid.UUID) (LedgerSummary, error)
}

type LedgerSummary struct {
	Entries          int64
	NetMinor         int64
	LastVersion      int64
	LastBalanceMinor int64
	ChainBreaks      int64
}

type InboxMessage struct {
	ConsumerName  string
	MessageID     string
	PayloadHash   [32]byte
	TransactionID uuid.UUID
	ReceivedAt    time.Time
	ProcessedAt   time.Time
}

type InboxRepository interface {
	InsertIfAbsent(ctx context.Context, msg InboxMessage) (existing *InboxMessage, err error)
	MarkProcessed(ctx context.Context, consumerName, messageID string, transactionID uuid.UUID, at time.Time) error
}

type OutboxRecord struct {
	EventID      uuid.UUID
	EventType    event.Type
	PartitionKey string
	Payload      []byte
	Attempts     int
	OccurredAt   time.Time
}

type OutboxBacklog struct {
	Pending          int64
	OldestOccurredAt time.Time
}

type OutboxRepository interface {
	Append(ctx context.Context, now time.Time, events ...event.Event) error
	Claim(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]OutboxRecord, error)
	MarkPublished(ctx context.Context, eventID uuid.UUID, owner string, at time.Time) (bool, error)
	ScheduleRetry(ctx context.Context, eventID uuid.UUID, owner string, next time.Time, reason string) (bool, error)
	Backlog(ctx context.Context) (OutboxBacklog, error)
}
