package domain

import (
	"math"
	"strings"
)

// Currency is an ISO 4217 alphabetic currency code, e.g. "USD".
//
// Amounts are always int64 values in the currency's minor unit (cents for USD),
// so no floating point arithmetic is ever involved.
type Currency string

// ParseCurrency normalises s to upper case and validates its shape.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if !c.Valid() {
		return "", ErrInvalidCurrency
	}
	return c, nil
}

// Valid reports whether c is three upper-case ASCII letters.
func (c Currency) Valid() bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// addAmount returns a+b or ErrBalanceOverflow if the result does not fit in an int64.
func addAmount(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrBalanceOverflow
	}
	return a + b, nil
}
