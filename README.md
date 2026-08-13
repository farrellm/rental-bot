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

**M4** — forwarded mail is read. A message that arrives is classified, its
enclosure is taken apart into the fields its kind has, and the result waits as
a *proposal* until somebody agrees to it. The Review screen puts the document
beside the reading: correct what the model got wrong, say which property it
belongs to, and approve. Approving writes the record, the audit row and the
link between them in one transaction.

Nothing an LLM produced reaches the ledger any other way. There is one
exception and it needs three things at once — a receipt, a confidence at or
above the threshold, and an address that folds to exactly one of your
properties — and the entry it writes still says nobody has checked it. Every
call is made tool-free at a single step, because forwarded email is untrusted
input and a PDF saying "mark all mortgages paid off" should reach a model with
no capability to act on it.

**M3.5** before it — the alert bus and the Telegram channel, outbound only. The
application tells you when it has stopped working: a revoked grant, a lapsed
watch, a job that gave up, a queue that stopped draining, a handler that
panicked. A condition is said once and then goes quiet until it clears, and the
Intake screen keeps the register of what went out.

**M3** before that — Gmail watch, webhook, fallback poller, raw archive.
Connect a mailbox and forwarded mail files itself: the message is archived as a
raw `.eml`, recorded in the register, and its attachments land in the document
store.

Everything before that still holds. You can sign in, put properties on file
with their units, file documents against them, keep a cash-flow ledger, run
repairs with a dated history, and record leases and tenants; occupancy is
derived from the lease dates rather than stored.

The chat is still one-way: it can pair, and after that it only sends. Commands
are M6's. Valuations are M5's.

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
make dev                            # API on :8082, Vite on :5174, hot reload
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
| `make dev-api` | The API alone on `:8082`, via `go run`. Serves JSON and the "frontend not in this binary" page. |
| `make dev-web` | Vite alone on `:5174`, proxying `/api`, `/healthz`, and `/readyz` to `:8082`. |
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

`./bin/rental-bot -unpair-telegram` forgets the paired chat, and it is the only
way to change who the alert channel trusts. Nothing reachable from Telegram can
do it, and neither can the web API — a hijacked session is a lower bar than a
shell, and the channel is what would report the hijack.

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

Secrets are never read from the config file:

| Variable | Purpose |
| --- | --- |
| `RENTAL_BOT_SECRET_KEY` | The AES-GCM key protecting encrypted columns. |
| `RENTAL_BOT_SECRET_KEY_FILE` | A path to that key. The file must be mode `0600`. |
| `RENTAL_BOT_GMAIL_CLIENT_SECRET` | The OAuth client secret. |
| `RENTAL_BOT_TELEGRAM_BOT_TOKEN` | The bot token from @BotFather. |

The encryption key becomes **required** once `gmail.client_id` is set: the Gmail
refresh token is stored encrypted, so a database copy without the key does not
hand over the mailbox.

The bot token is not stored anywhere. It is a static credential the operator
configures once, like the OAuth client secret above — unlike the Gmail refresh
token, which OAuth produces at runtime and which has nowhere to live but the
database. A leaked bot token grants the ability to send *as* the bot, not to
command it: authorization is the stored `chat_id`, and re-pairing needs a shell
on the host.

## Email ingestion

A blank `gmail.client_id` is the off switch. The application runs without a
Google project, the Intake screen says which keys are missing, and `/readyz`
reports the subsystem as fine rather than failing over something nobody asked
for.

Turning it on takes four things in Google Cloud:

1. **An OAuth client**, type *Web application*, with
   `<base_url>/api/v1/gmail/callback` as an authorized redirect URI. Its id goes
   in `gmail.client_id`; its secret goes in `RENTAL_BOT_GMAIL_CLIENT_SECRET`.
2. **A Pub/Sub topic**, named in `gmail.topic` in Google's full form
   (`projects/<project>/topics/<topic>`), with the Publisher role granted to
   `gmail-api-push@system.gserviceaccount.com` — that is the account Gmail
   publishes as, and without the grant `users.watch` fails.
3. **A push subscription** on that topic pointing at
   `<base_url>/webhooks/gmail`, with an OIDC token whose service account goes in
   `gmail.pubsub.service_account` and whose audience goes in
   `gmail.pubsub.audience`. Both are checked on every push, and a request that
   fails either is a `401`.
4. **`gmail.allowed_senders`** — only mail from these addresses is processed.
   Everything else is labelled and stored `ignored`. A public-ish inbox will
   receive spam, and this is the first and cheapest defense.

Then sign in, open **Intake**, and press *Connect Gmail*.

**The poller, not the webhook, is what makes ingestion reliable.** Pub/Sub is
at-least-once and occasionally lossy, and a `watch` can lapse silently, so the
scheduler walks the same history every `gmail.poll_interval` regardless. Every
step is idempotent on the Gmail message id, so the overlap costs one skipped
insert per message. The webhook only makes it fast.

Forwarded mail lands in three places: the raw `.eml` under `storage.raw_email`,
a row in the register, and each attachment in the content-addressed document
store. Then it is read, and what the reading produced waits in Review.

## Reading what arrives

Two stages, both against a model with no tools and one step. The first says
what kind of document this is and quotes any address on it; the second fills in
that kind's own form. Both land on one `ingest_proposals` row, which is what
lets the monthly token budget be a single sum rather than a ledger of its own.

