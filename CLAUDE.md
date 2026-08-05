# CLAUDE.md

`rental-bot` is a single-operator, self-hosted rental property manager: one Go
process serving a JSON API and an embedded React SPA, backed by SQLite.

**`docs/DESIGN.md` is the authority.** It covers the data model, the ingestion
pipeline, the LLM boundary, valuations, alerting, and the milestone plan. Read
it before proposing anything structural; this file records only what the code
does not already say.

Current milestone: **M1 complete** (auth, properties and units CRUD). Next is
M2 — documents, blob store, manual transactions / repairs / leases.

## Commands

`make` lists everything. The ones that matter:

| Command | Effect |
| --- | --- |
| `make check` | gofmt, vet, staticcheck, Go tests, frontend type-check. **A commit has to pass this.** |
| `make dev` | API on :8080 and Vite on :5174 together; one Ctrl-C stops both |
| `make build` | Frontend, then the binary with the SPA embedded, into `bin/` |
| `make migrate` | Apply pending migrations and exit |
| `make generate` | Regenerate the sqlc query layer; skips when sqlc is absent |
| `make test` / `make test-web` | Either half of the test suite alone |

`./bin/rental-bot -create-user <name>` is the only way a user is created.
There is no registration endpoint by design.

## Layout

```
cmd/rental-bot/     main, wiring, graceful shutdown
internal/config     TOML + RENTAL_BOT_* env overlay
internal/store      SQLite pools, migration runner, sqlc queries
internal/auth       argon2id, sessions, CSRF, rate limiting, middleware
internal/domain     Money, address normalization
internal/httpapi    handlers, DTOs, middleware, problem+json, SPA serving
internal/version    ldflags-stamped build identity
migrations/         NNNN_name.sql, embedded
web/                Vite React app, plus the build-tagged embed
```

The packages in `docs/DESIGN.md` §10 that are not listed here — `gmail`,
`ingest`, `llm`, `valuation`, `alert`, `telegram`, `blob`, `jobs`,
`scheduler` — arrive with the milestones that need them. Don't create them
empty ahead of time.

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

`web/dist/` is generated and gitignored.

## Conventions that must not erode

These come from the design doc and are load-bearing. Changing one is a design
decision, not a refactor.

- **Money is `domain.Money`, an int64 count of cents** — in the database, in
  Go, and on the wire. Never a float, never a decimal string. Income is
  positive, expense negative.
- **Calendar dates off documents are `TEXT` `YYYY-MM-DD`. Timestamps are
  RFC3339 UTC.** Documents rarely carry a timezone, and inventing one corrupts
  the record silently.
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
- **Nothing from an LLM reaches the ledger except through `ingest_proposals`**
  (from M4). Extraction runs tool-free with `MaxSteps: 1`, because forwarded
  email is untrusted input.
- **Property matching is deterministic Go** — normalize the address, compare
  against `properties.normalized_address`. The model returns a string; it
  never picks the property.
- **Frontend colours and faces come from `web/src/styles/tokens.css`**, never a
  literal hex value. Numbers use `.mono` so the tabular figures line up. The
  visual language is the county record card; `web/src/styles/card.css` explains
  it.
- **An entry is not a box.** Labels are pre-printed and values are typed onto
  the rule, so an input has no fill and no border except the line under it
  (`web/src/styles/controls.css`). Read and amend states share the same row
  geometry, which is what makes amending a card in place cost no reflow. Don't
  add a fill on focus — the focus ring is the indicator, and a fill draws back
  the box the whole design avoids.
- **Buttons are ink on stock**: a rule and a word, never a filled rectangle.
- **The stamp is the app's state machine.** One component says OPERATIONAL,
  DEGRADED, NO CONTACT, ACTIVE, PROSPECT, SOLD, AMENDING, REFUSED. Every
  variant is one `color:` declaration, because the border, inner ring, and
  divider all take `currentcolor`. It stays the only thing that moves.
- **Nouns carry the metaphor, verbs stay plain.** A "record" with a "file
  number" and an AMENDING stamp; buttons that say "Sign in", "Save changes",
  "Add unit".
- **Every screen has to look and work well on both a laptop and an iPhone.**
  This is a phone-first product in practice — approving a forwarded receipt
  happens on the phone, not at a desk — so a screen that only holds together
  at 1280px is not finished. See below for what that means concretely.

## Laptop and iPhone, both

Treat 320px (iPhone SE) through 1920px as the supported range, portrait and
landscape. The card layout switches at `40rem`: two columns above, stacked
below.

What has to hold at every width:

- No horizontal scrolling, ever. `document.scrollWidth` must equal
  `clientWidth`.
- Nothing overlaps. Reserve space with layout — flex or grid siblings — rather
  than a fixed padding guessed against another element's width. That guess is
  what broke the ledger against the stamp at 660px.
- Text stays legible: no tap target or control below 44px, and no body text
  below 11px.
- Layout survives inflated type. A flex item needs `min-width: 0` before
  `max-width` will constrain it, because the automatic minimum is the
  min-content width and a long word will not break on its own.

Verify with real device emulation, not a resized desktop viewport — a desktop
context narrowed to 320px applies text autosizing that a real iPhone does not,
and will report failures that do not exist. Wait for `networkidle` **and**
`document.fonts.ready` before measuring; the unstyled first paint is much
wider than the real layout and produces phantom overflow.

## Testing

Standard library `testing`, table-driven where there is more than one case. No
assertion library. Store tests open a real SQLite file under `t.TempDir()`;
`httpapi` tests substitute the small `Health` interface rather than a database.

## Known gaps at M1

- **`audit_log` is not written.** §8.5 implies web mutations are audited, but
  the table is not in migration 0001 and §11 assigns it to no milestone.
  Adding it exceeded "auth, properties and units CRUD".
- **`PATCH`/`DELETE /api/v1/units/{id}` are not in §7.1's table**, which gives
  no path for mutating a single unit. The screen needs both, so they exist.
- **Rate-limiter state is in memory** and does not survive a restart.
  Persisting it would put a write on every failed sign-in.
- **TOTP is still deferred.** The `users.totp_secret` column carries no rows.
- Migration 0001 lands only the tables M0 and M1 touch. The rest of
  `docs/DESIGN.md` §3 arrives milestone by milestone.
- Property detail has no tab bar. §7.2 describes eight tabs; seven of them are
  later milestones, and a tab bar with one tab is not a tab bar.
- List pagination works but nothing pages yet — the index fetches one page and
  the frontend ignores `next_cursor`. It matters past 50 properties.
