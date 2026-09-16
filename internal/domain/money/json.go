package money

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type wire struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, ErrUninitialized
	}
	data, err := json.Marshal(wire{Amount: m.Amount(), Currency: m.currency.code})
	if err != nil {
		return nil, fmt.Errorf("money: marshal: %w", err)
	}
	return data, nil
}

func (m *Money) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAmount, err)
	}
	parsed, err := ParseSigned(w.Amount, w.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
