package httpapi

import "time"

// The rules the database cannot state for itself.
//
// A CHECK constraint refuses a bad row, but it refuses it as a 500 carrying a
// constraint message. These checks exist so the operator gets a 422 naming what
// to fix. The database stays the authority; this is the part that can be
// polite about it.

// validationError carries a client-facing message out of a transaction.
//
// A read-modify-write does its validation inside the write transaction, where
// returning an HTTP status is not possible. This is how the message gets back
// out to the handler that can send one.
type validationError struct{ detail string }

func (e validationError) Error() string { return e.detail }

// isCalendarDate reports whether s is a YYYY-MM-DD date that exists.
//
// Dates that come off documents are stored exactly as written, with no
// timezone invented for them (docs/DESIGN.md §3), so this checks the spelling
// rather than parsing into a time.Time and back.
func isCalendarDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}

// validateRoomCounts checks the three measurements a property and a unit both
// carry, so the bounds cannot drift apart between the two screens that show
// them side by side.
func validateRoomCounts(beds *int64, baths *float64, sqft *int64) string {
	if beds != nil && (*beds < 0 || *beds > 1000) {
		return "Beds has to be between 0 and 1000."
	}
	if baths != nil && (*baths < 0 || *baths > 1000) {
		return "Baths has to be between 0 and 1000."
	}
	if sqft != nil && (*sqft < 0 || *sqft > 10_000_000) {
		return "Square feet has to be between 0 and 10,000,000."
	}
	return ""
}
