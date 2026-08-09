package telegram

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// spoolLimit bounds how many undelivered messages are kept.
//
// A channel that has been down for a week should not fill the disk with the
// same four conditions restated every hour. Past the limit the oldest are
// dropped, because the newest reading of a condition is the one worth
// delivering when the channel comes back.
const spoolLimit = 200

// Spool holds messages that could not be delivered when they were raised.
//
// It exists for one path: §8.4's critical alerts, which cannot ride the job
// queue because the queue is one of the things they report on. A network blip
// then delays delivery rather than losing it.
//
// One file per message, named by the time it was spooled, so the directory
// sorts into delivery order and a person can read it with ls.
type Spool struct{ dir string }

// spooled is one message on disk.
type spooled struct {
	At   time.Time `json:"at"`
	Key  string    `json:"key"`
	Text string    `json:"text"`
}

// NewSpool prepares the directory. 0700, like every other directory this
// process owns: an alert body is operational detail.
func NewSpool(root string) (*Spool, error) {
	dir := filepath.Join(root, "telegram")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("telegram: create the spool %s: %w", dir, err)
	}
	return &Spool{dir: dir}, nil
}

// Dir is where the spool writes, for the logs and for tests.
func (s *Spool) Dir() string { return s.dir }

// Add writes one message.
func (s *Spool) Add(at time.Time, key, text string) error {
	body, err := json.Marshal(spooled{At: at.UTC(), Key: key, Text: text})
	if err != nil {
		return fmt.Errorf("telegram: encode a spooled message: %w", err)
	}

	// Nanoseconds and the key: two conditions spooled in the same nanosecond
	// would otherwise be one file.
	name := fmt.Sprintf("%020d-%s.json", at.UTC().UnixNano(), safeName(key))
	if err := os.WriteFile(filepath.Join(s.dir, name), body, 0o600); err != nil {
		return fmt.Errorf("telegram: spool a message: %w", err)
	}
	return s.trim()
}

// Pending lists what is waiting, oldest first.
func (s *Spool) Pending() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("telegram: read the spool: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	// The name starts with a zero-padded nanosecond count, so lexicographic is
	// chronological -- the same trick the RFC3339 timestamps in SQL rely on.
	sort.Strings(names)
	return names, nil
}

// Read loads one spooled message by name.
func (s *Spool) Read(name string) (spooled, error) {
	body, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return spooled{}, fmt.Errorf("telegram: read a spooled message: %w", err)
	}
	var msg spooled
	if err := json.Unmarshal(body, &msg); err != nil {
		return spooled{}, fmt.Errorf("telegram: decode %s: %w", name, err)
	}
	return msg, nil
}

// Remove deletes a message that has been delivered.
func (s *Spool) Remove(name string) error {
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("telegram: remove a spooled message: %w", err)
	}
	return nil
}

// trim drops the oldest messages past the limit.
func (s *Spool) trim() error {
	names, err := s.Pending()
	if err != nil {
		return err
	}
	for i := 0; i < len(names)-spoolLimit; i++ {
		if err := s.Remove(names[i]); err != nil {
			return err
		}
	}
	return nil
}

// safeName keeps a dedupe key usable as a filename. Keys are dotted ASCII by
// convention; this is the guard for the one that is not.
func safeName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		return "unkeyed"
	}
	return truncate(name, 64)
}
