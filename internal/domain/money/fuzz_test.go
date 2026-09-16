package money_test

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"strings"
	"testing"

	"github.com/danfigueroa/backend-challenge-go/internal/domain/money"
)

var strictSigned = regexp.MustCompile(`^-?(0|[1-9][0-9]*)\.[0-9]{2}$`)

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"0.00", "25.00", "-25.00", "-0.00", "25", "25.0", "025.00", "2.5e1", "NaN",
		"Infinity", "", ".00", "92233720368547758.07", "92233720368547758.08",
		"-92233720368547758.08", "-92233720368547758.09", "1,00", "+1.00",
	} {
		f.Add(seed)
	}

	maxInt := big.NewInt(math.MaxInt64)
	minInt := big.NewInt(math.MinInt64)

	f.Fuzz(func(t *testing.T, in string) {
		m, err := money.ParseSigned(in, "BRL")

		matches := in != "-0.00" && strictSigned.MatchString(in)
		var inRange bool
		var want *big.Int
		if matches {
			digits := strings.Replace(in, ".", "", 1)
			want, _ = new(big.Int).SetString(digits, 10)
			inRange = want.Cmp(maxInt) <= 0 && want.Cmp(minInt) >= 0
		}

		switch {
		case err == nil:
			if !matches || !inRange {
				t.Fatalf("accepted %q outside grammar or range", in)
			}
			if m.Minor() != want.Int64() {
				t.Fatalf("ParseSigned(%q) = %d, want %s", in, m.Minor(), want)
			}
			if m.Amount() != in {
				t.Fatalf("Amount() = %q, want %q", m.Amount(), in)
			}
		case errors.Is(err, money.ErrOverflow):
			if !matches || inRange {
				t.Fatalf("overflow reported for %q (matches=%v inRange=%v)", in, matches, inRange)
			}
		case errors.Is(err, money.ErrInvalidAmount):
			if matches {
				t.Fatalf("rejected valid %q: %v", in, err)
			}
		default:
			t.Fatalf("unclassified error for %q: %v", in, err)
		}

		if len(in) > 0 && in[0] != '-' {
			p, perr := money.Parse(in, "BRL")
			if (perr == nil) != (err == nil) || (err == nil && !p.Equal(m)) {
				t.Fatalf("Parse and ParseSigned disagree on %q: %v vs %v", in, perr, err)
			}
		}
	})
}
