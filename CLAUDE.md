# CLAUDE.md

`rental-bot` is a single-operator, self-hosted rental property manager: one Go
process serving a JSON API and an embedded React SPA, backed by SQLite.

**`docs/DESIGN.md` is the authority.** It covers the data model, the ingestion
pipeline, the LLM boundary, valuations, alerting, and the milestone plan. Read
it before proposing anything structural; this file records only what the code
does not already say.

Current milestone: **M3.5 complete** (the alert bus with its cooldown, the
Telegram channel, pairing, and the dispatch register — outbound only; inbound
commands are M6's). Next is M4 — LLM classify/extract, `ingest_proposals`, and
the Review inbox.

## Commands

`make` lists every target with a one-line description; the Makefile is the
authority.

`.github/workflows/check.yml` runs `make check` and `make build` on every pull
request, plus `go test -race ./...`, which `make check` leaves out to stay quick
enough to run before a commit. CI installs staticcheck and then asserts it is on
PATH — `make lint` skips with a message and exits 0 without it, which is the
right default for a fresh clone and the wrong one for a merge gate.

`./bin/rental-bot -create-user <name>` is the only way a user is created.
There is no registration endpoint by design. `-unpair-telegram` is the only way
to change who the alert channel trusts, for the same kind of reason.

## Layout

`docs/DESIGN.md` §10 lists the packages. The ones it names that do not exist
yet — `ingest`, `llm`, `valuation` — arrive with the milestones that need them.
Don't create them empty ahead of time. `internal/secret` is not in §10's list;
it exists because §9.2's field encryption needed a home once M3 added the first
encrypted column.

## sqlc

Queries live in `internal/store/queries/*.sql`; `make generate` writes
`internal/store/sqlc/`. **The generated code is committed**, so `make check`
and a fresh clone never need the tool.

- **Comments in the query files are ASCII only.** sqlc's SQLite rewriter counts
  comment offsets in runes but slices them in bytes, so one em dash or `§`
  silently truncates the `RETURNING *` that follows and the query fails to
  parse. This is a sqlc bug, not a query error, and it is very confusing when
  you hit it.
- Writes go through `repo.Write()`, reads through `repo.Read()` — one
  `*sqlc.Queries` per pool, so a call site that picks the wrong one is visible
  in review. `repo.Tx` wraps a write transaction.

## Building

`go build` produces an **API-only** binary: the SPA embed sits behind the `spa`
build tag so a fresh clone compiles and tests before anyone has run npm. That
binary serves a root page saying so. `make build` runs the frontend first and
compiles with `-tags spa`; that is the binary you deploy.

## Conventions that must not erode

These come from the design doc and are load-bearing. Changing one is a design
decision, not a refactor.

- **Money is `domain.Money`, an int64 count of cents** — in the database, in
  Go, and on the wire. Never a float, never a decimal string. Income is
  positive, expense negative.
- **Calendar dates off documents are `TEXT` `YYYY-MM-DD`. Timestamps are
  RFC3339 UTC.** Documents rarely carry a timezone, and inventing one corrupts
  the record silently. `domain.Stamp`, `domain.ParseStamp` and `domain.Today`
  are the only spellings of either — don't add a package-local one. The format
  is load-bearing beyond display: `jobs.run_after` and `jobs.locked_at` are
  compared with `<` in SQL rather than parsed, which works only because RFC3339
  at a fixed UTC offset sorts lexicographically the way it sorts
  chronologically. A package with an injectable clock keeps the clock and calls
  `domain.Stamp(x.now())`.
- **Migrations are append-only and checksummed.** Never edit a migration that
  has been applied — the runner refuses to start when a recorded checksum
  changes. Corrections land as a new `NNNN_` file.
- **Every table gets `id INTEGER PRIMARY KEY`, `created_at`, `updated_at`**
  (documented exceptions only), is `STRICT`, and spells enums as `TEXT` with a
  `CHECK`.
- **API errors are RFC 7807 `application/problem+json`**, always, including
  405s. Use `httpapi.WriteProblem`.
- **Writes go through `db.Writer()`, reads through `db.Reader()`.** The writer
  pool is one connection on purpose.
- **A PATCH field has three states and all three differ**: absent leaves the
  column alone, `null` clears it, a value sets it. Bodies decode into
  `map[string]json.RawMessage` (see `httpapi/patch.go`), never a struct — a
  struct collapses absent and null, and "the price is unknown" is not "the
  price is zero". The merge is a read-modify-write in Go inside the write
  transaction, because COALESCE cannot express the difference.
- **`normalized_address` is derived and never accepted from a client.** It is
  recomputed on every write that touches the address.
- **Every property keeps at least one unit.** Creation makes an implicit one
  called `Main`; deleting the last is a 409. Every lease hangs off a unit, so
  zero would fork the query shape for every later milestone.
- **Sessions are opaque tokens; only the SHA-256 hash is stored.** Mutating
  requests need the `X-CSRF-Token` header matching the `rb_csrf` cookie.
- **A document's SHA-256 is its identity.** `documents.sha256` is UNIQUE and
  the bytes live at `blobs/<sha[0:2]>/<sha[2:4]>/<sha256>`, mode `0600` under
  `0700` directories. Uploading bytes already on file returns the existing row
  with `200` and `deduplicated: true` — forwarding the same receipt twice is
  normal, not an error. `blob.ValidDigest` guards the one place a
  caller-supplied string becomes a path; lowercase hex only, because accepting
  both cases would file one file at two paths.
- **Document content is served only through the authenticated handler**, never
  by the reverse proxy. Only `application/pdf`, `image/png|jpeg|gif|webp`, and
  `text/plain` are served `inline`; everything else is `attachment`, so an
  uploaded HTML or SVG cannot run script on the app's own origin. `nosniff` and
  `default-src 'none'; sandbox` are the second and third layers.
- **Occupancy is derived on every read, never stored.** A unit is occupied if a
  lease covers today (`ListUnitsWithOccupancy`). The API returns the lease's
  *id*, not a boolean, so a screen can link to the evidence. There is no
  `is_occupied` column and there must not be one.
- **A unit holds one live lease at a time**, enforced in the write path — two
  pending-or-active leases covering the same days make occupancy ambiguous, and
  the derived answer only stays unambiguous if the write refuses. Ended and
  terminated leases are history and overlap nothing.
- **The signed-cents convention is `transactions.amount_cents` only.** There
  the sign is the sole thing separating income from expense. A repair estimate
  or a rent is a magnitude; negating one produces "-$650.00" on a screen where
  it reads as a mistake.
- **A `sqlc.narg` in a SQLite query needs a `CAST(... AS TEXT)` around it**, or
  sqlc's inference gives up and types the parameter `interface{}`. The casts in
  `transactions.sql` are load-bearing; don't tidy them away.
- **The poller, not the webhook, is what makes ingestion reliable.** Pub/Sub is
  at-least-once and occasionally lossy and a `watch` lapses silently, so the
  scheduler walks the same history every `gmail.poll_interval` regardless. The
  webhook only makes it fast. Anything that would make the poll conditional on
  the push is a change to the reliability story, not an optimisation.
- **`email_messages.gmail_message_id` is UNIQUE, and that one constraint is the
  idempotency key for the whole pipeline.** Every step reads before it writes,
  so the poller re-walking what the push already delivered costs one skipped
  insert per message. Adding a step that is not idempotent on it breaks the
  overlap that the design rests on.
- **The raw `.eml` is archived before the row is inserted.** The archive exists
  so a parser fix can be replayed against what actually arrived, and a message
  that fails to parse is exactly the one whose bytes are wanted. A message too
  large to fetch gets a `failed` row with the reason and no `raw_path` — it
  will be the same size next time, so retrying is pointless and silence is
  worse.
- **The sender allowlist runs before anything is filed.** A message from an
  address outside it is stored `ignored` with no attachments taken. A
  public-ish inbox receives spam; this is the first and cheapest defense.
- **The Pub/Sub push carries a `historyId` that is never read.** Believing it
  would let anyone who captured a token choose where in history this process
  resumes from. The sync takes its cursor from `gmail_account`. The push's OIDC
  token is verified against a fixed `RS256` — never the algorithm the token
  names — and an unconfigured verifier refuses everything, because failing open
  puts an unauthenticated enqueue endpoint on the public internet.
- **A third-party redirect carries `auth.IssueState`/`CheckState`**, signed and
  bound to the session. Without the binding an attacker starts the OAuth flow
  with their own account and hands the callback to the operator, whose session
  then has the attacker's mailbox attached to it.
- **An alert `Key` names the condition, not the occurrence.** A job kind that
  keeps failing is one key, not one per job id; a route that keeps panicking is
  one key, not one per request. The cooldown, the tally, and the recovery
  message all hang off that distinction, and getting it wrong turns
  deduplication off without appearing to. Keys are dotted constants next to the
  code that raises them, never built from a message.
- **Critical alerts do not ride the job queue.** An alert reporting that the
  queue is stuck cannot be enqueued on the stuck queue. They take a buffered
  channel drained by a dedicated goroutine, with bounded retry and a disk spool
  under `storage.spool`; routine alerts go through the queue and inherit its
  retries. Moving either onto the other path is a change to the reliability
  story, not a simplification.
- **The alert bus records before it delivers, and never rolls that back.** A
  condition that was noticed and could not be sent is still a condition that
  was noticed; the sink owns retrying. `Publish` returns no error on purpose —
  every caller is something that has just found a problem and has its own work
  to get back to.
- **`internal/alert` must not import `internal/telegram`.** A channel is a
  `Sink`, and the queue-backed one lives in `telegram`, which imports both. The
  same rule is why `jobs.RunnerOptions.OnDeadLetter` is a callback rather than
  an `alert.Publisher`: the queue is a queue, not an alert client.
- **Re-pairing Telegram needs a shell on the host.** `-unpair-telegram` and
  nothing else. The web can issue a pairing code for an *unpaired* channel, so
  a first deploy does not need SSH, and is refused with a 409 once a chat is
  paired — a hijacked session must not be able to move the alert channel to the
  attacker's own chat, because that channel is what would report the hijack.
  Only the SHA-256 of a code is stored, and the single-use guard is in the
  `UPDATE` rather than in Go.
- **A job handler must be idempotent**, because the queue is at-least-once by
  construction: a process killed after the work and before the row is marked
  done runs it again. `attempts` increments on *claim*, not on failure, so a
  payload that kills its worker still spends a try rather than retrying
  forever.
- **A blank `gmail.client_id` is the off switch and a working state.** A fresh
  clone has no Google project. The subsystem is not built, the routes answer
  503 naming the missing keys, and `/readyz` reports it OK — a check that fails
  over a subsystem nobody asked for is a check that teaches its reader to
  ignore it.
- **Nothing from an LLM reaches the ledger except through `ingest_proposals`**
  (from M4). Extraction runs tool-free with `MaxSteps: 1`, because forwarded
  email is untrusted input.
- **Property matching is deterministic Go** — normalize the address, compare
  against `properties.normalized_address`. The model returns a string; it
  never picks the property.
- **The stamp is the app's state machine.** One component says OPERATIONAL,
  DEGRADED, NO CONTACT, ACTIVE, PROSPECT, SOLD, AMENDING, REFUSED; from M2
  OPEN, SCHEDULED, IN PROGRESS, DONE, WON'T FIX, PENDING, ENDED, TERMINATED;
  and from M3 WATCHING, LAPSED, REVOKED, NOT CONNECTED, NOT SET UP for the
  mailbox, plus RECEIVED, PARSING, NEEDS REVIEW, APPLIED, REJECTED, IGNORED,
  FAILED for one message. All seven message words exist now even though M3
  writes three, so the register can never show a state it has no word for.
  NOT SET UP and NOT CONNECTED are different claims and both are fine: nobody
  asked for ingestion, versus somebody did and did not finish. M3.5 adds only
  PAIRED and MUTED, for the alert channel, and reuses NOT SET UP, NOT CONNECTED
  and NO CONTACT — they are the same three claims about a different subsystem,
  and a reader should not have to learn a second set of words for them.
  Every variant is one `color:` declaration, because the border, inner ring,
  and divider all take `currentcolor`. It stays the only thing that moves.
- **The dispatch register is one line per condition.** A restatement bumps the
  `×n` tally in the margin; a cleared condition is ruled off with a
  strikethrough. That mark is the data model made visible, and it is why the
  API reads `notifications` one channel at a time — every subscribed channel
  has its own row per condition, so an unfiltered register shows everything
  twice.
- **Every screen has to look and work well on both a laptop and an iPhone.**
  This is a phone-first product in practice — approving a forwarded receipt
  happens on the phone, not at a desk — so a screen that only holds together
  at 1280px is not finished. `web/CLAUDE.md` says what that means concretely,
  along with the rest of the frontend's conventions; it loads when you work
  under `web/`.

## Testing

Standard library `testing`, table-driven where there is more than one case. No
assertion library. Store tests open a real SQLite file under `t.TempDir()`;
`httpapi` tests substitute the small `Health` interface rather than a database.

## Known gaps at M3.5

- **`audit_log` is still not written.** §8.5 implies web mutations are audited,
  but the table is in no migration and §11 assigns it to no milestone.
- **Orphaned blobs are never reclaimed.** Deleting a document removes the row
  and its links and leaves the bytes: a digest is the only name content has,
  and a restore should still find the file. A sweep belongs with M7's backup
  work, not with a single delete.
- **`documents_fts` is deferred to M6**, where the search endpoint gives it a
  reason to exist. Backfilling it over the documents on file is one
  `INSERT … SELECT`.
- **`document_links.entity_type` carries a CHECK over the entities that exist
  at M2.** Widening it for a later milestone's entity means rebuilding the
  table, which is the price of a typo'd type failing loudly instead of
  silently orphaning a link.
- **`transactions.proposal_id` is still absent**, for the reason
  `documents.source_message_id` was until M3 added it: with `foreign_keys=ON` a
  reference to a table that does not exist yet fails at INSERT rather than at
  CREATE, so the milestone that creates the target adds the column with its
  constraint. M4 adds this one.
- **An ingested attachment is filed against no property.** Matching an address
  to a property is deterministic Go over an extracted string, and the
  extraction that produces that string is M4's. The bytes are on file and the
  association is not; that split is the point of the content-addressed store.
- **`gmail.send` is in the requested scopes and nothing uses it.** §4.2 step 7
  replies in-thread once ingestion resolves, which needs M4's proposal to have
  resolved into something. Asking for the scope now means the operator consents
  once instead of being sent back through a consent screen by an upgrade.
- **A dead-lettered job now alerts but still nothing retries it.** A job past
  `max_attempts` is `failed`, the row stays as the record, and
  `RunnerOptions.OnDeadLetter` gives it a voice. Requeueing one is still a
  manual `UPDATE`; a re-run button belongs with a screen that lists the queue,
  and nothing lists the queue yet.
- **Nothing prunes the raw `.eml` archive.** §12's first open question — keep
  forever or prune after N years — is still open, and the year/month directory
  layout is what makes either answer cheap.
- **`PATCH`/`DELETE /api/v1/units/{id}`, and the repair, lease, tenant and
  vendor routes, are not all in §7.1's table.** The screens need them, and the
  endpoint list in the README is the current authority.
- **Rate-limiter state is in memory** and does not survive a restart.
  Persisting it would put a write on every failed sign-in.
- **TOTP is still deferred.** The `users.totp_secret` column carries no rows.
- **`telegram_state.muted_until` is written by nothing.** `/mute` is an inbound
  command and inbound commands are M6's. The column, the suppression in the
  sender, and the MUTED stamp all exist and are tested; there is no way to set
  it yet, which is the right order — the thing that reads a state should exist
  before the thing that writes it.
- **The `alerts` table from §3.2 is not in any migration.** All five of its
  kinds are business alerts M6 computes. `notifications` is the delivery log
  and is unrelated to it.
- **Nothing prunes `notifications` or the alert spool.** The spool is bounded at
  200 files; the table is not bounded at all. It grows one row per condition
  per channel, which is slow, but it is unbounded.
- Migrations land only the tables a milestone touches. The rest of
  `docs/DESIGN.md` §3 arrives milestone by milestone.
- Property detail has five of §7.2's eight tabs. Insurance, Mortgage, and Value
  arrive with the milestones that fill them. §7.2's Settings screen is now the
  Intake screen for its Gmail half and its Telegram half; provider health and
  backup status join it at M5 and M7.
- **List pagination works but nothing pages yet** — every index fetches one
  page and the frontend ignores `next_cursor`, the dispatch register included.
  It matters past 50 properties, or a property with a long ledger.
