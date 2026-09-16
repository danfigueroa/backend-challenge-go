package wagering_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/wagering"
)

func TestNewRequestValid(t *testing.T) {
	t.Parallel()

	req := newRequest(t, sampleInput)
	if req.ProviderID() != "provider-a" || req.ExternalTransactionID() != "transaction-123" ||
		req.IdempotencyKey() != "provider-a:transaction-123" || req.PlayerID() != playerID ||
		req.WalletID() != walletID || req.RoundID() != "round-987" || req.GameID() != "fortune-chimp" ||
		req.Kind() != wagering.KindBet || req.Money().String() != "25.00 BRL" || req.ReferenceExternalTransactionID() != "" {
		t.Errorf("request does not reflect input: %+v", req)
	}
	if req.PayloadHash().IsZero() {
		t.Error("payload hash is zero")
	}
}

func TestNewRequestValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    wagering.RequestInput
		code  wagering.FailureCode
		field string
	}{
		{"missing provider", input(func(in *wagering.RequestInput) { in.ProviderID = "" }), wagering.CodeMissingField, "providerId"},
		{"missing external id", input(func(in *wagering.RequestInput) { in.ExternalTransactionID = "" }), wagering.CodeMissingField, "externalTransactionId"},
		{"missing idempotency key", input(func(in *wagering.RequestInput) { in.IdempotencyKey = "" }), wagering.CodeMissingField, "idempotencyKey"},
		{"missing round", input(func(in *wagering.RequestInput) { in.RoundID = "" }), wagering.CodeMissingField, "roundId"},
		{"missing game", input(func(in *wagering.RequestInput) { in.GameID = "" }), wagering.CodeMissingField, "gameId"},
		{"missing player", input(func(in *wagering.RequestInput) { in.PlayerID = "" }), wagering.CodeMissingField, "playerId"},
		{"missing wallet", input(func(in *wagering.RequestInput) { in.WalletID = "" }), wagering.CodeMissingField, "walletId"},
		{"missing kind", input(func(in *wagering.RequestInput) { in.Kind = "" }), wagering.CodeMissingField, "kind"},
		{"missing amount", input(func(in *wagering.RequestInput) { in.Amount = "" }), wagering.CodeMissingField, "money.amount"},
		{"missing currency", input(func(in *wagering.RequestInput) { in.Currency = "" }), wagering.CodeMissingField, "money.currency"},
		{"provider with space", input(func(in *wagering.RequestInput) { in.ProviderID = "provider a" }), wagering.CodeInvalidField, "providerId"},
		{"provider padded", input(func(in *wagering.RequestInput) { in.ProviderID = " provider-a" }), wagering.CodeInvalidField, "providerId"},
		{"round with control char", input(func(in *wagering.RequestInput) { in.RoundID = "round\n1" }), wagering.CodeInvalidField, "roundId"},
		{"game non ascii", input(func(in *wagering.RequestInput) { in.GameID = "fortune-🐒" }), wagering.CodeInvalidField, "gameId"},
		{"external id too long", input(func(in *wagering.RequestInput) { in.ExternalTransactionID = strings.Repeat("x", 129) }), wagering.CodeInvalidField, "externalTransactionId"},
		{"idempotency key too long", input(func(in *wagering.RequestInput) { in.IdempotencyKey = strings.Repeat("k", 256) }), wagering.CodeInvalidField, "idempotencyKey"},
		{"player not uuid", input(func(in *wagering.RequestInput) { in.PlayerID = "player-1" }), wagering.CodeInvalidField, "playerId"},
		{"player uppercase uuid", input(func(in *wagering.RequestInput) { in.PlayerID = strings.ToUpper(playerID.String()) }), wagering.CodeInvalidField, "playerId"},
		{"player braces uuid", input(func(in *wagering.RequestInput) { in.PlayerID = "{" + playerID.String() + "}" }), wagering.CodeInvalidField, "playerId"},
		{"wallet urn uuid", input(func(in *wagering.RequestInput) { in.WalletID = "urn:uuid:" + walletID.String() }), wagering.CodeInvalidField, "walletId"},
		{"wallet nil uuid", input(func(in *wagering.RequestInput) { in.WalletID = "00000000-0000-0000-0000-000000000000" }), wagering.CodeInvalidField, "walletId"},
		{"opening kind", input(func(in *wagering.RequestInput) { in.Kind = "OPENING" }), wagering.CodeOpeningNotAllowed, "kind"},
		{"unknown kind", input(func(in *wagering.RequestInput) { in.Kind = "JACKPOT" }), wagering.CodeUnsupportedKind, "kind"},
		{"lowercase kind", input(func(in *wagering.RequestInput) { in.Kind = "bet" }), wagering.CodeUnsupportedKind, "kind"},
		{"negative amount", input(func(in *wagering.RequestInput) { in.Amount = "-25.00" }), wagering.CodeInvalidAmount, "money.amount"},
		{"scientific amount", input(func(in *wagering.RequestInput) { in.Amount = "2.5e1" }), wagering.CodeInvalidAmount, "money.amount"},
		{"excess scale", input(func(in *wagering.RequestInput) { in.Amount = "25.001" }), wagering.CodeInvalidAmount, "money.amount"},
		{"NaN amount", input(func(in *wagering.RequestInput) { in.Amount = "NaN" }), wagering.CodeInvalidAmount, "money.amount"},
		{"Infinity amount", input(func(in *wagering.RequestInput) { in.Amount = "Infinity" }), wagering.CodeInvalidAmount, "money.amount"},
		{"overflow amount", input(func(in *wagering.RequestInput) { in.Amount = "92233720368547758.08" }), wagering.CodeInvalidAmount, "money.amount"},
		{"unsupported currency", input(func(in *wagering.RequestInput) { in.Currency = "XYZ" }), wagering.CodeInvalidCurrency, "money.currency"},
		{"refund without reference", op("REFUND", "tx-r", "25.00"), wagering.CodeReferenceRequired, "referenceExternalTransactionId"},
		{"rollback without reference", op("ROLLBACK", "tx-rb", "25.00"), wagering.CodeReferenceRequired, "referenceExternalTransactionId"},
		{"bet with reference", op("BET", "tx-b", "25.00", withReference("tx-a")), wagering.CodeReferenceNotAllowed, "referenceExternalTransactionId"},
		{"loss with reference", op("LOSS", "tx-l", "0.00", withReference("tx-a")), wagering.CodeReferenceNotAllowed, "referenceExternalTransactionId"},
		{"self reference", op("REFUND", "tx-r", "25.00", withReference("tx-r")), wagering.CodeSelfReference, "referenceExternalTransactionId"},
		{"invalid reference", op("REFUND", "tx-r", "25.00", withReference("tx a")), wagering.CodeInvalidField, "referenceExternalTransactionId"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := wagering.NewRequest(tc.in)
			var verr *wagering.ValidationError
			if !errors.As(err, &verr) || !errors.Is(err, wagering.ErrValidation) {
				t.Fatalf("error = %v, want *ValidationError", err)
			}
			if verr.Code != tc.code || verr.Field != tc.field {
				t.Errorf("got %s on %q, want %s on %q", verr.Code, verr.Field, tc.code, tc.field)
			}
			if verr.Code.Category() != wagering.CategoryCorrectable {
				t.Errorf("category = %s, want CORRECTABLE", verr.Code.Category())
			}
		})
	}
}

func TestZeroAmountPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind, amount string
		ref          string
		want         wagering.FailureCode
	}{
		{"BET", "0.00", "", wagering.CodeAmountMustBePositive},
		{"BET", "0.01", "", ""},
		{"WIN", "0.00", "", wagering.CodeAmountMustBePositive},
		{"WIN", "0.01", "", ""},
		{"REFUND", "0.00", "tx-ref", wagering.CodeAmountMustBePositive},
		{"REFUND", "0.01", "tx-ref", ""},
		{"ROLLBACK", "0.00", "tx-ref", wagering.CodeAmountMustBePositive},
		{"ROLLBACK", "0.01", "tx-ref", ""},
		{"LOSS", "0.00", "", ""},
		{"LOSS", "0.01", "", wagering.CodeLossAmountMustBeZero},
		{"LOSS", "25.00", "", wagering.CodeLossAmountMustBeZero},
	}
	for _, tc := range tests {
		t.Run(tc.kind+"/"+tc.amount, func(t *testing.T) {
			t.Parallel()
			in := op(tc.kind, "tx-zero", tc.amount)
			in.ReferenceExternalTransactionID = tc.ref
			_, err := wagering.NewRequest(in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr *wagering.ValidationError
			if !errors.As(err, &verr) || verr.Code != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestCanonicalPayloadGolden(t *testing.T) {
	t.Parallel()

	req := newRequest(t, sampleInput)
	wantJSON := `{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`
	if got := string(req.CanonicalPayload()); got != wantJSON {
		t.Fatalf("canonical payload\n got: %s\nwant: %s", got, wantJSON)
	}
	const wantHash = "629836932b79106b99523d06a1e7fa80689b0ea1e1c47aa3f0a5a2c87d0c4344"
	if got := req.PayloadHash().String(); got != wantHash {
		t.Errorf("payload hash = %s, want %s", got, wantHash)
	}
}

func TestCanonicalPayloadMatchesSortedJSONOracle(t *testing.T) {
	t.Parallel()

	inputs := []wagering.RequestInput{
		sampleInput,
		op("ROLLBACK", `quote"and\backslash`, "10.50", withReference("ref<&>")),
		op("LOSS", "loss-1", "0.00"),
		op("WIN", "win-1", "92233720368547758.07", withReference("bet-1")),
	}
	for _, in := range inputs {
		req := newRequest(t, in)
		fields := map[string]any{
			"providerId":            in.ProviderID,
			"externalTransactionId": in.ExternalTransactionID,
			"playerId":              in.PlayerID,
			"walletId":              in.WalletID,
			"roundId":               in.RoundID,
			"gameId":                in.GameID,
			"kind":                  in.Kind,
			"money":                 map[string]string{"amount": in.Amount, "currency": in.Currency},
		}
		if in.ReferenceExternalTransactionID != "" {
			fields["referenceExternalTransactionId"] = in.ReferenceExternalTransactionID
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(fields); err != nil {
			t.Fatal(err)
		}
		oracle := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

		if got := req.CanonicalPayload(); !bytes.Equal(got, oracle) {
			t.Errorf("canonical payload differs from sorted JSON oracle\n got: %s\nwant: %s", got, oracle)
		}
		if req.PayloadHash() != sha256.Sum256(oracle) {
			t.Errorf("hash differs from sha256(oracle) for %s", in.ExternalTransactionID)
		}
	}
}

func TestPayloadHashIgnoresTransportFields(t *testing.T) {
	t.Parallel()

	base := newRequest(t, sampleInput)
	otherKey := newRequest(t, input(func(in *wagering.RequestInput) { in.IdempotencyKey = "a-completely-different-key" }))
	if base.PayloadHash() != otherKey.PayloadHash() {
		t.Error("idempotency key must not affect the payload hash")
	}
}

func TestPayloadHashDetectsBusinessChanges(t *testing.T) {
	t.Parallel()

	base := newRequest(t, sampleInput).PayloadHash()
	mutations := map[string]func(*wagering.RequestInput){
		"provider":  func(in *wagering.RequestInput) { in.ProviderID = "provider-b" },
		"external":  func(in *wagering.RequestInput) { in.ExternalTransactionID = "transaction-124" },
		"player":    func(in *wagering.RequestInput) { in.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" },
		"wallet":    func(in *wagering.RequestInput) { in.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" },
		"round":     func(in *wagering.RequestInput) { in.RoundID = "round-988" },
		"game":      func(in *wagering.RequestInput) { in.GameID = "other-game" },
		"kind":      func(in *wagering.RequestInput) { in.Kind = "WIN" },
		"amount":    func(in *wagering.RequestInput) { in.Amount = "25.01" },
		"currency":  func(in *wagering.RequestInput) { in.Currency = "USD" },
		"reference": func(in *wagering.RequestInput) { in.Kind = "WIN"; in.ReferenceExternalTransactionID = "bet-1" },
	}
	seen := map[wagering.PayloadHash]string{base: "base"}
	for name, mutate := range mutations {
		h := newRequest(t, input(mutate)).PayloadHash()
		if prev, dup := seen[h]; dup {
			t.Errorf("mutation %q collides with %q", name, prev)
		}
		seen[h] = name
	}
}

func TestPayloadHashFromBytes(t *testing.T) {
	t.Parallel()

	h := newRequest(t, sampleInput).PayloadHash()
	back, err := wagering.PayloadHashFromBytes(h.Bytes())
	if err != nil || back != h {
		t.Errorf("round trip = %s, %v", back, err)
	}
	if _, err := wagering.PayloadHashFromBytes([]byte{1, 2, 3}); !errors.Is(err, wagering.ErrInvalidState) {
		t.Errorf("short hash error = %v", err)
	}
}
