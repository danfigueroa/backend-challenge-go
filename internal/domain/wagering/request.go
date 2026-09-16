package wagering

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

const (
	MaxIdentifierLength     = 128
	MaxIdempotencyKeyLength = 255
)

type RequestInput struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID string
}

type Request struct {
	providerID                     string
	externalTransactionID          string
	idempotencyKey                 string
	playerID                       uuid.UUID
	walletID                       uuid.UUID
	roundID                        string
	gameID                         string
	kind                           Kind
	money                          money.Money
	referenceExternalTransactionID string
	payloadHash                    PayloadHash
}

func NewRequest(in RequestInput) (Request, error) {
	var (
		r   Request
		err error
	)

	textFields := []struct {
		name   string
		value  string
		max    int
		target *string
	}{
		{"providerId", in.ProviderID, MaxIdentifierLength, &r.providerID},
		{"externalTransactionId", in.ExternalTransactionID, MaxIdentifierLength, &r.externalTransactionID},
		{"idempotencyKey", in.IdempotencyKey, MaxIdempotencyKeyLength, &r.idempotencyKey},
		{"roundId", in.RoundID, MaxIdentifierLength, &r.roundID},
		{"gameId", in.GameID, MaxIdentifierLength, &r.gameID},
	}
	for _, f := range textFields {
		if err := validateIdentifier(f.name, f.value, f.max); err != nil {
			return Request{}, err
		}
		*f.target = f.value
	}

	if r.playerID, err = parseCanonicalUUID("playerId", in.PlayerID); err != nil {
		return Request{}, err
	}
	if r.walletID, err = parseCanonicalUUID("walletId", in.WalletID); err != nil {
		return Request{}, err
	}
	if in.Kind == "" {
		return Request{}, newValidationError(CodeMissingField, "kind", "is required")
	}
	if r.kind, err = parseExternalKind(in.Kind); err != nil {
		return Request{}, err
	}
	if r.money, err = parseExternalMoney(in.Amount, in.Currency); err != nil {
		return Request{}, err
	}
	if err := validateAmountPolicy(r.kind, r.money); err != nil {
		return Request{}, err
	}
	if err := r.setReference(in.ReferenceExternalTransactionID); err != nil {
		return Request{}, err
	}

	r.payloadHash = sha256.Sum256(r.CanonicalPayload())
	return r, nil
}

func (r *Request) setReference(reference string) error {
	switch {
	case reference == "" && r.kind.RequiresReference():
		return newValidationError(CodeReferenceRequired, "referenceExternalTransactionId", fmt.Sprintf("is required for %s", r.kind))
	case reference == "":
		return nil
	case !r.kind.AcceptsReference():
		return newValidationError(CodeReferenceNotAllowed, "referenceExternalTransactionId", fmt.Sprintf("is not allowed for %s", r.kind))
	case reference == r.externalTransactionID:
		return newValidationError(CodeSelfReference, "referenceExternalTransactionId", "must differ from externalTransactionId")
	}
	if err := validateIdentifier("referenceExternalTransactionId", reference, MaxIdentifierLength); err != nil {
		return err
	}
	r.referenceExternalTransactionID = reference
	return nil
}

func validateIdentifier(field, value string, maxLength int) error {
	switch {
	case value == "":
		return newValidationError(CodeMissingField, field, "is required")
	case len(value) > maxLength:
		return newValidationError(CodeInvalidField, field, fmt.Sprintf("must be at most %d bytes", maxLength))
	}
	for i := range len(value) {
		if value[i] < '!' || value[i] > '~' {
			return newValidationError(CodeInvalidField, field, "must contain only visible ASCII characters")
		}
	}
	return nil
}

func parseCanonicalUUID(field, value string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, newValidationError(CodeMissingField, field, "is required")
	}
	id, err := uuid.Parse(value)
	if err != nil || id.String() != value || id == uuid.Nil {
		return uuid.Nil, newValidationError(CodeInvalidField, field, "must be a canonical lowercase UUID")
	}
	return id, nil
}

