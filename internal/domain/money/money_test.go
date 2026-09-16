package money_test

import (
	"errors"
	"math"
	"testing"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

func mustParse(t *testing.T, amount, currency string) money.Money {
	t.Helper()
	m, err := money.ParseSigned(amount, currency)
	if err != nil {
		t.Fatalf("ParseSigned(%q, %q): %v", amount, currency, err)
	}
	return m
}

func mustNew(t *testing.T, minor int64, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.New(minor, currency)
	if err != nil {
		t.Fatalf("New(%d, %s): %v", minor, currency, err)
	}
	return m
}

func TestParse(t *testing.T) {
	t.Parallel()

	valid := []struct {
		in    string
		minor int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.10", 10},
		{"1.00", 100},
		{"25.00", 2500},
		{"1000.00", 100000},
		{"92233720368547758.07", math.MaxInt64},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.in, func(t *testing.T) {
			t.Parallel()
			m, err := money.Parse(tc.in, "BRL")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.Minor() != tc.minor {
				t.Errorf("minor = %d, want %d", m.Minor(), tc.minor)
			}
			if m.Currency() != money.BRL {
				t.Errorf("currency = %s, want BRL", m.Currency())
			}
			if m.Amount() != tc.in {
				t.Errorf("Amount() = %q, want canonical round-trip %q", m.Amount(), tc.in)
			}
		})
	}

	invalid := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", money.ErrInvalidAmount},
		{"no decimals", "25", money.ErrInvalidAmount},
		{"one decimal", "25.0", money.ErrInvalidAmount},
		{"excess scale", "25.000", money.ErrInvalidAmount},
		{"excess scale non zero", "0.001", money.ErrInvalidAmount},
		{"missing integer", ".50", money.ErrInvalidAmount},
		{"trailing dot", "25.", money.ErrInvalidAmount},
		{"leading zero", "025.00", money.ErrInvalidAmount},
		{"double zero", "00.00", money.ErrInvalidAmount},
		{"plus sign", "+25.00", money.ErrInvalidAmount},
		{"negative", "-25.00", money.ErrNegativeAmount},
		{"negative zero", "-0.00", money.ErrNegativeAmount},
		{"comma separator", "25,00", money.ErrInvalidAmount},
		{"thousands separator", "1,000.00", money.ErrInvalidAmount},
		{"scientific", "2.5e1", money.ErrInvalidAmount},
		{"scientific with scale", "1e2.00", money.ErrInvalidAmount},
		{"NaN", "NaN", money.ErrInvalidAmount},
		{"Infinity", "Infinity", money.ErrInvalidAmount},
		{"inf", "inf", money.ErrInvalidAmount},
		{"leading space", " 25.00", money.ErrInvalidAmount},
		{"trailing space", "25.00 ", money.ErrInvalidAmount},
		{"hex", "0x19.00", money.ErrInvalidAmount},
		{"unicode digit", "٢٥.00", money.ErrInvalidAmount},
		{"two dots", "1.2.00", money.ErrInvalidAmount},
		{"overflow by one cent", "92233720368547758.08", money.ErrOverflow},
		{"overflow large", "99999999999999999999.99", money.ErrOverflow},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := money.Parse(tc.in, "BRL")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse(%q) error = %v, want %v", tc.in, err, tc.want)
			}
			if m.IsValid() {
				t.Errorf("Parse(%q) returned a valid value alongside an error", tc.in)
			}
		})
	}
}

func TestParseSigned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in    string
		minor int64
		want  error
	}{
		{"-0.01", -1, nil},
		{"-25.00", -2500, nil},
		{"-92233720368547758.08", math.MinInt64, nil},
		{"-92233720368547758.09", 0, money.ErrOverflow},
		{"-0.00", 0, money.ErrInvalidAmount},
		{"--1.00", 0, money.ErrInvalidAmount},
		{"-", 0, money.ErrInvalidAmount},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			m, err := money.ParseSigned(tc.in, "BRL")
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.want == nil {
				if m.Minor() != tc.minor {
					t.Errorf("minor = %d, want %d", m.Minor(), tc.minor)
				}
				if m.Amount() != tc.in {
					t.Errorf("Amount() = %q, want %q", m.Amount(), tc.in)
				}
			}
		})
	}
}

func TestParseCurrency(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"BRL", "USD", "EUR"} {
		c, err := money.ParseCurrency(code)
		if err != nil {
			t.Errorf("ParseCurrency(%q): %v", code, err)
		}
		if c.Code() != code {
			t.Errorf("Code() = %q, want %q", c.Code(), code)
		}
	}
	for _, code := range []string{"", "brl", "BR", "BRLL", "XXX", "JPY", " BRL"} {
		if _, err := money.ParseCurrency(code); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) error = %v, want ErrInvalidCurrency", code, err)
		}
		if _, err := money.Parse("1.00", code); !errors.Is(err, money.ErrInvalidCurrency) {
			t.Errorf("Parse with currency %q error = %v, want ErrInvalidCurrency", code, err)
		}
	}
}

func TestZeroAndNew(t *testing.T) {
	t.Parallel()

	z, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsZero() || !z.IsValid() || z.Amount() != "0.00" || z.Currency() != money.BRL {
		t.Errorf("Zero(BRL) = %v", z)
	}

	if _, err := money.Zero(money.Currency{}); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("Zero(zero currency) error = %v, want ErrUninitialized", err)
	}
	if _, err := money.New(100, money.Currency{}); !errors.Is(err, money.ErrUninitialized) {
		t.Errorf("New(zero currency) error = %v, want ErrUninitialized", err)
	}

	n := mustNew(t, -150, money.USD)
	if n.Amount() != "-1.50" || !n.IsNegative() || n.IsPositive() {
		t.Errorf("New(-150, USD) = %v", n)
	}
}

