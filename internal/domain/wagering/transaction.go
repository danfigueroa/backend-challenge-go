package wagering

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

func ParseOrigin(s string) (Origin, error) {
	switch o := Origin(s); o {
	case OriginInternal, OriginExternal:
		return o, nil
	default:
		return "", fmt.Errorf("%w: unknown origin %q", ErrInvalidState, s)
	}
}

type Result struct {
	Balance       money.Money
	WalletVersion int64
}

type Transaction struct {
	id                             uuid.UUID
	origin                         Origin
	kind                           Kind
	status                         Status
	walletID                       uuid.UUID
	playerID                       uuid.UUID
	money                          money.Money
	providerID                     string
	externalTransactionID          string
	idempotencyKey                 string
	payloadHash                    PayloadHash
	roundID                        string
	gameID                         string
	referenceExternalTransactionID string
	referenceTransactionID         uuid.UUID
	failureCode                    FailureCode
	result                         Result
	attempts                       int
	nextAttemptAt                  time.Time
	expiresAt                      time.Time
	createdAt                      time.Time
	updatedAt                      time.Time
	completedAt                    time.Time
}

type OpeningParams struct {
	ID       uuid.UUID
	WalletID uuid.UUID
	PlayerID uuid.UUID
	Amount   money.Money
	Now      time.Time
}

func NewOpening(p OpeningParams) (*Transaction, error) {
	switch {
	case p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.PlayerID == uuid.Nil:
		return nil, fmt.Errorf("%w: opening requires id, wallet id and player id", ErrInvalidState)
	case !p.Amount.IsValid() || !p.Amount.IsPositive():
		return nil, fmt.Errorf("%w: opening amount must be positive", ErrInvalidState)
	case p.Now.IsZero():
		return nil, fmt.Errorf("%w: timestamp is required", ErrInvalidState)
	}
	now := p.Now.UTC()
	return &Transaction{
		id:        p.ID,
		origin:    OriginInternal,
		kind:      KindOpening,
		status:    StatusPending,
		walletID:  p.WalletID,
		playerID:  p.PlayerID,
		money:     p.Amount,
		createdAt: now,
		updatedAt: now,
	}, nil
}

func NewExternal(id uuid.UUID, req Request, now time.Time) (*Transaction, error) {
	switch {
	case id == uuid.Nil:
		return nil, fmt.Errorf("%w: id is required", ErrInvalidState)
	case req.payloadHash.IsZero():
		return nil, fmt.Errorf("%w: request was not built by NewRequest", ErrInvalidState)
	case now.IsZero():
		return nil, fmt.Errorf("%w: timestamp is required", ErrInvalidState)
	}
	utc := now.UTC()
	return &Transaction{
		id:                             id,
		origin:                         OriginExternal,
		kind:                           req.kind,
		status:                         StatusPending,
		walletID:                       req.walletID,
		playerID:                       req.playerID,
		money:                          req.money,
		providerID:                     req.providerID,
		externalTransactionID:          req.externalTransactionID,
		idempotencyKey:                 req.idempotencyKey,
		payloadHash:                    req.payloadHash,
		roundID:                        req.roundID,
		gameID:                         req.gameID,
		referenceExternalTransactionID: req.referenceExternalTransactionID,
		createdAt:                      utc,
		updatedAt:                      utc,
	}, nil
}

type RehydrateParams struct {
	ID                             uuid.UUID
	Origin                         Origin
	Kind                           Kind
	Status                         Status
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	Money                          money.Money
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    PayloadHash
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	ReferenceTransactionID         uuid.UUID
	FailureCode                    FailureCode
	Result                         Result
	Attempts                       int
	NextAttemptAt                  time.Time
	ExpiresAt                      time.Time
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	CompletedAt                    time.Time
}

func Rehydrate(p RehydrateParams) (*Transaction, error) {
	t := &Transaction{
		id:                             p.ID,
		origin:                         p.Origin,
		kind:                           p.Kind,
		status:                         p.Status,
		walletID:                       p.WalletID,
		playerID:                       p.PlayerID,
		money:                          p.Money,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		referenceTransactionID:         p.ReferenceTransactionID,
		failureCode:                    p.FailureCode,
		result:                         p.Result,
		attempts:                       p.Attempts,
		nextAttemptAt:                  utcOrZero(p.NextAttemptAt),
		expiresAt:                      utcOrZero(p.ExpiresAt),
		createdAt:                      utcOrZero(p.CreatedAt),
		updatedAt:                      utcOrZero(p.UpdatedAt),
		completedAt:                    utcOrZero(p.CompletedAt),
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Transaction) validate() error {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: transaction %s: %s", ErrInvalidState, t.id, fmt.Sprintf(format, args...))
	}

	if t.id == uuid.Nil || t.walletID == uuid.Nil || t.playerID == uuid.Nil {
		return invalid("id, wallet id and player id are required")
	}
	if _, err := ParseKind(string(t.kind)); err != nil {
		return err
	}
	if _, err := ParseStatus(string(t.status)); err != nil {
		return err
	}
	if !t.money.IsValid() || t.money.IsNegative() {
		return invalid("money must be valid and non-negative")
	}
	if t.createdAt.IsZero() || t.updatedAt.Before(t.createdAt) {
		return invalid("timestamps are inconsistent")
	}
	if t.attempts < 0 {
		return invalid("attempts must not be negative")
	}

	if err := t.validateOrigin(); err != nil {
		return invalid("%v", err)
	}
	if err := t.validateAmount(); err != nil {
		return invalid("%v", err)
	}
	if err := t.validateStatusData(); err != nil {
		return invalid("%v", err)
	}
	return nil
}