func parseExternalMoney(amount, currency string) (money.Money, error) {
	if amount == "" {
		return money.Money{}, newValidationError(CodeMissingField, "money.amount", "is required")
	}
	if currency == "" {
		return money.Money{}, newValidationError(CodeMissingField, "money.currency", "is required")
	}
	m, err := money.Parse(amount, currency)
	switch {
	case errors.Is(err, money.ErrInvalidCurrency):
		return money.Money{}, newValidationError(CodeInvalidCurrency, "money.currency", "must be a supported ISO 4217 code")
	case errors.Is(err, money.ErrNegativeAmount):
		return money.Money{}, newValidationError(CodeInvalidAmount, "money.amount", "must not be negative")
	case err != nil:
		return money.Money{}, newValidationError(CodeInvalidAmount, "money.amount", "must be a decimal string with exactly two fraction digits")
	}
	return m, nil
}

func validateAmountPolicy(kind Kind, m money.Money) error {
	if kind == KindLoss {
		if !m.IsZero() {
			return newValidationError(CodeLossAmountMustBeZero, "money.amount", "must be 0.00 for LOSS")
		}
		return nil
	}
	if !m.IsPositive() {
		return newValidationError(CodeAmountMustBePositive, "money.amount", fmt.Sprintf("must be greater than zero for %s", kind))
	}
	return nil
}

func (r Request) CanonicalPayload() []byte {
	var b strings.Builder
	b.Grow(512)
	b.WriteString(`{"externalTransactionId":`)
	writeJSONString(&b, r.externalTransactionID)
	b.WriteString(`,"gameId":`)
	writeJSONString(&b, r.gameID)
	b.WriteString(`,"kind":`)
	writeJSONString(&b, string(r.kind))
	b.WriteString(`,"money":{"amount":`)
	writeJSONString(&b, r.money.Amount())
	b.WriteString(`,"currency":`)
	writeJSONString(&b, r.money.Currency().Code())
	b.WriteString(`},"playerId":`)
	writeJSONString(&b, r.playerID.String())
	b.WriteString(`,"providerId":`)
	writeJSONString(&b, r.providerID)
	if r.referenceExternalTransactionID != "" {
		b.WriteString(`,"referenceExternalTransactionId":`)
		writeJSONString(&b, r.referenceExternalTransactionID)
	}
	b.WriteString(`,"roundId":`)
	writeJSONString(&b, r.roundID)
	b.WriteString(`,"walletId":`)
	writeJSONString(&b, r.walletID.String())
	b.WriteByte('}')
	return []byte(b.String())
}

func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := range len(s) {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
}

func (r Request) ProviderID() string                     { return r.providerID }
func (r Request) ExternalTransactionID() string          { return r.externalTransactionID }
func (r Request) IdempotencyKey() string                 { return r.idempotencyKey }
func (r Request) PlayerID() uuid.UUID                    { return r.playerID }
func (r Request) WalletID() uuid.UUID                    { return r.walletID }
func (r Request) RoundID() string                        { return r.roundID }
func (r Request) GameID() string                         { return r.gameID }
func (r Request) Kind() Kind                             { return r.kind }
func (r Request) Money() money.Money                     { return r.money }
func (r Request) ReferenceExternalTransactionID() string { return r.referenceExternalTransactionID }
func (r Request) PayloadHash() PayloadHash               { return r.payloadHash }

type PayloadHash [sha256.Size]byte

func PayloadHashFromBytes(b []byte) (PayloadHash, error) {
	var h PayloadHash
	if len(b) != len(h) {
		return PayloadHash{}, fmt.Errorf("%w: payload hash must have %d bytes, got %d", ErrInvalidState, len(h), len(b))
	}
	copy(h[:], b)
	return h, nil
}

func (h PayloadHash) Bytes() []byte  { return h[:] }
func (h PayloadHash) String() string { return hex.EncodeToString(h[:]) }
func (h PayloadHash) IsZero() bool   { return h == PayloadHash{} }
