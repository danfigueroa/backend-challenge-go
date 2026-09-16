package wagering

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/wallet"
)

type PendingPolicy struct {
	TTL       time.Duration
	BaseDelay time.Duration
	MaxDelay  time.Duration
	Jitter    func(time.Duration) time.Duration
}

func (p PendingPolicy) Validate() error {
	if p.TTL <= 0 || p.BaseDelay <= 0 || p.MaxDelay < p.BaseDelay {
		return fmt.Errorf("%w: pending policy requires TTL > 0 and 0 < base delay <= max delay", ErrInvalidState)
	}
	return nil
}

func (p PendingPolicy) Delay(attempt int) time.Duration {
	delay := p.BaseDelay
	for i := 1; i < attempt && delay < p.MaxDelay; i++ {
		delay *= 2
	}
	delay = min(delay, p.MaxDelay)
	if p.Jitter != nil {
		delay += max(p.Jitter(delay), 0)
	}
	return delay
}

type Reference struct {
	Transaction     *Transaction
	AlreadyReversed bool
}

type Outcome string

const (
	OutcomeProcessed         Outcome = "PROCESSED"
	OutcomeRejected          Outcome = "REJECTED"
	OutcomeAwaitingReference Outcome = "AWAITING_REFERENCE"
)

type Decision struct {
	Outcome     Outcome
	FailureCode FailureCode
	Entry       *wallet.LedgerEntry
}

type ProcessParams struct {
	Transaction *Transaction
	Wallet      *wallet.Wallet
	Reference   *Reference
	EntryID     uuid.UUID
	Now         time.Time
	Policy      PendingPolicy
}

func CheckWallet(t *Transaction, w *wallet.Wallet) error {
	switch {
	case t == nil || w == nil:
		return fmt.Errorf("%w: transaction and wallet are required", ErrInvalidState)
	case t.walletID != w.ID():
		return fmt.Errorf("%w: transaction wallet %s, loaded wallet %s", ErrWalletMismatch, t.walletID, w.ID())
	case t.playerID != w.PlayerID():
		return newValidationError(CodeWalletPlayerMismatch, "playerId", "does not own the wallet")
	case t.money.Currency() != w.Currency():
		return newValidationError(CodeWalletCurrencyMismatch, "money.currency", fmt.Sprintf("wallet currency is %s", w.Currency()))
	}
	return nil
}

func Process(p ProcessParams) (Decision, error) {
	t, w := p.Transaction, p.Wallet
	if err := CheckWallet(t, w); err != nil {
		return Decision{}, err
	}
	switch {
	case t.origin != OriginExternal:
		return Decision{}, fmt.Errorf("%w: only external transactions are processed", ErrInvalidState)
	case t.status != StatusPending && t.status != StatusPendingReference:
		return Decision{}, fmt.Errorf("%w: %s is not processable", ErrInvalidTransition, t.status)
	case p.Now.IsZero():
		return Decision{}, fmt.Errorf("%w: timestamp is required", ErrInvalidState)
	}
	if err := p.Policy.Validate(); err != nil {
		return Decision{}, err
	}

	var ref *Transaction
	if t.referenceExternalTransactionID != "" {
		resolved, waitCode, rejectCode := resolveReference(t, p.Reference)
		switch {
		case waitCode != "":
			return awaitOrExpire(p, waitCode)
		case rejectCode != "":
			return reject(p, rejectCode, referenceID(resolved))
		}
		ref = resolved
	}

	direction, moves := movementDirection(t.kind, ref)
	if !moves {
		return markProcessed(p, ref, nil)
	}

	movement := wallet.Movement{EntryID: p.EntryID, TransactionID: t.id, Amount: t.money, Now: p.Now}
	var (
		entry wallet.LedgerEntry
		err   error
	)
	if direction == wallet.DirectionDebit {
		entry, err = w.Debit(movement)
	} else {
		entry, err = w.Credit(movement)
	}
	switch {
	case errors.Is(err, wallet.ErrInsufficientFunds):
		code := CodeInsufficientFunds
		if t.kind.IsReversal() {
			code = CodeReversalInsufficientFunds
		}
		return reject(p, code, referenceID(ref))
	case errors.Is(err, wallet.ErrBalanceOverflow):
		return reject(p, CodeBalanceLimitExceeded, referenceID(ref))
	case err != nil:
		return Decision{}, fmt.Errorf("apply %s to wallet: %w", t.kind, err)
	}
	return markProcessed(p, ref, &entry)
}