func (t *Transaction) validateOrigin() error {
	external := t.providerID != "" || t.externalTransactionID != "" || t.idempotencyKey != "" ||
		!t.payloadHash.IsZero() || t.roundID != "" || t.gameID != "" || t.referenceExternalTransactionID != ""

	switch t.origin {
	case OriginInternal:
		if t.kind != KindOpening || external || t.referenceTransactionID != uuid.Nil {
			return fmt.Errorf("internal transactions must be OPENING without external metadata")
		}
	case OriginExternal:
		if t.kind == KindOpening {
			return fmt.Errorf("OPENING must be internal")
		}
		if t.providerID == "" || t.externalTransactionID == "" || t.idempotencyKey == "" ||
			t.payloadHash.IsZero() || t.roundID == "" || t.gameID == "" {
			return fmt.Errorf("external metadata is incomplete")
		}
		if t.kind.RequiresReference() && t.referenceExternalTransactionID == "" {
			return fmt.Errorf("%s requires a reference", t.kind)
		}
		if !t.kind.AcceptsReference() && t.referenceExternalTransactionID != "" {
			return fmt.Errorf("%s does not accept a reference", t.kind)
		}
	default:
		return fmt.Errorf("unknown origin %q", t.origin)
	}
	return nil
}

func (t *Transaction) validateAmount() error {
	if t.kind == KindLoss && !t.money.IsZero() {
		return fmt.Errorf("LOSS amount must be zero")
	}
	if t.kind != KindLoss && !t.money.IsPositive() {
		return fmt.Errorf("%s amount must be positive", t.kind)
	}
	return nil
}

func (t *Transaction) validateStatusData() error {
	switch t.status {
	case StatusPending:
		if t.failureCode != "" || t.result.Balance.IsValid() || !t.completedAt.IsZero() {
			return fmt.Errorf("pending transaction carries completion data")
		}
	case StatusPendingReference:
		if t.nextAttemptAt.IsZero() || t.expiresAt.IsZero() || t.failureCode != "" || !t.completedAt.IsZero() {
			return fmt.Errorf("pending reference requires schedule and no completion data")
		}
	case StatusProcessed:
		if t.failureCode != "" || !t.result.Balance.IsValid() || t.result.Balance.IsNegative() ||
			t.result.WalletVersion < 1 || t.completedAt.IsZero() {
			return fmt.Errorf("processed transaction requires result and no failure code")
		}
	case StatusRejected:
		if t.failureCode.Category() != CategoryDefinitive || t.completedAt.IsZero() {
			return fmt.Errorf("rejected transaction requires a definitive failure code")
		}
	case StatusFailed:
		if t.failureCode.Category() != CategoryInfrastructure || t.completedAt.IsZero() {
			return fmt.Errorf("failed transaction requires an infrastructure failure code")
		}
	}
	return nil
}

func (t *Transaction) MarkProcessed(result Result, referenceTransactionID uuid.UUID, now time.Time) error {
	if err := t.checkTransition(StatusProcessed, now); err != nil {
		return err
	}
	switch {
	case !result.Balance.IsValid() || result.Balance.IsNegative():
		return fmt.Errorf("%w: result balance must be valid and non-negative", ErrInvalidState)
	case result.Balance.Currency() != t.money.Currency():
		return fmt.Errorf("%w: result currency %s differs from %s", ErrInvalidState, result.Balance.Currency(), t.money.Currency())
	case result.WalletVersion < 1:
		return fmt.Errorf("%w: result wallet version must be positive", ErrInvalidState)
	case t.referenceExternalTransactionID != "" && referenceTransactionID == uuid.Nil:
		return fmt.Errorf("%w: resolved reference is required", ErrInvalidState)
	}
	t.status = StatusProcessed
	t.result = result
	t.referenceTransactionID = referenceTransactionID
	t.complete(now)
	return nil
}

