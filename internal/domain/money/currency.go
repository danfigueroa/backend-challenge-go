package money

import "fmt"

const Scale = 2

type Currency struct {
	code string
}

var (
	BRL = Currency{code: "BRL"}
	USD = Currency{code: "USD"}
	EUR = Currency{code: "EUR"}
)

var supportedCurrencies = map[string]Currency{
	BRL.code: BRL,
	USD.code: USD,
	EUR.code: EUR,
}

func ParseCurrency(code string) (Currency, error) {
	c, ok := supportedCurrencies[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	return c, nil
}

func (c Currency) Code() string { return c.code }

func (c Currency) String() string { return c.code }

func (c Currency) IsZero() bool { return c.code == "" }
