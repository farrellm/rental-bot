# rental-bot

A single-operator, self-hosted manager for a small portfolio of rental
properties. One Go process holds the durable facts about each property —
address, management company, mortgage, insurance, leases, tenants, cash flows,
repairs, valuation — and absorbs new ones with near-zero effort: forward an
email to a dedicated address, and an LLM extracts the update, files the
attachment, and proposes a ledger entry for one-tap approval.

Everything runs on one VPS: one binary, one SQLite file, one blob directory.

See [`docs/DESIGN.md`](docs/DESIGN.md) for the architecture and the milestone
plan.

## Status

**M2** — documents, blob store, manual transactions / repairs / leases. You
can sign in, put properties on file with their units, file documents against
them, keep a cash-flow ledger, run repairs with a dated history, and record
leases and tenants. Occupancy is derived from the lease dates rather than
stored. The email ingestion the product exists for arrives with M3 and M4.

## Requirements

Go 1.26 or newer, and Node 20 or newer to build the frontend. Nothing else —
the SQLite driver is pure Go, so the binary is static and cross-compiles
cleanly. `sqlite3`, `staticcheck`, and `sqlc` are optional; the targets that use
them say so when they are missing. The sqlc-generated query layer is committed,
so only someone changing a query needs the tool.

## Quick start

```sh
make build                          # bin/rental-bot, with the SPA embedded
./bin/rental-bot -create-user alice # prompts for a password, twice
make dev                            # API on :8080, Vite on :5174, hot reload
```

Then open <http://localhost:5174> and sign in.

There is no registration endpoint and no first-run setup screen: an instance
that is reachable before you have finished setting it up cannot be claimed by
whoever finds it first. Users come from `-create-user` and nowhere else.

## Make targets

`make` with no arguments lists these. Every target is safe to run from a fresh
clone; the ones that need `node_modules` install it themselves.

### Development

