package money_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

func TestMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"25.00":  `{"amount":"25.00","currency":"BRL"}`,
		"0.00":   `{"amount":"0.00","currency":"BRL"}`,
		"-20.00": `{"amount":"-20.00","currency":"BRL"}`,
	}
	for amount, want := range tests {
		got, err := json.Marshal(mustParse(t, amount, "BRL"))
		if err != nil {
			t.Fatalf("Marshal(%s): %v", amount, err)
		}
		if string(got) != want {
			t.Errorf("Marshal(%s) = %s, want %s", amount, got, want)
		}
	}
}

func TestUnmarshalJSON(t *testing.T) {
	t.Parallel()

	var m money.Money
	if err := json.Unmarshal([]byte(`{"amount":"975.00","currency":"BRL"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Minor() != 97500 || m.Currency() != money.BRL {
		t.Errorf("Unmarshal = %v", m)
	}

	var neg money.Money
	if err := json.Unmarshal([]byte(`{"amount":"-0.50","currency":"USD"}`), &neg); err != nil {
		t.Fatal(err)
	}
	if neg.Minor() != -50 {
		t.Errorf("Unmarshal negative = %v", neg)
	}

	invalid := []struct {
		name string
		in   string
		want error
	}{
		{"number amount", `{"amount":25.00,"currency":"BRL"}`, money.ErrInvalidAmount},
		{"integer amount", `{"amount":25,"currency":"BRL"}`, money.ErrInvalidAmount},
		{"unknown field", `{"amount":"25.00","currency":"BRL","scale":2}`, money.ErrInvalidAmount},
		{"missing amount", `{"currency":"BRL"}`, money.ErrInvalidAmount},
		{"missing currency", `{"amount":"25.00"}`, money.ErrInvalidCurrency},
		{"lowercase currency", `{"amount":"25.00","currency":"brl"}`, money.ErrInvalidCurrency},
		{"scientific", `{"amount":"2.5e1","currency":"BRL"}`, money.ErrInvalidAmount},
		{"excess scale", `{"amount":"25.001","currency":"BRL"}`, money.ErrInvalidAmount},
		{"null amount", `{"amount":null,"currency":"BRL"}`, money.ErrInvalidAmount},
		{"array", `["25.00","BRL"]`, money.ErrInvalidAmount},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got money.Money
			err := json.Unmarshal([]byte(tc.in), &got)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Unmarshal(%s) error = %v, want %v", tc.in, err, tc.want)
			}
			if got.IsValid() {
				t.Errorf("target modified on error: %v", got)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	t.Parallel()

	for _, amount := range []string{"0.00", "0.01", "-0.01", "92233720368547758.07", "-92233720368547758.08"} {
		orig := mustParse(t, amount, "EUR")
		data, err := json.Marshal(orig)
		if err != nil {
			t.Fatal(err)
		}
		var back money.Money
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if !back.Equal(orig) {
			t.Errorf("round trip %s = %v", amount, back)
		}
	}
}
