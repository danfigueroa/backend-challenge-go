package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrInvalidEvent = errors.New("event: invalid event")

type Type string

const (
	TypeWagerTransactionProcessed        Type = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         Type = "WagerTransactionRejected"
	TypeWagerTransactionPendingReference Type = "WagerTransactionPendingReference"
	TypeWalletBalanceChanged             Type = "WalletBalanceChanged"
)

type AggregateType string

const (
	AggregateWagerTransaction AggregateType = "WagerTransaction"
	AggregateWallet           AggregateType = "Wallet"
)

const TimestampLayout = "2006-01-02T15:04:05.000Z07:00"

type Timestamp time.Time

func (ts Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(ts).UTC().Format(TimestampLayout) + `"`), nil
}

func (ts *Timestamp) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("%w: timestamp: %w", ErrInvalidEvent, err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("%w: timestamp: %w", ErrInvalidEvent, err)
	}
	*ts = Timestamp(parsed.UTC())
	return nil
}

func (ts Timestamp) Time() time.Time { return time.Time(ts).UTC() }

type Metadata struct {
	EventID       uuid.UUID
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

func (m Metadata) validate() error {
	switch {
	case m.EventID == uuid.Nil:
		return fmt.Errorf("%w: event id is required", ErrInvalidEvent)
	case m.CorrelationID == "":
		return fmt.Errorf("%w: correlation id is required", ErrInvalidEvent)
	case m.OccurredAt.IsZero():
		return fmt.Errorf("%w: occurred at is required", ErrInvalidEvent)
	}
	return nil
}

type Event interface {
	ID() uuid.UUID
	Type() Type
	Version() int
	AggregateType() AggregateType
	AggregateID() uuid.UUID
	PartitionKey() string
	CorrelationID() string
	CausationID() string
	OccurredAt() time.Time
	MarshalJSON() ([]byte, error)
}

type Envelope[T any] struct {
	metadata      Metadata
	eventType     Type
	version       int
	aggregateType AggregateType
	aggregateID   uuid.UUID
	partitionKey  string
	data          T
}

func newEnvelope[T any](m Metadata, t Type, version int, aggregateType AggregateType, aggregateID uuid.UUID, partitionKey string, data T) (Envelope[T], error) {
	if err := m.validate(); err != nil {
		return Envelope[T]{}, err
	}
	if aggregateID == uuid.Nil || partitionKey == "" {
		return Envelope[T]{}, fmt.Errorf("%w: aggregate id and partition key are required", ErrInvalidEvent)
	}
	m.OccurredAt = m.OccurredAt.UTC()
	return Envelope[T]{
		metadata:      m,
		eventType:     t,
		version:       version,
		aggregateType: aggregateType,
		aggregateID:   aggregateID,
		partitionKey:  partitionKey,
		data:          data,
	}, nil
}

type wireEnvelope[T any] struct {
	EventID       uuid.UUID     `json:"eventId"`
	EventType     Type          `json:"eventType"`
	AggregateType AggregateType `json:"aggregateType"`
	AggregateID   uuid.UUID     `json:"aggregateId"`
	CorrelationID string        `json:"correlationId"`
	CausationID   string        `json:"causationId,omitempty"`
	OccurredAt    Timestamp     `json:"occurredAt"`
	Version       int           `json:"version"`
	Data          T             `json:"data"`
}

func (e Envelope[T]) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(wireEnvelope[T]{
		EventID:       e.metadata.EventID,
		EventType:     e.eventType,
		AggregateType: e.aggregateType,
		AggregateID:   e.aggregateID,
		CorrelationID: e.metadata.CorrelationID,
		CausationID:   e.metadata.CausationID,
		OccurredAt:    Timestamp(e.metadata.OccurredAt),
		Version:       e.version,
		Data:          e.data,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: marshal %s: %w", ErrInvalidEvent, e.eventType, err)
	}
	return data, nil
}

func (e Envelope[T]) ID() uuid.UUID                { return e.metadata.EventID }
func (e Envelope[T]) Type() Type                   { return e.eventType }
func (e Envelope[T]) Version() int                 { return e.version }
func (e Envelope[T]) AggregateType() AggregateType { return e.aggregateType }
func (e Envelope[T]) AggregateID() uuid.UUID       { return e.aggregateID }
func (e Envelope[T]) PartitionKey() string         { return e.partitionKey }
func (e Envelope[T]) CorrelationID() string        { return e.metadata.CorrelationID }
func (e Envelope[T]) CausationID() string          { return e.metadata.CausationID }
func (e Envelope[T]) OccurredAt() time.Time        { return e.metadata.OccurredAt }
func (e Envelope[T]) Data() T                      { return e.data }
