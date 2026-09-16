package walletapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/event"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

type OpenWalletCommand struct {
	Actor    app.Actor
	Meta     app.Metadata
	PlayerID string
	Amount   string
	Currency string
}

func (s *Service) OpenWallet(ctx context.Context, cmd OpenWalletCommand) (WalletView, error) {
	if err := cmd.Actor.RequireInternalService(); err != nil {
		return WalletView{}, err
	}
	playerID, initial, err := parseOpenWallet(cmd)
	if err != nil {
		return WalletView{}, err
	}
	meta := cmd.Meta.WithDefaults(s.ids)

	var view WalletView
	err = app.Retry(ctx, s.retry, s.retried(ctx, "open_wallet"), func(ctx context.Context) error {
		return s.tx.WithinTx(ctx, func(ctx context.Context) error {
			opened, err := s.persistOpening(ctx, meta, playerID, initial)
			if err != nil {
				return err
			}
			view = viewOf(opened)
			return nil
		})
	})
	if err != nil {
		return WalletView{}, err
	}
	return view, nil
}

func (s *Service) persistOpening(ctx context.Context, meta app.Metadata, playerID uuid.UUID, initial money.Money) (*wallet.Wallet, error) {
	now := s.clock.Now()
	walletID, openingID := s.ids.NewID(), s.ids.NewID()

	opened, err := wallet.Open(wallet.OpenParams{
		ID: walletID, PlayerID: playerID, InitialBalance: initial,
		OpeningTransactionID: openingID, OpeningEntryID: s.ids.NewID(), Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("open wallet: %w", err)
	}
	if err := s.wallets.Insert(ctx, opened.Wallet); err != nil {
		return nil, err
	}
	if opened.OpeningEntry == nil {
		return opened.Wallet, nil
	}

	opening, err := wagering.NewOpening(wagering.OpeningParams{
		ID: openingID, WalletID: walletID, PlayerID: playerID, Amount: initial, Now: now,
	})
	if err != nil {
		return nil, fmt.Errorf("create opening transaction: %w", err)
	}
	result := wagering.Result{Balance: opened.Wallet.Balance(), WalletVersion: opened.Wallet.Version()}
	if err := opening.MarkProcessed(result, uuid.Nil, now); err != nil {
		return nil, fmt.Errorf("process opening transaction: %w", err)
	}
	if err := s.transactions.Insert(ctx, opening); err != nil {
		return nil, err
	}
	if err := s.ledger.Insert(ctx, *opened.OpeningEntry); err != nil {
		return nil, err
	}

	processed, err := event.NewWagerTransactionProcessed(s.eventMetadata(meta, now), opening)
	if err != nil {
		return nil, fmt.Errorf("build opening event: %w", err)
	}
	changed, err := event.NewWalletBalanceChanged(s.eventMetadata(meta, now), *opened.OpeningEntry)
	if err != nil {
		return nil, fmt.Errorf("build balance event: %w", err)
	}
	if err := s.outbox.Append(ctx, now, processed, changed); err != nil {
		return nil, err
	}
	return opened.Wallet, nil
}

func parseOpenWallet(cmd OpenWalletCommand) (uuid.UUID, money.Money, error) {
	if cmd.PlayerID == "" {
		return uuid.Nil, money.Money{}, &app.ValidationError{Code: string(wagering.CodeMissingField), Field: "playerId", Reason: "is required"}
	}
	playerID, err := uuid.Parse(cmd.PlayerID)
	if err != nil || playerID.String() != cmd.PlayerID || playerID == uuid.Nil {
		return uuid.Nil, money.Money{}, &app.ValidationError{Code: string(wagering.CodeInvalidField), Field: "playerId", Reason: "must be a canonical lowercase UUID"}
	}
	switch {
	case cmd.Amount == "":
		return uuid.Nil, money.Money{}, &app.ValidationError{Code: string(wagering.CodeMissingField), Field: "initialBalance.amount", Reason: "is required"}
	case cmd.Currency == "":
		return uuid.Nil, money.Money{}, &app.ValidationError{Code: string(wagering.CodeMissingField), Field: "initialBalance.currency", Reason: "is required"}
	}
	initial, err := money.Parse(cmd.Amount, cmd.Currency)
	switch {
	case errors.Is(err, money.ErrInvalidCurrency):
		return uuid.Nil, money.Money{}, &app.ValidationError{Code: string(wagering.CodeInvalidCurrency), Field: "initialBalance.currency", Reason: "must be a supported ISO 4217 code"}
	case err != nil:
		return uuid.Nil, money.Money{}, &app.ValidationError{Code: string(wagering.CodeInvalidAmount), Field: "initialBalance.amount", Reason: "must be a non-negative decimal string with exactly two fraction digits"}
	}
	return playerID, initial, nil
}

func (s *Service) eventMetadata(meta app.Metadata, at time.Time) event.Metadata {
	return event.Metadata{EventID: s.ids.NewID(), CorrelationID: meta.CorrelationID, CausationID: meta.CausationID, OccurredAt: at}
}

func (s *Service) retried(ctx context.Context, operation string) func(int, error) {
	return func(_ int, err error) { s.observer.OperationRetried(ctx, operation, err) }
}
