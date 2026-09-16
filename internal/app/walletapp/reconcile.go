package walletapp

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

type ReconciliationReport struct {
	WalletID          uuid.UUID
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money
	Consistent        bool
	CheckedEntries    int64
	WalletVersion     int64
	LedgerVersion     int64
	ChainBreaks       int64
	CheckedAt         time.Time
}

func (s *Service) Reconcile(ctx context.Context, actor app.Actor, walletID uuid.UUID) (ReconciliationReport, error) {
	if err := actor.RequireInternalService(); err != nil {
		return ReconciliationReport{}, err
	}

	var report ReconciliationReport
	err := s.tx.WithinSnapshot(ctx, func(ctx context.Context) error {
		w, err := s.wallets.Get(ctx, walletID)
		if err != nil {
			return err
		}
		summary, err := s.ledger.Summarize(ctx, walletID)
		if err != nil {
			return err
		}
		report, err = buildReport(w.ID(), w.Balance(), w.Version(), summary, s.clock.Now())
		return err
	})
	if err != nil {
		return ReconciliationReport{}, err
	}

	s.observer.ReconciliationCompleted(ctx, app.ReconciliationObservation{
		WalletID: report.WalletID, Consistent: report.Consistent, Difference: report.Difference, Entries: report.CheckedEntries,
	})
	return report, nil
}

func buildReport(walletID uuid.UUID, stored money.Money, version int64, summary app.LedgerSummary, at time.Time) (ReconciliationReport, error) {
	calculated, err := money.New(summary.NetMinor, stored.Currency())
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("reconcile: calculated balance: %w", err)
	}
	difference, err := stored.Sub(calculated)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("reconcile: difference: %w", err)
	}

	versionMatches := summary.LastVersion == version
	lastBalanceMatches := summary.LastBalanceMinor == stored.Minor()
	if summary.Entries == 0 {
		versionMatches = version == 1
		lastBalanceMatches = stored.IsZero()
	}

	return ReconciliationReport{
		WalletID:          walletID,
		StoredBalance:     stored,
		CalculatedBalance: calculated,
		Difference:        difference,
		Consistent:        difference.IsZero() && summary.ChainBreaks == 0 && versionMatches && lastBalanceMatches,
		CheckedEntries:    summary.Entries,
		WalletVersion:     version,
		LedgerVersion:     summary.LastVersion,
		ChainBreaks:       summary.ChainBreaks,
		CheckedAt:         at,
	}, nil
}