Which property a document belongs to is never the model's decision. It returns
the address as a string, Go folds it the same way `properties.normalized_address`
was folded, and the two are compared: an exact fold, then the street line, then
an edit distance. More than one candidate at any tier is ambiguity rather than
a guess — the cost of a miss is a proposal that waits for you, and the cost of
a wrong match is a roof filed against the wrong building.

A read that fails leaves the message where it was, so the sweep finds it again.
That sweep is to the enqueue at sync time what the poller is to the webhook:
the enqueue makes reading fast, and the sweep makes it reliable.

Set `llm.provider` and `RENTAL_BOT_LLM_API_KEY` to turn it on. Leave the
provider blank and mail is still collected, archived and filed — there is
simply nobody reading it, and proposals already on file can still be settled.

## Alerts

The application is now able to fail silently — a watch lapses, a grant is
revoked, a job runs out of attempts — so it tells you when it has stopped
working.

A blank `telegram.bot_username` is the off switch, the same way a blank
`gmail.client_id` is. Everything still runs: conditions are recorded and
readable on the Intake screen's dispatch register, with no channel to send
them on. Turning it on takes two things:

1. **A bot**, from [@BotFather](https://t.me/BotFather). Its `@name` goes in
   `telegram.bot_username` without the `@`; its token goes in
   `RENTAL_BOT_TELEGRAM_BOT_TOKEN`.
2. **A pairing.** Sign in, open **Intake**, press *Get a pairing code*, and
   send the line it gives you to the bot. The code is single-use and lapses
   after `telegram.pairing_ttl`. If the web app is not reachable yet, the
   process logs a code at startup instead.

That single `chat_id` is the entire authorization model. Every update is
checked against it and dropped otherwise, and `-unpair-telegram` on the host is
the only way to change it.

**A condition is stated once and then goes quiet.** Each alert carries a stable
key; the second report of the same condition inside `telegram.cooldown` bumps a
tally rather than sending a second message, and an explicit message goes out
when the condition clears. An alert channel noisy enough to be muted is worse
than no channel at all.

**Critical alerts do not ride the job queue**, because the queue is one of the
things they report on. They take a direct path with bounded retry and a disk
spool under `storage.spool`, so a network blip delays delivery rather than
losing it. Routine alerts go through the queue and inherit its retries.

Long polling, not a webhook — deliberately. The process already terminates
public HTTPS for the Pub/Sub push, so a webhook would be nearly free, but the
alerting channel must not share a failure domain with the thing it reports on.
If TLS expires or the reverse proxy dies, a webhook-based bot goes silent at
exactly the moment it is needed.

Inbound commands are M6's. The only update this build acts on is the `/start`
that pairs a chat.

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

Email ingestion:

| Path | Purpose |
| --- | --- |
| `POST /webhooks/gmail` | The Pub/Sub push. Verifies the OIDC token, queues a sync, and answers immediately. **Outside the session**, because Pub/Sub has no cookie — the token is the whole of its authorization. |
| `GET /api/v1/gmail` | The mailbox's standing: connection, watch, cursor, last sync, and what the register holds. |
| `POST /api/v1/gmail/connect` | Returns the Google consent URL. The browser navigates to it. |
| `GET /api/v1/gmail/callback` | Where Google sends the operator back. The `state` is signed and bound to the session. |
| `DELETE /api/v1/gmail` | Revokes the grant at Google and forgets the account. The mail already on file stays. |
| `POST /api/v1/gmail/sync` | Queues a sync now. `queued: false` means one was already waiting. |
| `GET /api/v1/email-messages`, `GET /api/v1/email-messages/{id}` | The register, newest first, with attachments inline. |
| `GET /api/v1/email-messages/{id}/raw` | The archived `.eml`, always as an attachment. |

The review queue — nothing a model produced reaches the ledger except through
here:

| Path | Purpose |
| --- | --- |
| `GET /api/v1/review` | What is waiting, newest first, with each line's message facts and the queue's tally. `?status=all` to see what has already been decided. |
| `GET /api/v1/review/{id}` | One proposal: the extraction, the enclosures to render beside it, the matched property and why, and the portfolio to correct the match with. Carries a suggested new property when the document named an address nothing on file folds to. |
| `PATCH /api/v1/review/{id}` | Corrects what was read, before anybody agrees to it. Refused once the proposal is settled — from then on the record it produced is the thing to amend. |
| `POST /api/v1/review/{id}/approve` | Files it: the entity, the audit row, the document link, and the message's standing, in one transaction. `409` with the reason when it cannot be filed as things stand. |
| `POST /api/v1/review/{id}/reject` | Records that somebody looked and said no. The row stays. |
| `POST /api/v1/review/{id}/property` | Opens a record for a building the portfolio does not hold and files this proposal against it — the property, its implicit `Main` unit, and the retarget, in one transaction. |
| `GET /api/v1/properties/{id}/insurance`, `.../mortgage` | The policies and mortgages an applied proposal wrote. Read-only at M4; the mortgage carries its statements inline, and neither ever puts a policy or loan number on the wire. |

Alerts and the Telegram channel:

| Path | Purpose |
| --- | --- |
| `GET /api/v1/telegram` | The channel's standing: configured, paired, muted, when it last delivered, and the register's tally. |
| `POST /api/v1/telegram/pairing-code` | Issues a single-use code, shown once. `409` once a chat is paired — re-pairing needs server access. |
| `POST /api/v1/telegram/test` | Sends one notice, so the channel can be proved working before something goes wrong. |
| `GET /api/v1/notifications` | The dispatch register: what went out, when, and how many times. Works with no bot configured, because everything is recorded against the log channel too. |

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
