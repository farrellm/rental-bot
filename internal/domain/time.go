package domain

import "time"

// This file holds the one way this application spells a moment. Every package
// that writes a timestamp column went through a private copy of these three
// functions before they lived here, which is three chances for one of them to
// drift into a local time or a different layout.

// Stamp renders a timestamp the way every timestamp column in this schema holds
// one: RFC3339, UTC (docs/DESIGN.md §3).
//
// The format is not a display choice. Columns like jobs.run_after and
// jobs.locked_at are compared with < and <= in SQL rather than being parsed
// first, which works only because RFC3339 at a fixed UTC offset sorts
// lexicographically the way it sorts chronologically. Changing this is not a
// formatting change; it is a change to how the queue picks its next job.
func Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ParseStamp reads a stored timestamp back, reporting the zero time for
// anything unreadable.
//
// A column that will not parse is a broken record, not a reason to fail the
// work in hand: the callers are rendering a screen or deciding whether a
// cooldown has elapsed, and both do something sensible with a zero time.
func ParseStamp(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return at
}

// Today is the current calendar date, in UTC.
//
// A date off a document is stored exactly as written, with no timezone invented
// for it (docs/DESIGN.md §3), so the question "does this lease cover today" has
// to be asked in the same terms: one date string against two others. Taking the
// host's zone instead would make "occupied" depend on where the server sits.
func Today() string { return time.Now().UTC().Format(time.DateOnly) }
