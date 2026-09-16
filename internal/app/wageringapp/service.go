package wageringapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/event"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

type Deps struct {
	Tx           app.TxManager
	Wallets      app.WalletRepository
	Transactions app.TransactionRepository
	Ledger       app.LedgerRepository
	Inbox        app.InboxRepository
	Outbox       app.OutboxRepository
	Clock        app.Clock
	IDs          app.IDGenerator
	Observer     app.Observer
	Retry        app.RetryPolicy
	Pending      wagering.PendingPolicy
	ClaimLease   time.Duration
}

type Service struct {
	tx           app.TxManager
	wallets      app.WalletRepository
	transactions app.TransactionRepository
	ledger       app.LedgerRepository
	inbox        app.InboxRepository
	outbox       app.OutboxRepository
	clock        app.Clock
	ids          app.IDGenerator
	observer     app.Observer
	retry        app.RetryPolicy
	pending      wagering.PendingPolicy
	claimLease   time.Duration
}

func NewService(d Deps) (*Service, error) {
	if d.Tx == nil || d.Wallets == nil || d.Transactions == nil || d.Ledger == nil || d.Inbox == nil ||
		d.Outbox == nil || d.Clock == nil || d.IDs == nil || d.Observer == nil {
		return nil, errors.New("wageringapp: all dependencies are required")
	}
	if err := d.Retry.Validate(); err != nil {
		return nil, err
	}
	if err := d.Pending.Validate(); err != nil {
		return nil, fmt.Errorf("wageringapp: %w", err)
	}
	if d.ClaimLease <= 0 {
		return nil, errors.New("wageringapp: claim lease must be positive")
	}
	return &Service{
		tx: d.Tx, wallets: d.Wallets, transactions: d.Transactions, ledger: d.Ledger, inbox: d.Inbox, outbox: d.Outbox,
		clock: d.Clock, ids: d.IDs, observer: d.Observer, retry: d.Retry, pending: d.Pending, claimLease: d.ClaimLease,
	}, nil
}

type IdempotencyConflictError struct {
	Code                  wagering.FailureCode
	ExistingTransactionID uuid.UUID
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("wageringapp: %s with transaction %s", e.Code, e.ExistingTransactionID)
}

func (e *IdempotencyConflictError) Is(target error) bool { return target == app.ErrConflict }

func (s *Service) eventMetadata(meta app.Metadata, at time.Time) event.Metadata {
	return event.Metadata{EventID: s.ids.NewID(), CorrelationID: meta.CorrelationID, CausationID: meta.CausationID, OccurredAt: at}
}

func (s *Service) eventsFor(meta app.Metadata, t *wagering.Transaction, d wagering.Decision, firstAwait bool, at time.Time) ([]event.Event, error) {
	var events []event.Event
	switch d.Outcome {
	case wagering.OutcomeProcessed:
		processed, err := event.NewWagerTransactionProcessed(s.eventMetadata(meta, at), t)
		if err != nil {
			return nil, err
		}
		events = append(events, processed)
		if d.Entry != nil {
			changed, err := event.NewWalletBalanceChanged(s.eventMetadata(meta, at), *d.Entry)
			if err != nil {
				return nil, err
			}
			events = append(events, changed)
		}
	case wagering.OutcomeRejected:
		rejected, err := event.NewWagerTransactionRejected(s.eventMetadata(meta, at), t)
		if err != nil {
			return nil, err
		}
		events = append(events, rejected)
	case wagering.OutcomeAwaitingReference:
		if !firstAwait {
			return nil, nil
		}
		pending, err := event.NewWagerTransactionPendingReference(s.eventMetadata(meta, at), t, d.FailureCode)
		if err != nil {
			return nil, err
		}
		events = append(events, pending)
	}
	return events, nil
}

func (s *Service) resolveReference(ctx context.Context, t *wagering.Transaction) (*wagering.Reference, error) {
	if t.ReferenceExternalTransactionID() == "" {
		return nil, nil
	}
	ref, err := s.transactions.GetByExternalIDForUpdate(ctx, t.ProviderID(), t.ReferenceExternalTransactionID())
	if errors.Is(err, app.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	reversed, err := s.transactions.HasProcessedReversal(ctx, ref.ID())
	if err != nil {
		return nil, err
	}
	return &wagering.Reference{Transaction: ref, AlreadyReversed: reversed}, nil
}

func (s *Service) persistDecision(ctx context.Context, meta app.Metadata, t *wagering.Transaction, d wagering.Decision, firstAwait bool, at time.Time) error {
	if d.Entry != nil {
		if err := s.ledger.Insert(ctx, *d.Entry); err != nil {
			return err
		}
	}
	events, err := s.eventsFor(meta, t, d, firstAwait, at)
	if err != nil {
		return fmt.Errorf("build events for %s: %w", t.ID(), err)
	}
	if err := s.outbox.Append(ctx, at, events...); err != nil {
		return err
	}
	if d.Outcome == wagering.OutcomeProcessed {
		if _, err := s.transactions.WakeWaitingOn(ctx, t.ProviderID(), t.ExternalTransactionID(), at); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) retried(ctx context.Context, operation string) func(int, error) {
	return func(_ int, err error) { s.observer.OperationRetried(ctx, operation, err) }
}
