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

**M0** — skeleton, migrations, config, health endpoints. The process boots,
migrates, serves, and reports on itself; there is no application behaviour yet.

## Running it

```sh
make dev        # API on :8080, Vite on :5173
make build      # bin/rental-bot, with the SPA embedded
make check      # gofmt, vet, tests, frontend type-check
make            # list every target
```

Configuration is a TOML file overlaid with `RENTAL_BOT_*` environment
variables; both are optional, and the defaults write into `./data`. Copy
[`config.example.toml`](config.example.toml) to `config.toml` to change
anything. Secrets come from the environment only.

## Endpoints

| Path | Purpose |
| --- | --- |
| `GET /healthz` | The process is alive |
| `GET /readyz` | The process can serve traffic, with per-check detail |
| `GET /api/v1/status` | Build identity, uptime, schema state, applied migrations |
| `GET /` | The record-of-service card |

## License

[MIT](LICENSE)
