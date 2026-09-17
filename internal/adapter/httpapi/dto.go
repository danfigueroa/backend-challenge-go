package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app/walletapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

type moneyInput struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type openWalletRequest struct {
	PlayerID       string      `json:"playerId"`
	InitialBalance *moneyInput `json:"initialBalance"`
}

type transactionRequest struct {
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	PlayerID                       string      `json:"playerId"`
	WalletID                       string      `json:"walletId"`
	RoundID                        string      `json:"roundId"`
	GameID                         string      `json:"gameId"`
	Kind                           string      `json:"kind"`
	Money                          *moneyInput `json:"money"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
}

func (r transactionRequest) input(idempotencyKey string) wagering.RequestInput {
	var m moneyInput
	if r.Money != nil {
		m = *r.Money
	}
	return wagering.RequestInput{
		ProviderID: r.ProviderID, ExternalTransactionID: r.ExternalTransactionID, IdempotencyKey: idempotencyKey,
		PlayerID: r.PlayerID, WalletID: r.WalletID, RoundID: r.RoundID, GameID: r.GameID, Kind: r.Kind,
		Amount: m.Amount, Currency: m.Currency, ReferenceExternalTransactionID: r.ReferenceExternalTransactionID,
	}
}

type timestamp time.Time

func (t timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format("2006-01-02T15:04:05.000Z07:00") + `"`), nil
}

func optionalTime(t time.Time) *timestamp {
	if t.IsZero() {
		return nil
	}
	ts := timestamp(t)
	return &ts
}

type walletResponse struct {
	ID        uuid.UUID   `json:"id"`
	PlayerID  uuid.UUID   `json:"playerId"`
	Balance   money.Money `json:"balance"`
	Version   int64       `json:"version"`
	CreatedAt timestamp   `json:"createdAt"`
	UpdatedAt timestamp   `json:"updatedAt"`
}

func walletBody(v walletapp.WalletView) walletResponse {
	return walletResponse{
		ID: v.ID, PlayerID: v.PlayerID, Balance: v.Balance, Version: v.Version,
		CreatedAt: timestamp(v.CreatedAt), UpdatedAt: timestamp(v.UpdatedAt),
	}
}

type ledgerEntryResponse struct {
	ID            uuid.UUID        `json:"id"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Amount        money.Money      `json:"amount"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
	CreatedAt     timestamp        `json:"createdAt"`
}