func resolveReference(t *Transaction, r *Reference) (resolved *Transaction, waitCode, rejectCode FailureCode) {
	if r == nil || r.Transaction == nil {
		return nil, CodeReferenceNotFound, ""
	}
	ref := r.Transaction
	switch ref.status {
	case StatusPending, StatusPendingReference:
		return ref, CodeReferenceNotProcessed, ""
	case StatusRejected, StatusFailed:
		return ref, "", CodeReferenceNotProcessed
	case StatusProcessed:
	}

	switch {
	case ref.id == t.id || ref.origin != OriginExternal || !ref.kind.canBeReferencedBy(t.kind):
		return ref, "", CodeReferenceKindInvalid
	case ref.providerID != t.providerID:
		return ref, "", CodeReferenceProviderMismatch
	case ref.playerID != t.playerID:
		return ref, "", CodeReferencePlayerMismatch
	case ref.walletID != t.walletID:
		return ref, "", CodeReferenceWalletMismatch
	case ref.money.Currency() != t.money.Currency():
		return ref, "", CodeReferenceCurrencyMismatch
	case ref.roundID != t.roundID:
		return ref, "", CodeReferenceRoundMismatch
	case t.kind.IsReversal() && !ref.money.Equal(t.money):
		return ref, "", CodeReferenceAmountMismatch
	case t.kind.IsReversal() && r.AlreadyReversed:
		return ref, "", CodeReferenceAlreadyReversed
	}
	return ref, "", ""
}

func movementDirection(kind Kind, ref *Transaction) (wallet.Direction, bool) {
	switch kind {
	case KindBet:
		return wallet.DirectionDebit, true
	case KindWin, KindRefund, KindOpening:
		return wallet.DirectionCredit, true
	case KindRollback:
		if ref == nil {
			return "", false
		}
		original, moves := movementDirection(ref.kind, nil)
		if !moves {
			return "", false
		}
		if original == wallet.DirectionDebit {
			return wallet.DirectionCredit, true
		}
		return wallet.DirectionDebit, true
	case KindLoss:
		return "", false
	}
	return "", false
}

func awaitOrExpire(p ProcessParams, code FailureCode) (Decision, error) {
	t := p.Transaction
	now := p.Now.UTC()
	expiresAt := t.expiresAt
	if expiresAt.IsZero() {
		expiresAt = t.createdAt.Add(p.Policy.TTL)
	}
	if !now.Before(expiresAt) {
		return reject(p, code, uuid.Nil)
	}
	next := now.Add(p.Policy.Delay(t.attempts + 1))
	if next.After(expiresAt) {
		next = expiresAt
	}
	if err := t.AwaitReference(next, expiresAt, now); err != nil {
		return Decision{}, err
	}
	return Decision{Outcome: OutcomeAwaitingReference, FailureCode: code}, nil
}

func reject(p ProcessParams, code FailureCode, referenceTransactionID uuid.UUID) (Decision, error) {
	err := p.Transaction.Reject(Rejection{
		Code:                   code,
		ObservedBalance:        p.Wallet.Balance(),
		ObservedWalletVersion:  p.Wallet.Version(),
		ReferenceTransactionID: referenceTransactionID,
	}, p.Now)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Outcome: OutcomeRejected, FailureCode: code}, nil
}

func markProcessed(p ProcessParams, ref *Transaction, entry *wallet.LedgerEntry) (Decision, error) {
	result := Result{Balance: p.Wallet.Balance(), WalletVersion: p.Wallet.Version()}
	if err := p.Transaction.MarkProcessed(result, referenceID(ref), p.Now); err != nil {
		return Decision{}, err
	}
	return Decision{Outcome: OutcomeProcessed, Entry: entry}, nil
}

func referenceID(ref *Transaction) uuid.UUID {
	if ref == nil {
		return uuid.Nil
	}
	return ref.id
}