type Rejection struct {
	Code                   FailureCode
	ObservedBalance        money.Money
	ObservedWalletVersion  int64
	ReferenceTransactionID uuid.UUID
}

func (t *Transaction) Reject(r Rejection, now time.Time) error {
	if err := t.checkTransition(StatusRejected, now); err != nil {
		return err
	}
	if r.Code.Category() != CategoryDefinitive {
		return fmt.Errorf("%w: %q is not a definitive rejection code", ErrInvalidState, r.Code)
	}
	t.status = StatusRejected
	t.failureCode = r.Code
	t.referenceTransactionID = r.ReferenceTransactionID
	if r.ObservedBalance.IsValid() && r.ObservedWalletVersion >= 1 {
		t.result = Result{Balance: r.ObservedBalance, WalletVersion: r.ObservedWalletVersion}
	}
	t.complete(now)
	return nil
}

func (t *Transaction) Fail(code FailureCode, now time.Time) error {
	if err := t.checkTransition(StatusFailed, now); err != nil {
		return err
	}
	if code.Category() != CategoryInfrastructure {
		return fmt.Errorf("%w: %q is not an infrastructure failure code", ErrInvalidState, code)
	}
	t.status = StatusFailed
	t.failureCode = code
	t.complete(now)
	return nil
}

func (t *Transaction) AwaitReference(nextAttemptAt, expiresAt time.Time, now time.Time) error {
	if err := t.checkTransition(StatusPendingReference, now); err != nil {
		return err
	}
	if !t.kind.AcceptsReference() || t.referenceExternalTransactionID == "" {
		return fmt.Errorf("%w: %s has no reference to await", ErrInvalidTransition, t.kind)
	}
	if t.status == StatusPending {
		if !expiresAt.After(t.createdAt) {
			return fmt.Errorf("%w: expiration must be after creation", ErrInvalidState)
		}
		t.expiresAt = expiresAt.UTC()
	}
	if nextAttemptAt.Before(now) || nextAttemptAt.After(t.expiresAt) {
		return fmt.Errorf("%w: next attempt must be between now and expiration", ErrInvalidState)
	}
	t.status = StatusPendingReference
	t.attempts++
	t.nextAttemptAt = nextAttemptAt.UTC()
	t.updatedAt = maxTime(t.updatedAt, now.UTC())
	return nil
}

func (t *Transaction) checkTransition(next Status, now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidState)
	}
	if !t.status.CanTransitionTo(next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.status, next)
	}
	return nil
}

func (t *Transaction) complete(now time.Time) {
	utc := now.UTC()
	t.nextAttemptAt = time.Time{}
	t.completedAt = maxTime(t.createdAt, utc)
	t.updatedAt = maxTime(t.updatedAt, utc)
}

func (t *Transaction) MatchesPayload(hash PayloadHash) bool {
	return t.payloadHash == hash
}

func utcOrZero(ts time.Time) time.Time {
	if ts.IsZero() {
		return time.Time{}
	}
	return ts.UTC()
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (t *Transaction) ID() uuid.UUID                 { return t.id }
func (t *Transaction) Origin() Origin                { return t.origin }
func (t *Transaction) Kind() Kind                    { return t.kind }
func (t *Transaction) Status() Status                { return t.status }
func (t *Transaction) WalletID() uuid.UUID           { return t.walletID }
func (t *Transaction) PlayerID() uuid.UUID           { return t.playerID }
func (t *Transaction) Money() money.Money            { return t.money }
func (t *Transaction) ProviderID() string            { return t.providerID }
func (t *Transaction) ExternalTransactionID() string { return t.externalTransactionID }
func (t *Transaction) IdempotencyKey() string        { return t.idempotencyKey }
func (t *Transaction) PayloadHash() PayloadHash      { return t.payloadHash }
func (t *Transaction) RoundID() string               { return t.roundID }
func (t *Transaction) GameID() string                { return t.gameID }
func (t *Transaction) ReferenceExternalTransactionID() string {
	return t.referenceExternalTransactionID
}
func (t *Transaction) ReferenceTransactionID() uuid.UUID { return t.referenceTransactionID }
func (t *Transaction) FailureCode() FailureCode          { return t.failureCode }
func (t *Transaction) Attempts() int                     { return t.attempts }
func (t *Transaction) NextAttemptAt() time.Time          { return t.nextAttemptAt }
func (t *Transaction) ExpiresAt() time.Time              { return t.expiresAt }
func (t *Transaction) CreatedAt() time.Time              { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time              { return t.updatedAt }
func (t *Transaction) CompletedAt() time.Time            { return t.completedAt }

func (t *Transaction) Result() (Result, bool) {
	return t.result, t.result.Balance.IsValid()
}