func TestArithmetic(t *testing.T) {
	t.Parallel()

	a := mustParse(t, "100.00", "BRL")
	b := mustParse(t, "80.00", "BRL")

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "180.00" {
		t.Errorf("Add = %v, %v; want 180.00", sum, err)
	}
	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != "20.00" {
		t.Errorf("Sub = %v, %v; want 20.00", diff, err)
	}
	negDiff, err := b.Sub(a)
	if err != nil || negDiff.Amount() != "-20.00" || !negDiff.IsNegative() {
		t.Errorf("Sub = %v, %v; want -20.00", negDiff, err)
	}
	neg, err := a.Neg()
	if err != nil || neg.Amount() != "-100.00" {
		t.Errorf("Neg = %v, %v; want -100.00", neg, err)
	}
	back, err := neg.Neg()
	if err != nil || !back.Equal(a) {
		t.Errorf("Neg(Neg(a)) = %v, %v; want %v", back, err, a)
	}

	if a.Amount() != "100.00" || b.Amount() != "80.00" {
		t.Errorf("operands mutated: a=%v b=%v", a, b)
	}
}

func TestOverflow(t *testing.T) {
	t.Parallel()

	maxM := mustNew(t, math.MaxInt64, money.BRL)
	minM := mustNew(t, math.MinInt64, money.BRL)
	one := mustNew(t, 1, money.BRL)
	minusOne := mustNew(t, -1, money.BRL)

	tests := []struct {
		name string
		op   func() (money.Money, error)
	}{
		{"max + 1", func() (money.Money, error) { return maxM.Add(one) }},
		{"min + -1", func() (money.Money, error) { return minM.Add(minusOne) }},
		{"max + max", func() (money.Money, error) { return maxM.Add(maxM) }},
		{"min - 1", func() (money.Money, error) { return minM.Sub(one) }},
		{"max - -1", func() (money.Money, error) { return maxM.Sub(minusOne) }},
		{"0 - min", func() (money.Money, error) { return mustNew(t, 0, money.BRL).Sub(minM) }},
		{"-min", minM.Neg},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.op(); !errors.Is(err, money.ErrOverflow) {
				t.Errorf("error = %v, want ErrOverflow", err)
			}
		})
	}

	if got, err := maxM.Add(minusOne); err != nil || got.Minor() != math.MaxInt64-1 {
		t.Errorf("max + -1 = %v, %v", got, err)
	}
	if got, err := minM.Sub(minusOne); err != nil || got.Minor() != math.MinInt64+1 {
		t.Errorf("min - -1 = %v, %v", got, err)
	}
	if got, err := maxM.Neg(); err != nil || got.Minor() != -math.MaxInt64 {
		t.Errorf("-max = %v, %v", got, err)
	}
	if got := minM.Amount(); got != "-92233720368547758.08" {
		t.Errorf("min Amount() = %q", got)
	}
}

func TestCurrencyMismatch(t *testing.T) {
	t.Parallel()

	brl := mustParse(t, "10.00", "BRL")
	usd := mustParse(t, "10.00", "USD")

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Cmp error = %v, want ErrCurrencyMismatch", err)
	}
	if brl.Equal(usd) {
		t.Error("Equal across currencies = true, want false")
	}
}

func TestUninitialized(t *testing.T) {
	t.Parallel()

	var zero money.Money
	valid := mustParse(t, "1.00", "BRL")

	if zero.IsValid() {
		t.Error("zero value IsValid() = true")
	}
	checks := map[string]error{}
	_, checks["Add"] = valid.Add(zero)
	_, checks["zero.Add"] = zero.Add(valid)
	_, checks["Sub"] = valid.Sub(zero)
	_, checks["Neg"] = zero.Neg()
	_, checks["Cmp"] = zero.Cmp(valid)
	_, checks["MarshalJSON"] = zero.MarshalJSON()
	for op, err := range checks {
		if !errors.Is(err, money.ErrUninitialized) {
			t.Errorf("%s error = %v, want ErrUninitialized", op, err)
		}
	}
	if zero.String() != "<invalid money>" {
		t.Errorf("String() = %q", zero.String())
	}
}

func TestCmp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{"1.00", "2.00", -1},
		{"2.00", "1.00", 1},
		{"2.00", "2.00", 0},
		{"-1.00", "0.00", -1},
		{"0.01", "0.00", 1},
	}
	for _, tc := range tests {
		got, err := mustParse(t, tc.a, "BRL").Cmp(mustParse(t, tc.b, "BRL"))
		if err != nil || got != tc.want {
			t.Errorf("Cmp(%s, %s) = %d, %v; want %d", tc.a, tc.b, got, err, tc.want)
		}
	}
}

func TestPredicates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in                     string
		zero, positive, negate bool
	}{
		{"0.00", true, false, false},
		{"0.01", false, true, false},
		{"-0.01", false, false, true},
	}
	for _, tc := range tests {
		m := mustParse(t, tc.in, "EUR")
		if m.IsZero() != tc.zero || m.IsPositive() != tc.positive || m.IsNegative() != tc.negate {
			t.Errorf("%s: IsZero=%v IsPositive=%v IsNegative=%v", tc.in, m.IsZero(), m.IsPositive(), m.IsNegative())
		}
	}
	if got := mustParse(t, "25.00", "BRL").String(); got != "25.00 BRL" {
		t.Errorf("String() = %q", got)
	}
}
