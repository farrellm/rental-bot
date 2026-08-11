package domain

// Truncate bounds a string that is going into a column or into a message,
// marking that it was cut.
//
// The limit is in bytes, not runes, because what it protects is a column width
// and a message length. That can split a multi-byte rune at the boundary; the
// callers pass error text and operational detail, where a mangled last
// character costs nothing and a silently unbounded column costs a row.
//
// A stack trace in an alert body is a stack trace nobody reads on a phone
// (docs/DESIGN.md §8.6), and a 40 KB error from a rejected HTTP body does not
// belong in a jobs row every screen reads.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// Clip is Truncate without the ellipsis, for a value where the three dots would
// read as content rather than as a mark.
//
// An email subject and a snippet are the cases: they are shown as the thing
// itself, and "Re: rent for Apri..." is a subject line, where "...", appended to
// a jobs row's last_error, is punctuation about the record.
func Clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
