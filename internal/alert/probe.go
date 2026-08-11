package alert

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/farrellm/rental-bot/internal/jobs"
)

// KindSweep is the job kind that runs the probes. The string is in the jobs
// table, so renaming it strands the rows already queued under the old name.
const KindSweep = "alert.sweep"

// Reading is what a probe saw.
//
// Cleared is how a probe asks for the recovery message. Reporting the
// condition either way means a probe never has to remember what it said last
// time — the bus already knows, because it has the open row — and a probe that
// remembers nothing cannot get out of step with the record after a restart.
type Reading struct {
	Alert
	Cleared bool
}

// Watching returns the reading for a condition that is currently true.
func Watching(key string, severity Severity, title, detail string) Reading {
	return Reading{Alert: Alert{Key: key, Severity: severity, Title: title, Detail: detail}}
}

// Clear returns the reading for a condition that is not.
func Clear(key, title string) Reading {
	return Reading{Alert: Alert{Key: key, Title: title}, Cleared: true}
}

// Probe reports every condition it can see, in both directions.
//
// A probe is for what nothing publishes an event for: a watch that lapsed
// while the process was down, a queue that stopped draining. Anything with a
// moment of its own — a job running out of attempts, a recovered panic —
// publishes directly and does not belong here.
type Probe func(ctx context.Context) []Reading

// Watchdog runs the probes and hands what they saw to the bus.
type Watchdog struct {
	bus *Bus
	log *slog.Logger

	mu     sync.Mutex
	probes []namedProbe
}

type namedProbe struct {
	name  string
	probe Probe
}

// NewWatchdog builds a watchdog over a bus.
func NewWatchdog(bus *Bus, logger *slog.Logger) *Watchdog {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watchdog{bus: bus, log: logger}
}

// Add registers a probe under a name that appears in the logs.
func (w *Watchdog) Add(name string, p Probe) {
	if p == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.probes = append(w.probes, namedProbe{name: name, probe: p})
}

// Sweep runs every probe once.
//
// One probe that panics or hangs must not cost the others their turn: a
// watchdog that stops watching after one bad reading is worse than no watchdog,
// because the screen still says it is running.
func (w *Watchdog) Sweep(ctx context.Context) error {
	w.mu.Lock()
	probes := slices.Clone(w.probes)
	w.mu.Unlock()

	for _, p := range probes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, reading := range w.read(ctx, p) {
			if reading.Cleared {
				w.bus.Resolve(ctx, reading.Key, reading.Title)
				continue
			}
			w.bus.Publish(ctx, reading.Alert)
		}
	}
	return nil
}

// read calls one probe, turning a panic into a logged line.
func (w *Watchdog) read(ctx context.Context, p namedProbe) (readings []Reading) {
	defer func() {
		if v := recover(); v != nil {
			w.log.Error("a probe panicked", "probe", p.name, "panic", fmt.Sprint(v))
			readings = nil
		}
	}()
	return p.probe(ctx)
}

// RegisterSweep wires the sweep onto a runner, the way each subsystem
// registers its own handlers.
//
// It takes no logger, unlike its siblings: the watchdog already has one, and
// the runner logs the error this handler returns.
func RegisterSweep(runner *jobs.Runner, w *Watchdog) {
	runner.Handle(KindSweep, func(ctx context.Context, _ jobs.Job) error {
		return w.Sweep(ctx)
	})
}

// Keys for the conditions this package raises itself. Everything else names
// its own next to the code that notices it.
const (
	// KeyQueueBacklog is the queue not draining.
	KeyQueueBacklog = "jobs.backlog"
)

// QueueDepthProbe watches for a queue that has stopped draining.
//
// A threshold of zero turns the probe off rather than alerting on every job,
// because "tell me when there is any work queued" is not a thing anybody
// wants at three in the morning.
func QueueDepthProbe(queue *jobs.Queue, threshold int) Probe {
	if queue == nil || threshold <= 0 {
		return nil
	}
	const title = "The job queue is not draining"

	return func(ctx context.Context) []Reading {
		depth, err := queue.Depth(ctx)
		if err != nil {
			// The database is the thing that would record the alert, so there
			// is nothing useful to say from here. The database check on
			// /readyz is what covers this.
			return nil
		}
		pending := depth["pending"]
		if pending < int64(threshold) {
			return []Reading{Clear(KeyQueueBacklog, title)}
		}
		return []Reading{Watching(KeyQueueBacklog, Warning, title,
			Errorf("%d jobs are waiting, past the threshold of %d. Nothing is being processed, or something is being retried in a loop.",
				pending, threshold))}
	}
}
