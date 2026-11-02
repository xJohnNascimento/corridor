package money

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"regexp"
	"strconv"
)

const RateScale = 100_000_000

type Currency string

var (
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	amountPattern   = regexp.MustCompile(`^[1-9][0-9]*$`)
	ErrOverflow     = errors.New("result exceeds 64 bits")
)

func ParseCurrency(s string) (Currency, error) {
	if !currencyPattern.MatchString(s) {
		return "", fmt.Errorf("currency %q must be three capital letters", s)
	}
	return Currency(s), nil
}

// ParseAmount accepts a positive amount in minor units, written as a canonical decimal integer.
func ParseAmount(s string) (int64, error) {
	if !amountPattern.MatchString(s) {
		return 0, fmt.Errorf("amount %q must be a positive integer in minor units", s)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", s, ErrOverflow)
	}
	return v, nil
}

func Format(v int64) string {
	return strconv.FormatInt(v, 10)
}

// MulDivFloor returns floor(a × b / c) with a 128-bit intermediate product.
func MulDivFloor(a, b, c int64) (int64, error) {
	q, _, err := mulDiv(a, b, c)
	return q, err
}

// MulDivCeil returns ceil(a × b / c) with a 128-bit intermediate product.
func MulDivCeil(a, b, c int64) (int64, error) {
	q, r, err := mulDiv(a, b, c)
	if err != nil || r == 0 {
		return q, err
	}
	if q == math.MaxInt64 {
		return 0, ErrOverflow
	}
	return q + 1, nil
}

func mulDiv(a, b, c int64) (int64, uint64, error) {
	if a < 0 || b < 0 || c <= 0 {
		return 0, 0, fmt.Errorf("mulDiv(%d, %d, %d) needs non-negative operands and a positive divisor", a, b, c)
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(c) {
		return 0, 0, ErrOverflow
	}
	q, r := bits.Div64(hi, lo, uint64(c))
	if q > math.MaxInt64 {
		return 0, 0, ErrOverflow
	}
	return int64(q), r, nil
}
