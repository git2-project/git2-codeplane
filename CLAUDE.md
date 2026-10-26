# git2-codeplane — notes for Claude

The Git engine of [git2](https://github.com/git2-project/git2): **git-svc** stores bare repositories on disk and exposes them over gRPC. Other services (gateway, dash-api, admin-api, ci-svc, backup-svc) call it and never touch repository files. Owner: Apo (Abdullah, Git Core). Backup reviewer: Bilge.

Read `docs/decisions/` (what is decided and why) before changing anything they cover. The project-wide documents (charter, project plan, team map) are in the git2 repo under `docs/project_docs/`.

## Hard rules

- **PRs only.** Never commit to `main`. Work on a branch (`feat/…`, `fix/…`, `docs/…`, `chore/…`). Don't commit, push, open PRs or merge unless asked. Every PR gets one human review.
- **No migrations at runtime.** git-svc never changes the database schema. The schema lives in the git2 repo:
  - `migrations/NNN_git_<what>.sql`, first line a `-- ` comment, idempotent statements, no Down part. Our schema is `git_svc`.
  - A GitHub Action applies them to the shared dev DB after merge.
  - Keep a byte-identical copy in this repo's `migrations/`, which the tests use.
  - Never edit an applied migration or change the shared DB by hand (ADR 0006).
- **A doc with every change.** If anyone outside this repo can notice a change (proto, event, config, error, limit, behaviour, schema), the same PR adds a version note and a line in `docs/version_notes/overview.md`. The rules are in `docs/CLAUDE.md`.
- **Never edit other repositories** (`git2`, `git2-runner`, `git2-deploy`). When something there must change, write it as an action item in the version note.
- **Contracts.** `proto/git/v1/` is the source of truth for our gRPC API and events. A contract change needs approval from every affected owner.
- **Every change ships with tests.** `make tests` and `make lint` must pass.
- **Secrets** are never logged and never committed. `.env` is gitignored; `env.example` is the template.

## Stack

- Go 1.26, one module at the repo root (`github.com/git2-project/git2-codeplane`).
- gRPC with protobuf; buf for lint and codegen. Generated Go code in `gen/go/` is committed. REST lives only at git2's gateway.
- PostgreSQL through pgx/v5, on the team's shared dev DB (`POSTGRES_DSN`), schema `git_svc`. Events go to `git_svc.outbox` in the same transaction as the change.
- Git is the source of truth for objects and refs; the database holds metadata only and never stores commits. Anything that writes to a repository or speaks the Git protocol uses the `git` CLI (2.38 or newer) through `internal/gitcmd`. go-git is allowed later for read paths.
- git-svc does not use Redis.
- `log/slog` JSON logs.
- Dev tools (buf, protoc plugins, golangci-lint, grpcurl) are pinned in `tools.mod` and run with `go tool -modfile=tools.mod <tool>`.

## Layout

```
.devcontainer/          Go 1.26, git, psql, make + a throwaway PostgreSQL for tests
git-svc/cmd/git-svc     main: config, startup checks, gRPC server, graceful shutdown
git-svc/internal/
  config                env parsing and validation
  gitcmd                the only place that executes git
  storage               on-disk layout; create, trash and remove bare repositories
  db                    PostgreSQL pool (pooler-aware), schema check
  outbox                writes events into git_svc.outbox inside a transaction
  repo                  domain: validation, create/get/find/list/delete, SQL for repositories
  server                gRPC handlers, interceptors, error to status mapping
proto/git/v1/           the gRPC contract (source of truth)
gen/go/                 generated code (make proto)
migrations/             copy of our migrations in the git2 repo
docs/decisions/         ADRs
docs/version_notes/     one note per change other lanes must know about; overview.md is the index
env.example             template for .env
```

A request flows `server → repo → (storage, db, outbox)`. `repo` knows nothing about gRPC; `server` knows no business rules.

## Development

Open the repo in the devcontainer (VS Code → *Reopen in Container*). It provides Go 1.26, git, psql and make, plus a throwaway PostgreSQL (`postgres:5432`).
- That database loads `migrations/` on its first start, and `TEST_POSTGRES_DSN` points at it.
- Repositories live on a Docker volume at `/var/lib/git-svc` (`GIT2_STORAGE_ROOT`).

```bash
cp env.example .env    # POSTGRES_DSN: the same shared DSN as in the git2 repo
make tests             # every test; Postgres tests run because TEST_POSTGRES_DSN is set
make run               # git-svc on :9090, Ctrl-C stops it
```

Until the shared DB has the `git_svc` schema (the migration must be merged in git2 first), point `POSTGRES_DSN` at the test database: `postgres://git2:git2@postgres:5432/git2_test?sslmode=disable`.

| Task | Command |
|---|---|
| Run git-svc | `make run` |
| All tests | `make tests` (`go test -count=1 ./...`) |
| One test | `go test ./git-svc/internal/gitcmd -run TestParseVersion` |
| Build | `make build` (output in `bin/`) |
| Lint | `make lint` (golangci-lint, `buf lint`, `buf format` check) |
| Regenerate proto code | `make proto` (after every `.proto` change; commit `gen/` with it) |
| Check generated code is current | `make proto-check` (compares with the git index, so stage `gen/` first) |

Call the running service with grpcurl (needs `GIT2_GITSVC_DEV=true`, which turns on reflection):

```sh
go tool -modfile=tools.mod grpcurl -plaintext localhost:9090 list
go tool -modfile=tools.mod grpcurl -plaintext \
  -d '{"owner_id":"11111111-1111-4111-8111-111111111111","name":"payments"}' \
  localhost:9090 git.v1.RepositoryService/CreateRepository
```

| Variable | Default | Meaning |
|---|---|---|
| `POSTGRES_DSN` | required | PostgreSQL connection string, same name as in git2 (never log it) |
| `POSTGRES_MAX_CONNS` | `10` | connection pool size, 1 to 1000 |
| `TEST_POSTGRES_DSN` | unset | throwaway DB for Postgres tests; they skip when it is unset. Never the shared DB |
| `GIT2_STORAGE_ROOT` | required | absolute path; holds `repositories/`, `tmp/`, `trash/`, `home/` |
| `GIT2_GITSVC_GRPC_ADDR` | `:9090` | gRPC listen address |
| `GIT2_GITSVC_DEV` | `false` | dev-only features (gRPC reflection) |
| `GIT2_GITSVC_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

## Conventions

These match git2 and git2-runner, so all git2 services look alike (ADR 0010).

- **Running git.** Only through `gitcmd.Runner`: no shell, explicit arguments, sanitized env, per-call timeout.
  - User input is never placed in option position. Validate it, then attach it to its flag (`--initial-branch=`+b) or pass it after `--`.
  - Repository paths are derived from the repository ID only, never from names.
- **gRPC API** (ADR 0008):
  - Every RPC has its own `XRequest`/`XResponse`.
  - IDs are UUID strings; a malformed one is `INVALID_ARGUMENT`.
  - List RPCs paginate per AIP-158.
  - Every service, RPC, message and field has a comment (buf lint `COMMENTS`).
- **Errors.**
  - Wrap with context: `fmt.Errorf("create repository: %w", err)`.
  - Domain errors are sentinels, checked with `errors.Is` and `errors.As`.
  - Clients only see `INVALID_ARGUMENT`, `NOT_FOUND`, `ALREADY_EXISTS` or a generic `INTERNAL`. Never return paths, SQL or git stderr to a client; log them instead.
  - A database fault is `INTERNAL` or `UNAVAILABLE`, never `UNAUTHENTICATED`.
- **SQL.**
  - Schema-qualify every table (`git_svc.repositories`); never rely on `search_path`.
  - A state change and its outbox row go in one transaction.
  - The shared DB may be behind Supabase's transaction pooler (port 6543), which keeps no session state: no session advisory locks, no `LISTEN/NOTIFY`, no prepared statements.
  - Queries must also work in pgx's exec mode: send JSON as text with a cast (`$1::jsonb`), never as `[]byte`.
- **Logging.** `log/slog`, JSON to stdout, key/value attrs; errors under the key `err`.
- **Style.** gofmt and goimports, with local imports grouped last. Every package and exported identifier has a doc comment.
- **Tests.**
  - Table tests for pure logic. Use real git, not mocks, for anything touching repositories.
  - Postgres tests live in `*_integration_test.go` files and skip unless `TEST_POSTGRES_DSN` is set. They create the schema from `migrations/` and delete their own rows.
  - Tests that need Docker carry `//go:build integration`.
- **Commits.** Conventional commits (`feat(storage): ...`, `fix: ...`, `docs: ...`), small and focused.
- **Decisions.** A decision not covered by existing docs gets a short ADR in `docs/decisions/NNNN-title.md`.
- **Library APIs.** Check grpc-go, pgx and protobuf APIs against their docs or `go doc`, not memory.

## Working on a Windows host (not recommended)

The devcontainer is the supported environment. On a Windows host:

- **PowerShell 5.1 splits arguments like `-modfile=tools.mod` at the dot.** Quote them (`go tool "-modfile=tools.mod" buf lint`) or use `make`.
- **make.** GNU make without `sh` runs recipes with `cmd.exe`, so every recipe stays a single command with no shell syntax.
- **Line endings.** `.gitattributes` forces LF in the repo, whatever `core.autocrlf` says.
- **Paths in tests.** `t.TempDir()` paths are `C:\...`. Build `file://` URLs with `filepath.ToSlash` and a leading `/` (`file:///C:/...`).
- **Path length.**
  - During a push, git writes objects about 100 characters below the repository path, and Windows limits paths to 260 characters.
  - Keep `GIT2_STORAGE_ROOT` short (e.g. `C:\git2-data`), or set `core.longpaths = true` in a config file the receiving git reads (`GIT_CONFIG_GLOBAL`).
  - `git -c core.longpaths=true push` does not help for `file://` pushes: git does not pass `-c` options to the receiving side.