type ledgerPageResponse struct {
	WalletID   uuid.UUID             `json:"walletId"`
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func ledgerBody(walletID uuid.UUID, page walletapp.LedgerPage) ledgerPageResponse {
	entries := make([]ledgerEntryResponse, 0, len(page.Entries))
	for _, e := range page.Entries {
		entries = append(entries, ledgerEntryResponse{
			ID: e.ID(), TransactionID: e.TransactionID(), Direction: e.Direction(), Amount: e.Amount(),
			BalanceBefore: e.BalanceBefore(), BalanceAfter: e.BalanceAfter(), WalletVersion: e.WalletVersion(),
			CreatedAt: timestamp(e.CreatedAt()),
		})
	}
	return ledgerPageResponse{WalletID: walletID, Entries: entries, NextCursor: page.NextCursor}
}

type reconciliationResponse struct {
	WalletID          uuid.UUID   `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int64       `json:"checkedEntries"`
	WalletVersion     int64       `json:"walletVersion"`
	LedgerVersion     int64       `json:"ledgerVersion"`
	ChainBreaks       int64       `json:"chainBreaks"`
	CheckedAt         timestamp   `json:"checkedAt"`
}

func reconciliationBody(r walletapp.ReconciliationReport) reconciliationResponse {
	return reconciliationResponse{
		WalletID: r.WalletID, StoredBalance: r.StoredBalance, CalculatedBalance: r.CalculatedBalance,
		Difference: r.Difference, Consistent: r.Consistent, CheckedEntries: r.CheckedEntries,
		WalletVersion: r.WalletVersion, LedgerVersion: r.LedgerVersion, ChainBreaks: r.ChainBreaks,
		CheckedAt: timestamp(r.CheckedAt),
	}
}

type processResponse struct {
	TransactionID    uuid.UUID            `json:"transactionId"`
	Status           wagering.Status      `json:"status"`
	Balance          *money.Money         `json:"balance,omitempty"`
	FailureCode      wagering.FailureCode `json:"failureCode,omitempty"`
	IdempotentReplay bool                 `json:"idempotentReplay"`
	NextAttemptAt    *timestamp           `json:"nextAttemptAt,omitempty"`
	ExpiresAt        *timestamp           `json:"expiresAt,omitempty"`
}

func processBody(t *wagering.Transaction, replay bool) processResponse {
	body := processResponse{
		TransactionID: t.ID(), Status: t.Status(), FailureCode: t.FailureCode(), IdempotentReplay: replay,
	}
	if result, ok := t.Result(); ok {
		body.Balance = &result.Balance
	}
	if t.Status() == wagering.StatusPendingReference {
		body.NextAttemptAt, body.ExpiresAt = optionalTime(t.NextAttemptAt()), optionalTime(t.ExpiresAt())
	}
	return body
}

type transactionResponse struct {
	TransactionID                  uuid.UUID                `json:"transactionId"`
	Origin                         wagering.Origin          `json:"origin"`
	Kind                           wagering.Kind            `json:"kind"`
	Status                         wagering.Status          `json:"status"`
	WalletID                       uuid.UUID                `json:"walletId"`
	PlayerID                       uuid.UUID                `json:"playerId"`
	Money                          money.Money              `json:"money"`
	ProviderID                     string                   `json:"providerId,omitempty"`
	ExternalTransactionID          string                   `json:"externalTransactionId,omitempty"`
	RoundID                        string                   `json:"roundId,omitempty"`
	GameID                         string                   `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string                   `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         *uuid.UUID               `json:"referenceTransactionId,omitempty"`
	FailureCode                    wagering.FailureCode     `json:"failureCode,omitempty"`
	FailureCategory                wagering.FailureCategory `json:"failureCategory,omitempty"`
	Balance                        *money.Money             `json:"balance,omitempty"`
	WalletVersion                  int64                    `json:"walletVersion,omitempty"`
	Attempts                       int                      `json:"attempts"`
	NextAttemptAt                  *timestamp               `json:"nextAttemptAt,omitempty"`
	ExpiresAt                      *timestamp               `json:"expiresAt,omitempty"`
	CreatedAt                      timestamp                `json:"createdAt"`
	UpdatedAt                      timestamp                `json:"updatedAt"`
	CompletedAt                    *timestamp               `json:"completedAt,omitempty"`
}

func transactionBody(t *wagering.Transaction) transactionResponse {
	body := transactionResponse{
		TransactionID: t.ID(), Origin: t.Origin(), Kind: t.Kind(), Status: t.Status(),
		WalletID: t.WalletID(), PlayerID: t.PlayerID(), Money: t.Money(),
		ProviderID: t.ProviderID(), ExternalTransactionID: t.ExternalTransactionID(), RoundID: t.RoundID(), GameID: t.GameID(),
		ReferenceExternalTransactionID: t.ReferenceExternalTransactionID(),
		FailureCode:                    t.FailureCode(), Attempts: t.Attempts(),
		NextAttemptAt: optionalTime(t.NextAttemptAt()), ExpiresAt: optionalTime(t.ExpiresAt()),
		CreatedAt: timestamp(t.CreatedAt()), UpdatedAt: timestamp(t.UpdatedAt()), CompletedAt: optionalTime(t.CompletedAt()),
	}
	if t.FailureCode() != "" {
		body.FailureCategory = t.FailureCode().Category()
	}
	if ref := t.ReferenceTransactionID(); ref != uuid.Nil {
		body.ReferenceTransactionID = &ref
	}
	if result, ok := t.Result(); ok {
		body.Balance, body.WalletVersion = &result.Balance, result.WalletVersion
	}
	return body
}
