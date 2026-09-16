package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
)

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

type IDGenerator interface {
	NewID() uuid.UUID
}

type UUIDv7Generator struct{}

func (UUIDv7Generator) NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

type Channel string

const (
	ChannelHTTP     Channel = "HTTP"
	ChannelSQS      Channel = "SQS"
	ChannelWorker   Channel = "WORKER"
	ChannelInternal Channel = "INTERNAL"
)

type Metadata struct {
	Channel       Channel
	CorrelationID string
	CausationID   string
}

func (m Metadata) WithDefaults(ids IDGenerator) Metadata {
	if m.CorrelationID == "" {
		m.CorrelationID = ids.NewID().String()
	}
	if m.Channel == "" {
		m.Channel = ChannelInternal
	}
	return m
}

type RetryPolicy struct {
	Attempts  int
	BaseDelay time.Duration
	MaxDelay  time.Duration
}

func (p RetryPolicy) Validate() error {
	if p.Attempts < 1 || p.BaseDelay <= 0 || p.MaxDelay < p.BaseDelay {
		return fmt.Errorf("app: retry policy requires attempts >= 1 and 0 < base delay <= max delay, got %+v", p)
	}
	return nil
}

func IsRetryable(err error) bool {
	if errors.Is(err, ErrTransient) || errors.Is(err, ErrConcurrentUpdate) {
		return true
	}
	kind, ok := ConflictKindOf(err)
	if !ok {
		return false
	}
	switch kind {
	case ConflictIdempotencyKey, ConflictExternalTransaction, ConflictReferenceReversed, ConflictInboxMessage, ConflictLedgerEntry:
		return true
	case ConflictUnknown, ConflictWalletExists, ConflictOpeningExists, ConflictDuplicateID:
		return false
	}
	return false
}

func Retry(ctx context.Context, p RetryPolicy, observe func(attempt int, err error), fn func(ctx context.Context) error) error {
	delay := p.BaseDelay
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(ctx); err == nil || !IsRetryable(err) || attempt >= p.Attempts {
			return err
		}
		if observe != nil {
			observe(attempt, err)
		}
		wait := delay + rand.N(delay/2+1)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, p.MaxDelay)
	}
}
