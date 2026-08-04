package domain

import (
	"encoding/json"
	"testing"
)

func TestMoneyString(t *testing.T) {
	tests := []struct {
		in   Money
		want string
	}{
		{0, "$0.00"},
		{5, "$0.05"},
		{99, "$0.99"},
		{100, "$1.00"},
		{-1820, "-$18.20"},
		{123456, "$1,234.56"},
		{100000000, "$1,000,000.00"},
		{-100000000, "-$1,000,000.00"},
		{999, "$9.99"},
		{1000, "$10.00"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Money(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
		}
	}
}

func TestParseMoney(t *testing.T) {
	tests := []struct {
		in   string
		want Money
	}{
		{"0", 0},
		{"1", 100},
		{"1.5", 150},
		{"1.05", 105},
		{"$1,234.56", 123456},
		{" -18.20 ", -1820},
		{"+4.00", 400},
		{".99", 99},
		{"1000000", 100000000},
	}
	for _, tt := range tests {
		got, err := ParseMoney(tt.in)
		if err != nil {
			t.Errorf("ParseMoney(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseMoney(%q) = %d, want %d", tt.in, int64(got), int64(tt.want))
		}
	}
}

func TestParseMoneyRejects(t *testing.T) {
	// Three decimal places means the input was not an amount of money, and
	// silently truncating it is how a ledger starts lying.
	for _, in := range []string{"", "  ", "$", "12.345", "twelve", "1.2.3"} {
		if got, err := ParseMoney(in); err == nil {
			t.Errorf("ParseMoney(%q) = %d, want an error", in, int64(got))
		}
	}
}

func TestMoneyRoundTripsThroughParse(t *testing.T) {
	for _, m := range []Money{0, 1, -1, 99, 123456, -123456, 100000000} {
		got, err := ParseMoney(m.String())
		if err != nil {
			t.Errorf("ParseMoney(%q): %v", m.String(), err)
			continue
		}
		if got != m {
			t.Errorf("round trip of %d gave %d", int64(m), int64(got))
		}
	}
}

func TestMoneyJSONIsIntegerCents(t *testing.T) {
	b, err := json.Marshal(struct {
		Amount Money `json:"amount_cents"`
	}{-1820})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"amount_cents":-1820}`; got != want {
		t.Errorf("marshalled to %s, want %s", got, want)
	}

	var out struct {
		Amount Money `json:"amount_cents"`
	}
	if err := json.Unmarshal([]byte(`{"amount_cents":48219}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Amount != 48219 {
		t.Errorf("unmarshalled to %d, want 48219", int64(out.Amount))
	}

	// A decimal amount on the wire is a client bug, and it fails loudly
	// rather than silently truncating to 482 cents.
	if err := json.Unmarshal([]byte(`{"amount_cents":482.19}`), &out); err == nil {
		t.Error("unmarshalling a decimal amount succeeded, want an error")
	}
}

func TestMoneyArithmetic(t *testing.T) {
	rent, fee := Money(185000), Money(-14800)

	if got, want := rent.Add(fee), Money(170200); got != want {
		t.Errorf("Add = %d, want %d", int64(got), int64(want))
	}
	if got, want := rent.Sub(rent), Money(0); got != want || !got.IsZero() {
		t.Errorf("Sub = %d, want 0", int64(got))
	}
	if got, want := fee.Abs(), Money(14800); got != want {
		t.Errorf("Abs = %d, want %d", int64(got), int64(want))
	}
	if got, want := fee.Neg(), Money(14800); got != want {
		t.Errorf("Neg = %d, want %d", int64(got), int64(want))
	}
	if got, want := rent.Cents(), int64(185000); got != want {
		t.Errorf("Cents = %d, want %d", got, want)
	}
}
