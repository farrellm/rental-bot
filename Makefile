# rental-bot
#
# `make` lists the targets. `make check` is what a commit has to pass.

BIN        := bin/rental-bot
PKG        := github.com/farrellm/rental-bot
CONFIG     ?= config.toml

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%d)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.BuildDate=$(BUILD_DATE)

NPM := npm --prefix web

.DEFAULT_GOAL := help
.PHONY: help dev dev-api dev-web watch watch-dev build run migrate test test-web \
        fmt fmt-check vet lint check generate tidy web-deps web-install \
        web-build web-clean clean db-shell

help: ## List the targets
	@grep -hE '^[a-z][a-z-]*:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'

## Development ---------------------------------------------------------------

dev: ## Run the API and the Vite dev server together; one Ctrl-C stops both
	@$(MAKE) -j2 --no-print-directory dev-api dev-web

dev-api: ## Run the API alone on :8080, serving JSON only
	go run ./cmd/rental-bot -config $(CONFIG)

dev-web: web-deps ## Run Vite alone on :5174, proxying the API to :8080
	@$(NPM) run dev

watch: ## Run the API with live reload, restarting on .go and .sql changes
	@command -v wgo >/dev/null 2>&1 || { \
		echo "wgo is not installed: go install github.com/bokwoon95/wgo@latest"; exit 1; }
	wgo run -file='\.sql$$' ./cmd/rental-bot -config $(CONFIG)

watch-dev: ## Like dev, but the API reloads too
	@$(MAKE) -j2 --no-print-directory watch dev-web

## Build ---------------------------------------------------------------------

build: web-build ## Build the release binary with the SPA embedded
	@mkdir -p bin
	go build -tags spa -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/rental-bot
	@echo "built $(BIN) $(VERSION) ($(COMMIT))"

run: build ## Build and run the release binary
	./$(BIN) -config $(CONFIG)

migrate: ## Apply pending migrations and exit
	go run ./cmd/rental-bot -config $(CONFIG) -migrate

## Checks --------------------------------------------------------------------

check: fmt-check vet lint test test-web ## Everything a commit has to pass

test: ## Run the Go tests
	go test ./...

test-web: web-deps ## Type-check the frontend
	@$(NPM) run typecheck

fmt: ## Format the Go sources
	gofmt -w .

fmt-check: ## Fail if anything is unformatted
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

vet: ## Run go vet
	go vet ./...

lint: ## Run staticcheck when it is installed
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

## Codegen -------------------------------------------------------------------

# The generated query layer is committed, so `make check` and a fresh clone
# never need sqlc. Run this after changing a migration or a query file.
generate: ## Regenerate the sqlc query layer when sqlc is installed
	@if command -v sqlc >/dev/null 2>&1; then \
		sqlc generate && echo "regenerated internal/store/sqlc"; \
	else \
		echo "sqlc not installed; skipping (go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest)"; \
	fi

## Frontend ------------------------------------------------------------------

web-install: ## Install frontend dependencies from the lockfile
	@$(NPM) ci

# Every frontend target depends on this, so a fresh clone works without
# anyone having to know to run npm first.
web-deps:
	@[ -d web/node_modules ] || $(NPM) install

web-build: web-deps ## Build the frontend into web/dist
	@$(NPM) run build

web-clean: ## Remove the built frontend
	rm -rf web/dist

## Housekeeping --------------------------------------------------------------

tidy: ## Tidy go.mod
	go mod tidy

clean: web-clean ## Remove build output
	rm -rf bin

db-shell: ## Open the development database in the sqlite3 CLI
	@command -v sqlite3 >/dev/null 2>&1 || { echo "sqlite3 is not installed"; exit 1; }
	sqlite3 data/rental.db
