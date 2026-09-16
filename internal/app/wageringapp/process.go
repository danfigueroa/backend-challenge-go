package wageringapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

type Delivery struct {
	ConsumerName string
	MessageID    string
	PayloadHash  [32]byte
	ReceivedAt   time.Time
}

type ProcessCommand struct {
	Actor    app.Actor
	Meta     app.Metadata
	Input    wagering.RequestInput
	Delivery *Delivery
}

type ProcessResult struct {
	Transaction       *wagering.Transaction
	IdempotentReplay  bool
	DuplicateDelivery bool
}

func (s *Service) Process(ctx context.Context, cmd ProcessCommand) (ProcessResult, error) {
	started := time.Now()
	meta := cmd.Meta.WithDefaults(s.ids)

	if !cmd.Actor.CanActForProvider(cmd.Input.ProviderID) {
		return ProcessResult{}, fmt.Errorf("%w: %s %q cannot act for provider %q", app.ErrForbidden, cmd.Actor.Kind, cmd.Actor.Subject, cmd.Input.ProviderID)
	}
	req, err := wagering.NewRequest(cmd.Input)
	if err != nil {
		return ProcessResult{}, err
	}

	var result ProcessResult
	err = app.Retry(ctx, s.retry, s.retried(ctx, "process_transaction"), func(ctx context.Context) error {
		var err error
		result, err = s.processOnce(ctx, meta, req, cmd.Delivery)
		return err
	})
	if conflict, ok := errors.AsType[*IdempotencyConflictError](err); ok {
		s.observer.TransactionConflict(ctx, meta.Channel, conflict.Code)
	}
	if err != nil {
		return ProcessResult{}, err
	}

	s.observer.TransactionCompleted(ctx, app.TransactionObservation{
		Channel: meta.Channel, Kind: result.Transaction.Kind(), Status: result.Transaction.Status(),
		FailureCode: result.Transaction.FailureCode(), IdempotentReplay: result.IdempotentReplay, Duration: time.Since(started),
	})
	return result, nil
}

func (s *Service) processOnce(ctx context.Context, meta app.Metadata, req wagering.Request, delivery *Delivery) (ProcessResult, error) {
	if delivery == nil {
		if result, found, err := s.replay(ctx, req); found || err != nil {
			return result, err
		}
	}

	var result ProcessResult
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.clock.Now()

		if delivery != nil {
			duplicate, err := s.registerDelivery(ctx, delivery)
			if err != nil {
				return err
			}
			if duplicate {
				replayed, found, err := s.replay(ctx, req)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("%w: inbox message %s has no transaction", app.ErrIntegrityViolation, delivery.MessageID)
				}
				replayed.DuplicateDelivery = true
				result = replayed
				return nil
			}
		}

		w, err := s.wallets.GetForUpdate(ctx, req.WalletID())
		if errors.Is(err, app.ErrNotFound) {
			return wagering.NewValidationError(wagering.CodeWalletNotFound, "walletId", "wallet does not exist")
		}
		if err != nil {
			return err
		}

		replayed, found, err := s.replay(ctx, req)
		if err != nil {
			return err
		}
		if found {
			result = replayed
			return s.completeDelivery(ctx, delivery, replayed.Transaction, now)
		}

		t, err := wagering.NewExternal(s.ids.NewID(), req, now)
		if err != nil {
			return err
		}
		if err := wagering.CheckWallet(t, w); err != nil {
			return err
		}
		ref, err := s.resolveReference(ctx, t)
		if err != nil {
			return err
		}
		decision, err := wagering.Process(wagering.ProcessParams{
			Transaction: t, Wallet: w, Reference: ref, EntryID: s.ids.NewID(), Now: now, Policy: s.pending,
		})
		if err != nil {
			return err
		}

		if err := s.transactions.Insert(ctx, t); err != nil {
			return err
		}
		if err := s.wallets.Update(ctx, w); err != nil {
			return err
		}
		if err := s.persistDecision(ctx, meta, t, decision, true, now); err != nil {
			return err
		}
		if err := s.completeDelivery(ctx, delivery, t, now); err != nil {
			return err
		}
		result = ProcessResult{Transaction: t}
		return nil
	})
	if err != nil {
		return ProcessResult{}, err
	}
	return result, nil
}

func (s *Service) replay(ctx context.Context, req wagering.Request) (ProcessResult, bool, error) {
	existing, err := s.transactions.GetByIdempotencyKey(ctx, req.ProviderID(), req.IdempotencyKey())
	if errors.Is(err, app.ErrNotFound) {
		existing, err = s.transactions.GetByExternalID(ctx, req.ProviderID(), req.ExternalTransactionID())
	}
	switch {
	case errors.Is(err, app.ErrNotFound):
		return ProcessResult{}, false, nil
	case err != nil:
		return ProcessResult{}, false, err
	case existing.IdempotencyKey() != req.IdempotencyKey():
		return ProcessResult{}, false, &IdempotencyConflictError{Code: wagering.CodeExternalTransactionConflict, ExistingTransactionID: existing.ID()}
	case !existing.MatchesPayload(req.PayloadHash()):
		return ProcessResult{}, false, &IdempotencyConflictError{Code: wagering.CodeIdempotencyKeyConflict, ExistingTransactionID: existing.ID()}
	}
	return ProcessResult{Transaction: existing, IdempotentReplay: true}, true, nil
}

func (s *Service) registerDelivery(ctx context.Context, d *Delivery) (bool, error) {
	existing, err := s.inbox.InsertIfAbsent(ctx, app.InboxMessage{
		ConsumerName: d.ConsumerName, MessageID: d.MessageID, PayloadHash: d.PayloadHash, ReceivedAt: d.ReceivedAt,
	})
	if err != nil {
		return false, err
	}
	if existing == nil {
		return false, nil
	}
	if existing.PayloadHash != d.PayloadHash {
		return false, fmt.Errorf("%w: consumer %s message %s", app.ErrInboxMismatch, d.ConsumerName, d.MessageID)
	}
	return true, nil
}

func (s *Service) completeDelivery(ctx context.Context, d *Delivery, t *wagering.Transaction, at time.Time) error {
	if d == nil {
		return nil
	}
	processedAt := at
	if processedAt.Before(d.ReceivedAt) {
		processedAt = d.ReceivedAt
	}
	return s.inbox.MarkProcessed(ctx, d.ConsumerName, d.MessageID, t.ID(), processedAt)
}
