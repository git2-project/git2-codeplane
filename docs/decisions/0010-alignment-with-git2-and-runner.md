# 0010. Alignment with the git2 and git2-runner repos

- Status: Accepted
- Date: 2026-10-26

## Context

A review of the git2 repo (auth-svc) and git2-runner found differences that cost time when people switch between repos and when services share a database. git2-runner listed two of them as assumption A-14: the Go version and the name of the database variable.

## Decision

Follow git2 and git2-runner in these points:

| Topic | Before | Now |
|---|---|---|
| Go | 1.27 | 1.26: `go 1.26.0` in `go.mod`. The devcontainer pins `golang:1.26.10-bookworm`, because buf in `tools.mod` needs 1.26.7 or newer and a floating `1.26` tag can be stale in a local cache |
| Database variable | `GIT2_DATABASE_URL` | `POSTGRES_DSN`, plus `POSTGRES_MAX_CONNS` (default 10, 1 to 1000) |
| Connection pool | pgx defaults | git2's settings and transaction-pooler handling (ADR 0006) |
| Schema changes | goose, run by git-svc at startup | SQL files in git2's `migrations/`, applied by a GitHub Action; a copy here (ADR 0006) |
| Development | tools on the host, `make dev-db` | the devcontainer (`.devcontainer/`): Go, git, psql, make, a throwaway PostgreSQL, repositories on a Docker volume |
| Postgres tests | testcontainers behind the `integration` build tag | `*_integration_test.go` files that skip unless `TEST_POSTGRES_DSN` is set. That is a throwaway database, never the shared one. `//go:build integration` stays for tests that need Docker. |
| Log keys | `"error"` | `"err"` |
| Make targets | `test`, `test-integration`, `migrate`, `dev-db` | `run`, `tests`, `build`, plus `lint`, `proto`, `proto-check` |
| Change notes | ADRs only | plus `docs/version_notes/` in git2-runner's format |
| Repo files | none | `env.example`, a pull request template |

Kept different on purpose:

- **Variable prefixes.** git-svc's own variables keep their `GIT2_` / `GIT2_GITSVC_` names (`GIT2_STORAGE_ROOT`, `GIT2_GITSVC_GRPC_ADDR`, …). They came with the original brief and belong to git-svc alone.
- **Repository IDs stay random UUID v4.** auth-svc uses v7. The directory shard is the first two hex characters of the ID, and v7's time-ordered prefix would put a day's repositories into one shard (ADR 0005).
- **Layout.** git-svc keeps `git-svc/cmd` and `git-svc/internal`. Each repo already differs here: git2 has `auth-svc/` plus a shared `internal/`, git2-runner puts everything under `app/`.
- **Config source.** git-svc reads only real environment variables. The devcontainer loads `.env` through `env_file`; git2's binary reads `.env` itself.

## Consequences

- Deployment files use `POSTGRES_DSN` for git-svc too. The brief's `GIT2_DATABASE_URL` is gone.
- The integration tests use `TEST_POSTGRES_DSN` instead of testcontainers, and run in the devcontainer without Docker-in-Docker.
- ADR 0001's notes about running make under `cmd.exe` still hold on a Windows host, but the devcontainer is the supported environment.
