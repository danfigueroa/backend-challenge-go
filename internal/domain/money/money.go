package money

import (
	"fmt"
	"math"
)

const minorPerUnit = 100

type Money struct {
	minor    int64
	currency Currency
}

func New(minor int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, fmt.Errorf("%w: currency", ErrUninitialized)
	}
	return Money{minor: minor, currency: currency}, nil
}

func Zero(currency Currency) (Money, error) {
	return New(0, currency)
}

func Parse(amount, currencyCode string) (Money, error) {
	if len(amount) > 0 && amount[0] == '-' {
		return Money{}, fmt.Errorf("%w: %q", ErrNegativeAmount, amount)
	}
	return parse(amount, currencyCode)
}

func ParseSigned(amount, currencyCode string) (Money, error) {
	return parse(amount, currencyCode)
}

func parse(amount, currencyCode string) (Money, error) {
	currency, err := ParseCurrency(currencyCode)
	if err != nil {
		return Money{}, err
	}
	minor, err := parseMinor(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: currency}, nil
}

func parseMinor(s string) (int64, error) {
	negative := len(s) > 0 && s[0] == '-'
	digits := s
	if negative {
		digits = s[1:]
	}

	dot := len(digits) - Scale - 1
	if dot < 1 || digits[dot] != '.' {
		return 0, invalidAmount(s)
	}
	integer, fraction := digits[:dot], digits[dot+1:]
	if len(integer) > 1 && integer[0] == '0' || !isDigits(integer) || !isDigits(fraction) {
		return 0, invalidAmount(s)
	}

	var accumulated int64
	for _, part := range [...]string{integer, fraction} {
		for i := range len(part) {
			digit := int64(part[i] - '0')
			if accumulated < (math.MinInt64+digit)/10 {
				return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
			}
			accumulated = accumulated*10 - digit
		}
	}

	switch {
	case negative && accumulated == 0:
		return 0, invalidAmount(s)
	case negative:
		return accumulated, nil
	case accumulated == math.MinInt64:
		return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
	default:
		return -accumulated, nil
	}
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func invalidAmount(s string) error {
	return fmt.Errorf("%w: %q", ErrInvalidAmount, s)
}

func (m Money) Minor() int64 { return m.minor }

func (m Money) Currency() Currency { return m.currency }

func (m Money) IsValid() bool { return !m.currency.IsZero() }

func (m Money) IsZero() bool { return m.minor == 0 }

func (m Money) IsPositive() bool { return m.minor > 0 }

func (m Money) IsNegative() bool { return m.minor < 0 }

func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if (other.minor > 0 && m.minor > math.MaxInt64-other.minor) ||
		(other.minor < 0 && m.minor < math.MinInt64-other.minor) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrOverflow, m, other)
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

func (m Money) Sub(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if (other.minor < 0 && m.minor > math.MaxInt64+other.minor) ||
		(other.minor > 0 && m.minor < math.MinInt64+other.minor) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrOverflow, m, other)
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, ErrUninitialized
	}
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrOverflow, m)
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

func (m Money) Cmp(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) Equal(other Money) bool {
	return m.currency == other.currency && m.minor == other.minor
}

func (m Money) Amount() string {
	units, cents := m.minor/minorPerUnit, m.minor%minorPerUnit
	if m.minor < 0 {
		return fmt.Sprintf("-%d.%02d", -units, -cents)
	}
	return fmt.Sprintf("%d.%02d", units, cents)
}

func (m Money) String() string {
	if !m.IsValid() {
		return "<invalid money>"
	}
	return m.Amount() + " " + m.currency.code
}

func (m Money) compatible(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return ErrUninitialized
	}
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return nil
}
