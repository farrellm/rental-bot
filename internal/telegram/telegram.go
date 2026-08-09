// Package telegram is the channel operational alerts go out on, and the
// pairing that decides who receives them (docs/DESIGN.md §8).
//
// M3.5 is outbound only. The long-poll loop runs, but the only update it acts
// on is the `/start <code>` that pairs a chat; the commands in §8.5 arrive at
// M6. That is a deliberate shape rather than an unfinished one — the alerting
// half is worth having before the LLM work makes silent failure possible, and
// a chat that can only receive is a chat with no way to be talked into
// anything.
//
// Three decisions here come straight from the design doc and are load-bearing:
//
//   - Long polling, not a webhook. The process already terminates public HTTPS
//     for the Pub/Sub push, so a webhook would be nearly free. It is still
//     wrong: the alerting channel must not share a failure domain with the
//     thing it reports on. If TLS expires or the reverse proxy dies, a
//     webhook-based bot goes silent at exactly the moment it is needed (§8.1).
//   - A single chat_id is the entire authorization model, and re-pairing needs
//     server access. Nothing reachable from Telegram can change who Telegram
//     trusts (§8.2).
//   - Critical alerts do not ride the job queue. An alert saying the queue is
//     stuck cannot be enqueued on the stuck queue (§8.4).
package telegram

import "errors"

// Job kinds. These strings are in the jobs table, so renaming one strands the
// rows already queued under the old name.
const (
	// KindSend carries a routine alert. Critical ones take the other path.
	KindSend = "telegram.send"
)

var (
	// ErrNotPaired reports that no chat has been paired, which is a working
	// state and not a fault: the bot is configured and nobody has finished
	// setting it up.
	ErrNotPaired = errors.New("telegram: no chat is paired")
	// ErrAlreadyPaired reports an attempt to pair over an existing chat.
	// Re-pairing is server access only (§8.2).
	ErrAlreadyPaired = errors.New("telegram: a chat is already paired")
	// ErrBadPairingCode reports a code that was wrong, used, or expired. The
	// three are one error on purpose: telling a prober which of the three it
	// got is telling it how to get closer.
	ErrBadPairingCode = errors.New("telegram: that pairing code is not valid")
)

// truncate bounds what goes into a column and into a message.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
