package sqsconsumer

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

const (
	MessageType     = "WagerTransactionRequested"
	maxMessageIDLen = 128
)

var ErrMalformedMessage = errors.New("sqsconsumer: malformed message")

type envelope struct {
	MessageID  string   `json:"messageId"`
	Type       string   `json:"type"`
	OccurredAt string   `json:"occurredAt"`
	Data       *payload `json:"data"`
}

type moneyPayload struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type payload struct {
	ProviderID                     string        `json:"providerId"`
	ExternalTransactionID          string        `json:"externalTransactionId"`
	IdempotencyKey                 string        `json:"idempotencyKey"`
	PlayerID                       string        `json:"playerId"`
	WalletID                       string        `json:"walletId"`
	RoundID                        string        `json:"roundId"`
	GameID                         string        `json:"gameId"`
	Kind                           string        `json:"kind"`
	Money                          *moneyPayload `json:"money"`
	ReferenceExternalTransactionID string        `json:"referenceExternalTransactionId"`
}

type Decoded struct {
	MessageID   string
	OccurredAt  time.Time
	Input       wagering.RequestInput
	PayloadHash [sha256.Size]byte
}

func Decode(body string) (Decoded, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return Decoded{}, fmt.Errorf("%w: %w", ErrMalformedMessage, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Decoded{}, fmt.Errorf("%w: unexpected data after the JSON object", ErrMalformedMessage)
	}

	switch {
	case !validMessageID(env.MessageID):
		return Decoded{}, fmt.Errorf("%w: messageId must be 1-%d visible ASCII characters", ErrMalformedMessage, maxMessageIDLen)
	case env.Type != MessageType:
		return Decoded{}, fmt.Errorf("%w: unsupported type %q", ErrMalformedMessage, env.Type)
	case env.Data == nil:
		return Decoded{}, fmt.Errorf("%w: data is required", ErrMalformedMessage)
	}
	occurredAt, err := time.Parse(time.RFC3339Nano, env.OccurredAt)
	if err != nil {
		return Decoded{}, fmt.Errorf("%w: occurredAt must be RFC 3339: %w", ErrMalformedMessage, err)
	}

	d := env.Data
	var m moneyPayload
	if d.Money != nil {
		m = *d.Money
	}
	input := wagering.RequestInput{
		ProviderID: d.ProviderID, ExternalTransactionID: d.ExternalTransactionID, IdempotencyKey: d.IdempotencyKey,
		PlayerID: d.PlayerID, WalletID: d.WalletID, RoundID: d.RoundID, GameID: d.GameID, Kind: d.Kind,
		Amount: m.Amount, Currency: m.Currency, ReferenceExternalTransactionID: d.ReferenceExternalTransactionID,
	}
	req, err := wagering.NewRequest(input)
	if err != nil {
		return Decoded{}, err
	}
	return Decoded{MessageID: env.MessageID, OccurredAt: occurredAt.UTC(), Input: input, PayloadHash: deliveryHash(req)}, nil
}

func deliveryHash(req wagering.Request) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte(MessageType))
	h.Write([]byte{'\n'})
	h.Write([]byte(req.IdempotencyKey()))
	h.Write([]byte{'\n'})
	h.Write(req.CanonicalPayload())
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func validMessageID(id string) bool {
	if id == "" || len(id) > maxMessageIDLen {
		return false
	}
	for i := range len(id) {
		if id[i] < '!' || id[i] > '~' {
			return false
		}
	}
	return true
}
