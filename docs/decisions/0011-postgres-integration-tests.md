# 0011. Postgres integration tests

- Status: Accepted
- Date: 2026-10-27

## Context

The repository flows depend on PostgreSQL: the unique index decides duplicate names and concurrent creates, and the outbox row must be written in the data transaction. Unit tests cannot cover that. ADR 0010 replaced testcontainers with `TEST_POSTGRES_DSN`, a throwaway database that the devcontainer provides. The shared dev database may sit behind Supabase's transaction pooler, where pgx runs in exec mode; one outbox bug appeared only in that mode.

## Decision

- **Where.** Postgres tests live in `*_integration_test.go` files and skip when `TEST_POSTGRES_DSN` is not set, so `make tests` works everywhere and runs them in the devcontainer.
- **End to end.** The repository tests in `internal/server` wire git-svc as `main` does: `db.Open`, real git, a storage root in a temp dir, and the gRPC server on an in-memory listener (`bufconn`). They call the gRPC API and check the disk and the database directly.
- **Schema.** `testdb.Open` applies this repo's copy of `migrations/` before every test, each file in its own transaction, under a transaction-scoped advisory lock because packages run in parallel. The files are idempotent (ADR 0006), so this is cheap and also brings an older test database up to date.
- **Both pgx modes.** Every Postgres test runs twice: in pgx's default mode and in exec mode (`default_query_exec_mode=exec`), which is what `db.Open` selects behind a transaction pooler. There is no PgBouncer in the devcontainer; exec mode is the part of pooler behavior that changes how queries are sent.
- **Isolation.** Each test uses fresh random owner IDs and deletes the owner's rows (repositories and outbox events) when it ends. Tests never truncate tables and never depend on what else is in the database.

## Consequences

- No CI workflow runs these tests yet, so they run only when someone runs `make tests` in the devcontainer. The PR checklist asks for it.
- The tests write and delete rows. `TEST_POSTGRES_DSN` must never point at the shared database.
- Some pooler effects are not covered: PgBouncer itself, and session state that leaks between transactions. The code avoids session state instead (ADR 0006).
