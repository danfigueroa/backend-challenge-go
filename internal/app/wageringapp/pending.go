package wageringapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

type ResolveStats struct {
	Claimed      int
	Processed    int
	Rejected     int
	StillPending int
	Failed       int
	Deferred     int
}

func (s *Service) ResolveDuePending(ctx context.Context, limit int) (ResolveStats, error) {
	var claims []app.PendingClaim
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		claims, err = s.transactions.ClaimDuePending(ctx, s.clock.Now(), s.claimLease, limit)
		return err
	})
	if err != nil {
		return ResolveStats{}, err
	}

	stats := ResolveStats{Claimed: len(claims)}
	for i, claim := range claims {
		if err := ctx.Err(); err != nil {
			stats.Deferred += len(claims) - i
			return stats, fmt.Errorf("resolve pending interrupted: %w", err)
		}
		outcome, err := s.resumeWithRetry(ctx, claim)
		switch {
		case err == nil:
			stats.count(outcome)
		case app.IsRetryable(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			stats.Deferred++
		default:
			if failErr := s.markFailed(ctx, claim, err); failErr != nil {
				stats.Deferred++
				continue
			}
			stats.Failed++
		}
	}
	return stats, nil
}

func (st *ResolveStats) count(o wagering.Status) {
	switch o {
	case wagering.StatusProcessed:
		st.Processed++
	case wagering.StatusRejected:
		st.Rejected++
	case wagering.StatusFailed:
		st.Failed++
	case wagering.StatusPending, wagering.StatusPendingReference:
		st.StillPending++
	}
}

func (s *Service) resumeWithRetry(ctx context.Context, claim app.PendingClaim) (wagering.Status, error) {
	var status wagering.Status
	err := app.Retry(ctx, s.retry, s.retried(ctx, "resume_pending"), func(ctx context.Context) error {
		var err error
		status, err = s.resume(ctx, claim)
		return err
	})
	return status, err
}

func (s *Service) resume(ctx context.Context, claim app.PendingClaim) (wagering.Status, error) {
	var (
		status   wagering.Status
		resumed  *wagering.Transaction
		finished bool
	)
	meta := app.Metadata{Channel: app.ChannelWorker, CausationID: claim.TransactionID.String()}.WithDefaults(s.ids)

	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		now := s.clock.Now()
		w, err := s.wallets.GetForUpdate(ctx, claim.WalletID)
		if err != nil {
			return err
		}
		t, err := s.transactions.GetForUpdate(ctx, claim.TransactionID)
		if err != nil {
			return err
		}
		if t.Status().IsTerminal() {
			status = t.Status()
			return nil
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
		if err := s.transactions.Update(ctx, t); err != nil {
			return err
		}
		if err := s.wallets.Update(ctx, w); err != nil {
			return err
		}
		if err := s.persistDecision(ctx, meta, t, decision, false, now); err != nil {
			return err
		}
		status, resumed, finished = t.Status(), t, t.Status().IsTerminal()
		return nil
	})
	if err != nil {
		return "", err
	}
	if finished {
		s.observer.TransactionCompleted(ctx, app.TransactionObservation{
			Channel: app.ChannelWorker, Kind: resumed.Kind(), Status: resumed.Status(), FailureCode: resumed.FailureCode(),
		})
	}
	return status, nil
}

func (s *Service) markFailed(ctx context.Context, claim app.PendingClaim, cause error) error {
	var failed *wagering.Transaction
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		t, err := s.transactions.GetForUpdate(ctx, claim.TransactionID)
		if err != nil {
			return err
		}
		if t.Status().IsTerminal() {
			return nil
		}
		if err := t.Fail(wagering.CodeInternalProcessingFailed, s.clock.Now()); err != nil {
			return fmt.Errorf("mark %s failed after %w: %w", t.ID(), cause, err)
		}
		if err := s.transactions.Update(ctx, t); err != nil {
			return err
		}
		failed = t
		return s.wakeWaitingOn(ctx, t, s.clock.Now())
	})
	if err != nil {
		return err
	}
	if failed != nil {
		s.observer.TransactionCompleted(ctx, app.TransactionObservation{
			Channel: app.ChannelWorker, Kind: failed.Kind(), Status: failed.Status(), FailureCode: failed.FailureCode(),
		})
	}
	return nil
}
