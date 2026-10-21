# CLAUDE.md

Guidance for Claude Code and for humans working in `git2-codeplane`.

## What lives here

The Git engine of [git2](https://github.com/git2-project/git2).

- `git-svc/`: a Go gRPC service that owns bare repositories on disk. Other services (gateway, dash-api, ci-svc, backup-svc in the `git2` repo) call it over gRPC and never touch repository files.
- `proto/git/v1/`: its gRPC contract (package `git.v1`). `gen/go/` holds the generated Go code, which is committed.
- `docs/decisions/`: ADRs. Read them before changing anything they cover.

Fixed architecture:
- gRPC between services. REST lives only at the gateway, not here.
- PostgreSQL schema `git_svc`, owned by this service alone. Events are written to `git_svc.outbox` in the same transaction as the data change.
- Git is the source of truth for objects and refs. The database holds metadata only, and commits are never stored in it.
- Anything that writes to a repository or speaks the Git protocol uses the `git` CLI (≥ 2.38) through `internal/gitcmd`. go-git is allowed later for read paths.
- git-svc does not use Redis.

## Prerequisites

- Go (version in `go.mod`), git ≥ 2.38, Docker (integration tests and `make dev-db`), GNU make.
- Nothing else. buf, protoc plugins and golangci-lint are pinned in `tools.mod` and run with `go tool -modfile=tools.mod <tool>`.

## Commands

Run from the repo root.

| Task | Command |
|---|---|
| Build | `make build` (output in `bin/`) |
| Unit tests | `make test` (`go test ./...`, no Docker) |
| Integration tests | `make test-integration` (`go test -tags=integration -count=1 ./...`, starts PostgreSQL with testcontainers) |
| One test | `go test ./git-svc/internal/gitcmd -run TestParseVersion` |
| Lint | `make lint` |
| Regenerate proto code | `make proto` |
| Local PostgreSQL | `make dev-db` / `make dev-db-stop` |
| Apply migrations | `make migrate` (needs `GIT2_DATABASE_URL`) |

Run locally:

```sh
make dev-db
export GIT2_DATABASE_URL='postgres://git2:git2@localhost:5432/git2?sslmode=disable'
export GIT2_STORAGE_ROOT="$PWD/.data"   # absolute path
export GIT2_GITSVC_DEV=true             # enables gRPC reflection
./bin/git-svc
```

| Variable | Default | Meaning |
|---|---|---|
| `GIT2_GITSVC_GRPC_ADDR` | `:9090` | gRPC listen address |
| `GIT2_DATABASE_URL` | required | PostgreSQL URL (never log it) |
| `GIT2_STORAGE_ROOT` | required | absolute path; holds `repositories/`, `tmp/`, `trash/`, `home/` |
| `GIT2_GITSVC_DEV` | `false` | dev-only features (gRPC reflection) |
| `GIT2_GITSVC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Layout

```
git-svc/cmd/git-svc     main: config, startup checks, gRPC server, graceful shutdown
git-svc/internal/
  config                env parsing and validation
  gitcmd                the only place that executes git
  storage               on-disk layout under GIT2_STORAGE_ROOT
  server                gRPC wiring, interceptors, error to status mapping
```

## Conventions

- **Running git.** Only through `gitcmd.Runner`: no shell, explicit arguments, sanitized env, per-call timeout.
  - User input is never placed in option position. Validate it, then attach it to its flag (`--initial-branch=`+b) or pass it after `--`.
  - Repository paths are derived from the repository ID only, never from names.
- **Errors.**
  - Wrap with context: `fmt.Errorf("create repository: %w", err)`.
  - Domain errors are sentinels, checked with `errors.Is` and `errors.As`.
  - Clients only see `InvalidArgument`, `NotFound`, `AlreadyExists` or a generic `Internal`. Never return paths, SQL or git stderr to a client; log them instead.
- **SQL.** Schema-qualify every table (`git_svc.repositories`); never rely on `search_path`. Domain change and outbox row go in one transaction.
- **Logging.** `log/slog`, JSON to stdout. Use key/value attrs, not formatted strings.
- **Style.** gofmt and goimports, with local imports grouped last. Every package and exported identifier has a doc comment.
- **Tests.**
  - Table tests for pure logic.
  - Use real git, not mocks, for anything touching repositories.
  - Integration tests use the `//go:build integration` tag.
- **Commits.** Conventional commits (`feat(storage): ...`, `fix: ...`, `docs: ...`), small and focused.
- **Decisions.** A decision not covered by existing docs gets a short ADR in `docs/decisions/NNNN-title.md`.

## Scope rules

- **Never edit other repositories** (`git2`, `git2-runner`, `git2-deploy`). If something there must change, write it down for the owner.
- **Contract changes** (`proto/git/v1/`, event payloads, outbox format) need approval from every affected owner before merge.
- **Every change ships with tests.** `make test`, `make lint` and, when relevant, `make test-integration` must pass.
- **Branches and review.** Work on a feature branch. Do not push, open PRs or merge unless asked. Every PR needs one human review.

## Windows notes

- **PowerShell 5.1 splits `-modfile=tools.mod` at the dot.** Quote it (`go tool "-modfile=tools.mod" buf lint`) or use `make`.
- **make.** GNU make without `sh` runs recipes with `cmd.exe`, so keep every recipe a single command with no shell syntax.
- **Line endings.** `.gitattributes` forces LF in the repo, whatever `core.autocrlf` says.
- **Paths in tests.** `t.TempDir()` paths are `C:\...`. Build `file://` URLs with `filepath.ToSlash` and a leading `/` (`file:///C:/...`).
