package jobs

import (
	"testing"
	"time"
)

// backoff runs once per failed job and is pure arithmetic; this exists so a
// change to how the schedule is computed shows up as a number.
func BenchmarkBackoff(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for attempts := int64(1); attempts <= 6; attempts++ {
			wait = backoff(attempts)
		}
	}
}

var wait time.Duration
