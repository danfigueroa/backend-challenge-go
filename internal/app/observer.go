package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

type TransactionObservation struct {
	Channel          Channel
	Kind             wagering.Kind
	Status           wagering.Status
	FailureCode      wagering.FailureCode
	IdempotentReplay bool
	Duration         time.Duration
}

type ReconciliationObservation struct {
	WalletID   uuid.UUID
	Consistent bool
	Difference money.Money
	Entries    int64
}

type Observer interface {
	TransactionCompleted(ctx context.Context, o TransactionObservation)
	TransactionConflict(ctx context.Context, channel Channel, code wagering.FailureCode)
	OperationRetried(ctx context.Context, operation string, err error)
	ReconciliationCompleted(ctx context.Context, o ReconciliationObservation)
}

type NopObserver struct{}

func (NopObserver) TransactionCompleted(context.Context, TransactionObservation)       {}
func (NopObserver) TransactionConflict(context.Context, Channel, wagering.FailureCode) {}
func (NopObserver) OperationRetried(context.Context, string, error)                    {}
func (NopObserver) ReconciliationCompleted(context.Context, ReconciliationObservation) {}
