package domain

import "testing"

// The benchmarks in this file guard the value types against a refactor that
// makes them slower. Money.String runs once per amount on every ledger render
// and NormalizeAddress runs on every property write and on every ingested
// address, so both are worth a number rather than an assumption.

func BenchmarkNormalizeAddress(b *testing.B) {
	cases := []struct {
		name                              string
		line1, line2, city, state, postal string
	}{
		{"house", "412 Elm Street", "", "Athens", "Ohio", "45701"},
		{"unit", "88 North Oak Avenue Southwest", "Apt 2B", "Columbus", "OH", "43215-1234"},
		{"folded", "412 ELM ST ATHENS OH 45701", "", "", "", ""},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				sink = NormalizeAddress(c.line1, c.line2, c.city, c.state, c.postal)
			}
		})
	}
}

func BenchmarkMoneyString(b *testing.B) {
	amounts := []Money{0, 1820, -1820, 185000, 123456789, -9}
	b.ReportAllocs()
	for b.Loop() {
		for _, m := range amounts {
			sink = m.String()
		}
	}
}

func BenchmarkParseMoney(b *testing.B) {
	inputs := []string{"1,234.56", "$18.20", "-4", "0.07", "  985 "}
	b.ReportAllocs()
	for b.Loop() {
		for _, in := range inputs {
			money, _ = ParseMoney(in)
		}
	}
}

// sink and money keep the compiler from eliding the work being measured.
var (
	sink  string
	money Money
)
