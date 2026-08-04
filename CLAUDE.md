# CLAUDE.md

`rental-bot` is a single-operator, self-hosted rental property manager: one Go
process serving a JSON API and an embedded React SPA, backed by SQLite.

**`docs/DESIGN.md` is the authority.** It covers the data model, the ingestion
pipeline, the LLM boundary, valuations, alerting, and the milestone plan. Read
it before proposing anything structural; this file records only what the code
does not already say.

Current milestone: **M0 complete** (skeleton, migrations, config, health
endpoints). Next is M1 — auth, properties and units CRUD.

## Commands

`make` lists everything. The ones that matter:

| Command | Effect |
| --- | --- |
| `make check` | gofmt, vet, staticcheck, Go tests, frontend type-check. **A commit has to pass this.** |
| `make dev` | API on :8080 and Vite on :5174 together; one Ctrl-C stops both |
| `make build` | Frontend, then the binary with the SPA embedded, into `bin/` |
| `make migrate` | Apply pending migrations and exit |
| `make test` / `make test-web` | Either half of the test suite alone |

## Layout

```
cmd/rental-bot/     main, wiring, graceful shutdown
internal/config     TOML + RENTAL_BOT_* env overlay
internal/store      SQLite pools, checksummed migration runner
internal/domain     Money and, later, address normalization
internal/httpapi    handlers, middleware, problem+json, SPA serving
internal/version    ldflags-stamped build identity
migrations/         NNNN_name.sql, embedded
web/                Vite React app, plus the build-tagged embed
```

The packages in `docs/DESIGN.md` §10 that are not listed here — `auth`,
`gmail`, `ingest`, `llm`, `valuation`, `alert`, `telegram`, `blob`, `jobs`,
`scheduler` — arrive with the milestones that need them. Don't create them
empty ahead of time.

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

## Known gaps at M0

- `/api/v1/status` is **unauthenticated** — there is no auth until M1. It
  exposes build identity and schema state only. Move it behind the session
  middleware when that middleware exists.
- Migration 0001 lands only the tables M0 and M1 touch. The rest of
  `docs/DESIGN.md` §3 arrives milestone by milestone.
- `sqlc` is not wired up yet; there are no queries to generate. It comes with
  M1's CRUD.
