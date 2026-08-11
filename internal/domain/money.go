// Package domain holds the entities and value types shared across the
// application, independent of how they are stored or served.
package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in whole cents.
//
// Every monetary value in this system is an integer number of cents, in the
// database and on the wire alike (docs/DESIGN.md §3). Floats are never used:
// a rent roll that drifts by a cent per row is a bug nobody notices until the
// year-end totals disagree with the bank.
//
// Sign follows the ledger convention: income is positive, expense negative.
type Money int64

// Cents returns the underlying amount.
func (m Money) Cents() int64 { return int64(m) }

// Add returns m + n.
func (m Money) Add(n Money) Money { return m + n }

// Sub returns m - n.
func (m Money) Sub(n Money) Money { return m - n }

// Neg returns -m.
func (m Money) Neg() Money { return -m }

// Abs returns the magnitude of m.
func (m Money) Abs() Money {
	if m < 0 {
		return -m
	}
	return m
}

// IsZero reports whether m is exactly zero.
func (m Money) IsZero() bool { return m == 0 }

// String renders the amount for display: "$1,234.56", "-$18.20", "$0.00".
func (m Money) String() string {
	sign := ""
	n := int64(m)
	if n < 0 {
		sign = "-"
		n = -n
	}

	cents := n % 100
	whole := strconv.FormatInt(n/100, 10)
	return sign + "$" + group(whole) + "." +
		string([]byte{byte('0' + cents/10), byte('0' + cents%10)})
}

// group inserts thousands separators into a run of digits.
func group(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// ParseMoney reads a human-entered amount — "1,234.56", "$18.20", "-4" — and
// returns it in cents. It accepts at most two decimal places, because a third
// one means the input was not really an amount of money.
func ParseMoney(s string) (Money, error) {
	orig := s
	s = strings.TrimSpace(s)
	s = strings.NewReplacer(",", "", "$", "", " ", "").Replace(s)
	if s == "" {
		return 0, fmt.Errorf("parse money %q: no digits", orig)
	}

	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if hasFrac {
		if len(frac) > 2 {
			return 0, fmt.Errorf("parse money %q: more than two decimal places", orig)
		}
		frac += strings.Repeat("0", 2-len(frac))
	} else {
		frac = "00"
	}

	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse money %q: %w", orig, err)
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse money %q: %w", orig, err)
	}

	total := Money(dollars*100 + cents)
	if neg {
		total = -total
	}
	return total, nil
}

// MarshalJSON writes the amount as an integer number of cents.
//
// The API never sends a decimal amount. A client that receives 48219 cannot
// misread it; a client that receives 482.19 eventually will.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(m))
}

// UnmarshalJSON reads an integer number of cents.
func (m *Money) UnmarshalJSON(b []byte) error {
	var cents int64
	if err := json.Unmarshal(b, &cents); err != nil {
		return fmt.Errorf("money must be an integer number of cents: %w", err)
	}
	*m = Money(cents)
	return nil
}
