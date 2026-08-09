package telegram

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSpoolKeepsDeliveryOrder(t *testing.T) {
	spool, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for i, key := range []string{"third", "first", "second"} {
		// Out of chronological order on purpose: the file name is what sorts,
		// not the order they were written.
		offsets := map[string]time.Duration{"first": 0, "second": time.Second, "third": 2 * time.Second}
		if err := spool.Add(start.Add(offsets[key]), key, "condition "+key); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	names, err := spool.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("Pending returned %d files, want 3", len(names))
	}

	want := []string{"first", "second", "third"}
	for i, name := range names {
		msg, err := spool.Read(name)
		if err != nil {
			t.Fatalf("Read %s: %v", name, err)
		}
		if msg.Key != want[i] {
			t.Errorf("position %d holds %q, want %q: the spool delivers oldest first", i, msg.Key, want[i])
		}
	}

	if err := spool.Remove(names[0]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Removing what is not there is how a double delivery ends, and it is not
	// an error.
	if err := spool.Remove(names[0]); err != nil {
		t.Errorf("removing an absent message = %v, want nil", err)
	}

	names, _ = spool.Pending()
	if len(names) != 2 {
		t.Errorf("Pending returned %d files after a removal, want 2", len(names))
	}
}

// A channel that has been down for a week must not fill the disk with the same
// four conditions restated every hour.
func TestSpoolIsBounded(t *testing.T) {
	spool, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}

	start := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for i := range spoolLimit + 20 {
		if err := spool.Add(start.Add(time.Duration(i)*time.Second), "host.disk", "low"); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	names, err := spool.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(names) != spoolLimit {
		t.Fatalf("Pending returned %d files, want the limit of %d", len(names), spoolLimit)
	}

	// The oldest went, not the newest: the latest reading of a condition is
	// the one worth delivering when the channel comes back.
	oldest, err := spool.Read(names[0])
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !oldest.At.After(start) {
		t.Errorf("the oldest message is %s, want the first ones dropped", oldest.At)
	}
}

// A key is a dotted ASCII constant by convention; this is the guard for the
// one that is not.
func TestSpoolNamesAreSafe(t *testing.T) {
	spool, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatalf("NewSpool: %v", err)
	}
	if err := spool.Add(time.Now(), "../../etc/passwd", "nope"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	names, err := spool.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("Pending returned %d files, want 1", len(names))
	}
	if filepath.Dir(filepath.Join(spool.Dir(), names[0])) != spool.Dir() {
		t.Errorf("the message landed at %q, outside the spool", names[0])
	}
}
