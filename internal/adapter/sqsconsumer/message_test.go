package sqsconsumer

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	"github.com/danfigueroa/backend-challenge-go/internal/app"
	"github.com/danfigueroa/backend-challenge-go/internal/app/wageringapp"
	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
	"github.com/danfigueroa/backend-challenge-go/internal/platform/metrics"
)

const specMessage = `{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}`

func TestDecodeSpecificationExample(t *testing.T) {
	t.Parallel()

	d, err := Decode(specMessage)
	if err != nil {
		t.Fatal(err)
	}
	if d.MessageID != "msg-123" || !d.OccurredAt.Equal(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)) ||
		d.Input.IdempotencyKey != "provider-a:transaction-123" || d.Input.Amount != "25.00" || d.Input.Kind != "BET" {
		t.Errorf("decoded = %+v", d)
	}

	compact := strings.Join(strings.Fields(specMessage), "")
	again, err := Decode(compact)
	if err != nil {
		t.Fatal(err)
	}
	if again.PayloadHash != d.PayloadHash {
		t.Error("whitespace changed the delivery hash")
	}

	otherTime, err := Decode(strings.Replace(specMessage, "12:00:00.000Z", "13:00:00.000Z", 1))
	if err != nil || otherTime.PayloadHash != d.PayloadHash {
		t.Errorf("occurredAt is transport metadata and must not change the hash: %v", err)
	}
	otherKey, err := Decode(strings.Replace(specMessage, `"idempotencyKey": "provider-a:transaction-123"`, `"idempotencyKey": "k2"`, 1))
	if err != nil || otherKey.PayloadHash == d.PayloadHash {
		t.Errorf("a different idempotency key must change the delivery hash: %v", err)
	}
	otherAmount, err := Decode(strings.Replace(specMessage, `"25.00"`, `"26.00"`, 1))
	if err != nil || otherAmount.PayloadHash == d.PayloadHash {
		t.Errorf("a different amount must change the delivery hash: %v", err)
	}
}

func TestDecodeRejectsInvalidMessages(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body string
		want error
		code wagering.FailureCode
	}{
		"not json":           {`hello`, ErrMalformedMessage, ""},
		"array":              {`[]`, ErrMalformedMessage, ""},
		"trailing data":      {specMessage + `{}`, ErrMalformedMessage, ""},
		"unknown field":      {strings.Replace(specMessage, `"type"`, `"extra": 1, "type"`, 1), ErrMalformedMessage, ""},
		"missing message id": {strings.Replace(specMessage, `"msg-123"`, `""`, 1), ErrMalformedMessage, ""},
		"message id spaces":  {strings.Replace(specMessage, `"msg-123"`, `"msg 123"`, 1), ErrMalformedMessage, ""},
		"wrong type":         {strings.Replace(specMessage, `WagerTransactionRequested`, `WalletOpened`, 1), ErrMalformedMessage, ""},
		"bad occurredAt":     {strings.Replace(specMessage, `2026-09-08T12:00:00.000Z`, `yesterday`, 1), ErrMalformedMessage, ""},
		"missing data":       {`{"messageId":"m","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z"}`, ErrMalformedMessage, ""},
		"number amount":      {strings.Replace(specMessage, `"25.00"`, `25.00`, 1), ErrMalformedMessage, ""},
		"opening kind":       {strings.Replace(specMessage, `"BET"`, `"OPENING"`, 1), wagering.ErrValidation, wagering.CodeOpeningNotAllowed},
		"missing key":        {strings.Replace(specMessage, `"provider-a:transaction-123"`, `""`, 1), wagering.ErrValidation, wagering.CodeMissingField},
		"scientific amount":  {strings.Replace(specMessage, `"25.00"`, `"2.5e1"`, 1), wagering.ErrValidation, wagering.CodeInvalidAmount},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Decode(tc.body)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			reason, code := classifyDecodeError(err)
			if tc.code != "" && (reason != ReasonValidation || code != string(tc.code)) {
				t.Errorf("classification = %s/%s", reason, code)
			}
			if tc.code == "" && reason != ReasonMalformed {
				t.Errorf("classification = %s", reason)
			}
		})
	}
}

func TestClassifyProcessError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err    error
		want   outcome
		reason string
	}{
		{nil, outcomeDelete, ""},
		{wagering.NewValidationError(wagering.CodeWalletNotFound, "walletId", "missing"), outcomeDeadLetter, ReasonValidation},
		{&wageringapp.IdempotencyConflictError{Code: wagering.CodeIdempotencyKeyConflict, ExistingTransactionID: uuid.New()}, outcomeDeadLetter, ReasonConflict},
		{fmt.Errorf("wrapped: %w", app.ErrInboxMismatch), outcomeDeadLetter, ReasonInbox},
		{app.ErrForbidden, outcomeDeadLetter, ReasonForbidden},
		{fmt.Errorf("%w: db down", app.ErrTransient), outcomeRetry, ""},
		{app.ErrIntegrityViolation, outcomeRetry, ""},
		{errors.New("unexpected"), outcomeRetry, ""},
	}
	for _, tc := range tests {
		got, reason, _ := classifyProcessError(tc.err)
		if got != tc.want || reason != tc.reason {
			t.Errorf("classify(%v) = %v/%s, want %v/%s", tc.err, got, reason, tc.want, tc.reason)
		}
	}
}

func TestRetryDelayAndReceiveCount(t *testing.T) {
	t.Parallel()

	c, err := New(noopSQS{}, noopProcessor{}, Settings{
		ConsumerName: "c", QueueURL: "q", DeadLetterURL: "d", Workers: 1, MaxMessages: 10,
		ProcessingTimeout: time.Second, RetryBaseDelay: 2 * time.Second, RetryMaxDelay: 30 * time.Second, ErrorBackoff: time.Second,
	}, Hooks{}, metrics.New(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for receive, want := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 5: 30 * time.Second, 50: 30 * time.Second} {
		if got := c.retryDelay(receive); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", receive, got, want)
		}
	}

	msg := types.Message{MessageId: aws.String("x"), Attributes: map[string]string{"ApproximateReceiveCount": "3", "MessageGroupId": "wallet-1"}}
	if receiveCountOf(msg) != 3 || attributeSystem(msg, types.MessageSystemAttributeNameMessageGroupId) != "wallet-1" {
		t.Errorf("attributes not read: %+v", msg.Attributes)
	}
	if receiveCountOf(types.Message{}) != 1 {
		t.Error("missing receive count must default to 1")
	}

	if _, err := New(noopSQS{}, noopProcessor{}, Settings{}, Hooks{}, metrics.New(), slog.Default()); err == nil {
		t.Error("empty settings accepted")
	}
}
