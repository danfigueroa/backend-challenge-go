package event

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

const (
	WagerTransactionProcessedVersion        = 1
	WagerTransactionRejectedVersion         = 1
	WagerTransactionPendingReferenceVersion = 1
	WalletBalanceChangedVersion             = 1
)

type TransactionRef struct {
	TransactionID                  uuid.UUID       `json:"transactionId"`
	Origin                         wagering.Origin `json:"origin"`
	Kind                           wagering.Kind   `json:"kind"`
	WalletID                       uuid.UUID       `json:"walletId"`
	PlayerID                       uuid.UUID       `json:"playerId"`
	Money                          money.Money     `json:"money"`
	ProviderID                     string          `json:"providerId,omitempty"`
	ExternalTransactionID          string          `json:"externalTransactionId,omitempty"`
	RoundID                        string          `json:"roundId,omitempty"`
	GameID                         string          `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string          `json:"referenceExternalTransactionId,omitempty"`
}

func transactionRef(tx *wagering.Transaction) TransactionRef {
	return TransactionRef{
		TransactionID:                  tx.ID(),
		Origin:                         tx.Origin(),
		Kind:                           tx.Kind(),
		WalletID:                       tx.WalletID(),
		PlayerID:                       tx.PlayerID(),
		Money:                          tx.Money(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalTransactionID(),
		RoundID:                        tx.RoundID(),
		GameID:                         tx.GameID(),
		ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID(),
	}
}

type WagerTransactionProcessedData struct {
	TransactionRef
	ReferenceTransactionID *uuid.UUID  `json:"referenceTransactionId,omitempty"`
	Balance                money.Money `json:"balance"`
	WalletVersion          int64       `json:"walletVersion"`
	ProcessedAt            Timestamp   `json:"processedAt"`
}

type WagerTransactionProcessed = Envelope[WagerTransactionProcessedData]

func NewWagerTransactionProcessed(m Metadata, tx *wagering.Transaction) (WagerTransactionProcessed, error) {
	if tx == nil || tx.Status() != wagering.StatusProcessed {
		return WagerTransactionProcessed{}, fmt.Errorf("%w: %s requires a processed transaction", ErrInvalidEvent, TypeWagerTransactionProcessed)
	}
	result, _ := tx.Result()
	return newEnvelope(m, TypeWagerTransactionProcessed, WagerTransactionProcessedVersion,
		AggregateWagerTransaction, tx.ID(), tx.WalletID().String(),
		WagerTransactionProcessedData{
			TransactionRef:         transactionRef(tx),
			ReferenceTransactionID: optionalID(tx.ReferenceTransactionID()),
			Balance:                result.Balance,
			WalletVersion:          result.WalletVersion,
			ProcessedAt:            Timestamp(tx.CompletedAt()),
		})
}

type WagerTransactionRejectedData struct {
	TransactionRef
	ReferenceTransactionID *uuid.UUID               `json:"referenceTransactionId,omitempty"`
	FailureCode            wagering.FailureCode     `json:"failureCode"`
	FailureCategory        wagering.FailureCategory `json:"failureCategory"`
	RejectedAt             Timestamp                `json:"rejectedAt"`
}

type WagerTransactionRejected = Envelope[WagerTransactionRejectedData]

func NewWagerTransactionRejected(m Metadata, tx *wagering.Transaction) (WagerTransactionRejected, error) {
	if tx == nil || tx.Status() != wagering.StatusRejected {
		return WagerTransactionRejected{}, fmt.Errorf("%w: %s requires a rejected transaction", ErrInvalidEvent, TypeWagerTransactionRejected)
	}
	return newEnvelope(m, TypeWagerTransactionRejected, WagerTransactionRejectedVersion,
		AggregateWagerTransaction, tx.ID(), tx.WalletID().String(),
		WagerTransactionRejectedData{
			TransactionRef:         transactionRef(tx),
			ReferenceTransactionID: optionalID(tx.ReferenceTransactionID()),
			FailureCode:            tx.FailureCode(),
			FailureCategory:        tx.FailureCode().Category(),
			RejectedAt:             Timestamp(tx.CompletedAt()),
		})
}

type WagerTransactionPendingReferenceData struct {
	TransactionRef
	WaitingReason wagering.FailureCode `json:"waitingReason"`
	Attempts      int                  `json:"attempts"`
	NextAttemptAt Timestamp            `json:"nextAttemptAt"`
	ExpiresAt     Timestamp            `json:"expiresAt"`
}

type WagerTransactionPendingReference = Envelope[WagerTransactionPendingReferenceData]

func NewWagerTransactionPendingReference(m Metadata, tx *wagering.Transaction, reason wagering.FailureCode) (WagerTransactionPendingReference, error) {
	if tx == nil || tx.Status() != wagering.StatusPendingReference {
		return WagerTransactionPendingReference{}, fmt.Errorf("%w: %s requires a transaction pending reference", ErrInvalidEvent, TypeWagerTransactionPendingReference)
	}
	if reason != wagering.CodeReferenceNotFound && reason != wagering.CodeReferenceNotProcessed {
		return WagerTransactionPendingReference{}, fmt.Errorf("%w: unexpected waiting reason %q", ErrInvalidEvent, reason)
	}
	return newEnvelope(m, TypeWagerTransactionPendingReference, WagerTransactionPendingReferenceVersion,
		AggregateWagerTransaction, tx.ID(), tx.WalletID().String(),
		WagerTransactionPendingReferenceData{
			TransactionRef: transactionRef(tx),
			WaitingReason:  reason,
			Attempts:       tx.Attempts(),
			NextAttemptAt:  Timestamp(tx.NextAttemptAt()),
			ExpiresAt:      Timestamp(tx.ExpiresAt()),
		})
}

type WalletBalanceChangedData struct {
	WalletID      uuid.UUID        `json:"walletId"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

type WalletBalanceChanged = Envelope[WalletBalanceChangedData]

func NewWalletBalanceChanged(m Metadata, entry wallet.LedgerEntry) (WalletBalanceChanged, error) {
	if entry == (wallet.LedgerEntry{}) {
		return WalletBalanceChanged{}, fmt.Errorf("%w: %s requires a ledger entry", ErrInvalidEvent, TypeWalletBalanceChanged)
	}
	return newEnvelope(m, TypeWalletBalanceChanged, WalletBalanceChangedVersion,
		AggregateWallet, entry.WalletID(), entry.WalletID().String(),
		WalletBalanceChangedData{
			WalletID:      entry.WalletID(),
			TransactionID: entry.TransactionID(),
			Direction:     entry.Direction(),
			Money:         entry.Amount(),
			BalanceBefore: entry.BalanceBefore(),
			BalanceAfter:  entry.BalanceAfter(),
			WalletVersion: entry.WalletVersion(),
		})
}

func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
