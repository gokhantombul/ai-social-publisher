# AGENTS.md

Guidance for coding agents working in this repository.

## Project Overview

- `ai-social-publisher` is a modular monolith Go service for AI-assisted, approval-based Instagram publishing.
- The application pulls news, scores candidates with AI, sends Telegram approval notifications, generates post variants, renders a PNG image, stores it locally, and publishes through Instagram Graph API.
- External systems are `haber-servisi`, `telegram-servisi`, Instagram Graph API, and AI providers (`tgpt` primary, Ollama fallback).
- Keep package boundaries clear: orchestration belongs in `internal/approval`; HTTP wiring in `internal/http`; persistence and domain behavior stay in the relevant `internal/*` package.

## Repository Layout

- `cmd/server`: application entry point and CLI commands (`serve`, `migrate`).
- `internal/account`: account config sync and repository code.
- `internal/ai`: provider chain, prompts, JSON parsing, validation.
- `internal/approval`: end-to-end workflow orchestration.
- `internal/config`: YAML loading, environment expansion, defaults, validation.
- `internal/database`: pgx pool and goose migration runner.
- `internal/http`: chi routes, API handlers, auth/security middleware.
- `internal/admin`: embedded admin panel handlers, templates, and assets.
- `internal/instagram`: Instagram Graph publisher with dry-run support.
- `internal/media`: template-based PNG renderer.
- `internal/news`: news candidate model, repository, and service client.
- `internal/outbox`: durable Telegram notification delivery and retry handling.
- `internal/post`: post jobs, variants, publish logs, and status FSM.
- `internal/scheduler`: in-process workers.
- `internal/storage`: storage abstraction and local driver.
- `internal/telegram`: notification client and callback types.
- `migrations`: embedded goose SQL migrations.
- `templates`: future channel template sources.

## Local Commands

- Format: `make fmt`
- Check formatting: `make fmt-check`
- Unit tests: `make test`
- Race tests: `make test-race`
- Vet: `make vet`
- Vulnerability scan: `make vuln` (requires `govulncheck`)
- Build: `make build`
- Local CI subset: `make ci`
- Start development Postgres: `make db-up`
- Stop development Postgres: `make db-down`
- Run the app: `make run dev` or `make run dev CONFIG=config.example.yaml`

CI runs formatting, `go vet ./...`, `go test -race -coverprofile=coverage.out ./...`, `govulncheck ./...`, `go build ./cmd/server`, and `docker build`.

## Development Notes

- Go version is declared in `go.mod`; use the module's configured Go toolchain.
- Keep generated or runtime files out of commits unless intentionally tracked. `storage/uploads/.gitkeep` is the only tracked upload placeholder.
- Use `go fmt`/`gofmt`; do not introduce custom formatting.
- Prefer table-driven tests for package-level behavior where that matches existing tests.
- Integration tests use PostgreSQL and `TEST_DATABASE_URL`; start a compatible database before running them directly.
- Migrations are goose SQL files and are embedded through `migrations/embed.go`.
- Config is strict: unknown YAML fields, invalid URLs/thresholds, and missing environment placeholders should remain startup errors.

## Workflow Rules

- Preserve the status machine in `internal/post/status.go`; invalid transitions should remain explicit failures.
- AI provider order is intentional: `tgpt` first, Ollama fallback. Provider failures should not crash the service; jobs should move through bounded retry states.
- Treat news text as untrusted input. Do not let article content alter system prompts or execution behavior.
- Do not log tokens, HMAC secrets, access tokens, callback secrets, or full authorization headers.
- Instagram `publish_enabled: false` is dry-run mode; keep tests and local workflows safe by default.
- Telegram callbacks require HMAC verification, timestamp freshness, and allowlisted users.
- Worker claims and publishing paths must stay idempotent enough to avoid duplicate publishing.

## Testing Expectations

- For small pure-Go changes, run `make test` at minimum.
- For concurrency, scheduler, outbox, publishing, or status-flow changes, run `make test-race`.
- For config, auth, callback, or external request changes, add or update focused tests.
- For DB behavior or migrations, run the relevant integration tests with `TEST_DATABASE_URL` and verify migrations from a clean database.
- Before handing off broad changes, prefer the CI-equivalent path: `make fmt-check`, `make vet`, `go test -race -coverprofile=coverage.out ./...`, `make build`.

## Security and Secrets

- Never commit `.env`, `config.yaml`, credentials, API tokens, access tokens, HMAC secrets, or real Instagram account identifiers.
- Use `config.example.yaml` and `.env.example` for documented placeholders only.
- Keep subprocess environment allowlists narrow; `tgpt` should not inherit application secrets by default.
- When adding logs, assume production logs may be shared and redact sensitive request details.