| Target | Effect |
| --- | --- |
| `make dev` | Runs the API and the Vite dev server together under `-j2`. One Ctrl-C stops both. Use this for day-to-day work. |
| `make dev-api` | The API alone on `:8080`, via `go run`. Serves JSON and the "frontend not in this binary" page. |
| `make dev-web` | Vite alone on `:5174`, proxying `/api`, `/healthz`, and `/readyz` to `:8080`. |
| `make watch` | The API with live reload, via [wgo](https://github.com/bokwoon95/wgo). Restarts on `.go` and `.sql` changes — the latter because migrations are embedded, so a new one only takes effect on a rebuild. |
| `make watch-dev` | `watch` and `dev-web` together: both halves reload. |

`make watch` needs wgo (`go install github.com/bokwoon95/wgo@latest`); the
target says so if it is missing. Editing a migration that has already been
applied will stop the reload loop with a checksum error — that is the runner
doing its job. Add a new `NNNN_` file instead.

### Build and run

| Target | Effect |
| --- | --- |
| `make build` | Builds the frontend, then compiles with `-tags spa` into `bin/rental-bot`. This is the deployable binary. |
| `make run` | `make build`, then runs the result against `$(CONFIG)`. |
| `make migrate` | Applies pending migrations and exits. Idempotent — a second run applies nothing. |

`./bin/rental-bot -create-user <name>` creates that user, or resets their
password if they already exist — which also ends every session they had. It
prompts twice with the echo off, or reads `RENTAL_BOT_ADMIN_PASSWORD` when
stdin is not a terminal. Passwords are at least twelve characters: this is
designed to be reachable from the internet and TOTP is still deferred, so the
password is the only thing between a stranger and the ledger.

Version, commit, and build date are stamped in through `-ldflags`, so
`./bin/rental-bot -version` and the status card report the real build rather
than `dev`.

### Checks

| Target | Effect |
| --- | --- |
| `make check` | `fmt-check`, `vet`, `lint`, `test`, `test-web`. **A commit has to pass this.** |
| `make test` | The Go tests. |
| `make test-web` | Type-checks the frontend with `tsc --noEmit`. |
| `make fmt` | Formats the Go sources in place. |
| `make fmt-check` | Fails, listing files, if anything is unformatted. Does not modify anything. |
| `make vet` | `go vet ./...`. |
| `make lint` | Runs `staticcheck` when it is installed, and says so when it is not. |
| `make generate` | Regenerates the sqlc query layer when `sqlc` is installed. Run it after changing a migration or a file in `internal/store/queries`. |

### Frontend

| Target | Effect |
| --- | --- |
| `make web-install` | `npm ci` — installs exactly what the lockfile pins. Use this in CI. |
| `make web-build` | Builds the frontend into `web/dist`, installing dependencies first if they are missing. |
| `make web-clean` | Removes `web/dist`. |

### Housekeeping

| Target | Effect |
| --- | --- |
| `make tidy` | `go mod tidy`. |
| `make clean` | Removes `bin/` and `web/dist`. |
| `make db-shell` | Opens the development database in the `sqlite3` CLI. |
| `make help` | Lists every target. This is the default. |

### Variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `CONFIG` | `config.toml` | Config file passed to `dev-api`, `run`, and `migrate`. A missing file is not an error. |
| `VERSION` | `git describe` | Version stamped into the binary. |
| `COMMIT` | short SHA | Commit stamped into the binary. |
| `BUILD_DATE` | today, UTC | Build date stamped into the binary. |

```sh
make run CONFIG=/etc/rental-bot/config.toml
make build VERSION=0.1.0
```

## Building without the frontend

A plain `go build` produces an **API-only** binary: the SPA embed sits behind
the `spa` build tag, so a fresh clone compiles and tests before anyone has run
npm. That binary serves a root page saying which build it is and how to fix it.
`make build` runs the frontend first and compiles with the tag — that is the
binary you deploy.

`web/dist/` is generated and gitignored.

## Configuration

A TOML file overlaid with `RENTAL_BOT_*` environment variables. Both are
optional: with neither, the defaults write into `./data`. Copy
[`config.example.toml`](config.example.toml) to `config.toml` to change
anything.

```sh
RENTAL_BOT_SERVER_ADDR=:9000 RENTAL_BOT_LOG_FORMAT=json make dev-api
```

`storage.max_upload_bytes` caps one document at 25 MiB by default. It is a cap
on the request body, so anything larger is refused before it reaches the disk.

Secrets are never read from the config file. The AES-GCM key protecting
encrypted columns comes from `RENTAL_BOT_SECRET_KEY`, or from a file named by
`RENTAL_BOT_SECRET_KEY_FILE` that must be mode `0600`.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /healthz` | The process is alive. Touches nothing else, so a database blip cannot trigger a restart loop. |
| `GET /readyz` | The process can serve traffic. `503` with per-check detail when it cannot. |
| `GET /api/v1/status` | Build identity, uptime, schema state, applied migrations, and the checks. Answers `200` even while degraded — the condition is in the body. |
| `GET /` | The single-page app. |

`/healthz` and `/readyz` are open, because a process manager has no session.
Everything under `/api/v1` needs one, except signing in:

| Path | Purpose |
| --- | --- |
| `POST /api/v1/auth/login` | Sets the session and CSRF cookies. |
| `POST /api/v1/auth/logout` | Ends the session and clears both. |
| `GET /api/v1/auth/me` | The signed-in operator; also reissues the CSRF cookie. |
| `GET/POST /api/v1/properties` | List (keyset pagination) and create. |
| `GET/PATCH/DELETE /api/v1/properties/{id}` | One property, with its units inline. |
| `GET/POST /api/v1/properties/{id}/units` | The units of one property. |
| `PATCH/DELETE /api/v1/units/{id}` | One unit. Deleting the last one is a `409`. |

Documents, the ledger, and tenancy:

| Path | Purpose |
| --- | --- |
| `POST /api/v1/documents` | Upload, as `multipart/form-data`. Bytes already on file return `200` with `deduplicated: true` rather than a second row. |
| `GET /api/v1/properties/{id}/documents` | Everything filed against a property. |
| `GET/PATCH/DELETE /api/v1/documents/{id}` | One document's record. Deleting it leaves the bytes. |
| `GET /api/v1/documents/{id}/content` | The bytes, for a signed-in operator only. |
| `POST/DELETE /api/v1/documents/{id}/links` | File a document against any record. |
| `GET/POST /api/v1/properties/{id}/transactions` | The ledger, with `from`, `to`, and `category` filters. The response carries the server's totals for the whole filtered set. |
| `PATCH/DELETE /api/v1/transactions/{id}` | One ledger entry. |
| `GET/POST /api/v1/properties/{id}/repairs` | Repairs, filterable by `status`. |
| `GET/PATCH/DELETE /api/v1/repairs/{id}` | One repair, with its event timeline inline. |
| `POST /api/v1/repairs/{id}/events` | Add a dated line to a repair's history. |
| `DELETE /api/v1/repair-events/{id}` | Remove one. |
| `GET/POST /api/v1/properties/{id}/leases` | Leases, reached through the property's units. |
| `GET/PATCH/DELETE /api/v1/leases/{id}` | One lease, with its tenants inline. A second live lease on one unit is a `409`. |
| `POST/DELETE /api/v1/leases/{id}/tenants` | Who is on a lease. |
| `GET/POST /api/v1/tenants`, `GET/PATCH/DELETE /api/v1/tenants/{id}` | Tenants, portfolio-wide. |
| `GET/POST /api/v1/vendors`, `GET/PATCH/DELETE /api/v1/vendors/{id}` | Vendors, portfolio-wide. |

Money on the wire is a signed integer count of cents. On a ledger entry the
sign is the whole distinction: income positive, expense negative. A repair
estimate or a rent is a magnitude and carries no sign.

Uploaded documents are stored by content: the SHA-256 is the filename, under
`blobs/<aa>/<bb>/<sha256>`, so re-forwarding a PDF costs nothing. Their bytes
are served only through `GET /api/v1/documents/{id}/content`, which needs a
session; only PDFs, common images, and plain text are served `inline`, and
everything else downloads, so an uploaded HTML file cannot run script on the
app's own origin.

Errors are RFC 7807 `application/problem+json`, always, including `405`s.

Mutating requests carry the `rb_csrf` cookie's value in an `X-CSRF-Token`
header. A `PATCH` distinguishes three states per field: absent leaves the
column alone, `null` clears it, a value sets it.

## License

[MIT](LICENSE)
