package alert

import "testing"

// BenchmarkPublishSuppressed measures the branch that runs almost every time: a
// condition that is already open and still inside its cooldown. That path is one
// read and a timestamp comparison, and §8.3 means it to stay that cheap -- a
// flapping condition publishes as often as it flaps, and only the cooldown
// stands between that and an alert storm.
func BenchmarkPublishSuppressed(b *testing.B) {
	bus, _ := openBus(b)
	ctx := b.Context()

	// The first publish opens the row and records a send; every one after it
	// takes the suppressed path.
	bus.Publish(ctx, condition())

	b.ReportAllocs()
	for b.Loop() {
		bus.Publish(ctx, condition())
	}
}
