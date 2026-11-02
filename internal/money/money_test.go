package money_test

import (
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/xJohnNascimento/corridor/internal/money"
)

func TestParseAcceptsOnlyCanonicalPositiveMinorUnits(t *testing.T) {
	for _, s := range []string{"1", "100", "9223372036854775807"} {
		if _, err := money.ParseAmount(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "0", "-1", "01", "1.0", "1e3", " 1", "9223372036854775808"} {
		if _, err := money.ParseAmount(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestCurrencyNeedsThreeCapitals(t *testing.T) {
	for s, ok := range map[string]bool{"USD": true, "usd": false, "US": false, "USDT": false, "": false} {
		if _, err := money.ParseCurrency(s); (err == nil) != ok {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestMulDivMatchesBigIntegers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.Int64Range(0, 1<<62).Draw(t, "a")
		b := rapid.Int64Range(0, 1<<40).Draw(t, "b")
		c := rapid.Int64Range(1, 1<<40).Draw(t, "c")
		product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
		floor, rem := new(big.Int).QuoRem(product, big.NewInt(c), new(big.Int))
		ceil := new(big.Int).Set(floor)
		if rem.Sign() > 0 {
			ceil.Add(ceil, big.NewInt(1))
		}
		gotFloor, errFloor := money.MulDivFloor(a, b, c)
		gotCeil, errCeil := money.MulDivCeil(a, b, c)
		if floor.IsInt64() != (errFloor == nil) || ceil.IsInt64() != (errCeil == nil) {
			t.Fatalf("overflow detection: %v %v for %d×%d/%d", errFloor, errCeil, a, b, c)
		}
		if errFloor == nil && gotFloor != floor.Int64() || errCeil == nil && gotCeil != ceil.Int64() {
			t.Fatalf("%d×%d/%d: floor %d (want %s), ceil %d (want %s)", a, b, c, gotFloor, floor, gotCeil, ceil)
		}
	})
}

func TestMulDivRefusesNegativeOperands(t *testing.T) {
	if _, err := money.MulDivFloor(-1, 1, 1); err == nil {
		t.Fatal("a negative operand was accepted")
	}
	if _, err := money.MulDivFloor(1, 1, 0); err == nil {
		t.Fatal("a zero divisor was accepted")
	}
}
