# git2-codeplane

> The Git engine behind [git2](https://github.com/git2-project/git2): stores repositories and speaks the Git protocols.

**Repos:** [git2](https://github.com/git2-project/git2) · [git2-codeplane](https://github.com/git2-project/git2-codeplane) · [git2-runner](https://github.com/git2-project/git2-runner) · [git2-deploy](https://github.com/git2-project/git2-deploy)

## What lives here

| Path | What it is |
| :--- | :--- |
| `git-svc/` | The Git service: repo storage on disk, push and clone, diff, merge, branch protection |
| `proto/git/v1/` | This service's own gRPC API, consumed by the core services in [git2](https://github.com/git2-project/git2) |
| `migrations/` | A copy of git-svc's SQL migrations; the originals live in git2's `migrations/` |
| `docs/version_notes/` | What changed for the other lanes, newest first in [overview.md](docs/version_notes/overview.md) |

## Responsibilities

- **Smart HTTP**: `git clone` / `git push` over HTTPS.
- **SSH**: the same operations over SSH keys.
- **Git LFS**: large file storage, with objects kept in the instance bucket.
- **pre-receive**: server-side hooks that enforce branch protection and emit `repo.pushed` events.
- **Diff and merge**: the engine behind merge requests in Dash.

Built on `git` and `go-git`. Callers never touch repository files directly; they go through the `git/v1` gRPC API.

## Status

Early development, on the 9-week plan in the [org README](https://github.com/git2-project).

## Development

Open the repo in the devcontainer (VS Code → *Reopen in Container*). It has Go 1.26, git, psql and make, plus a throwaway PostgreSQL for the tests. Dev tools (buf, golangci-lint, grpcurl) are pinned in `tools.mod`.

```sh
cp env.example .env    # fill in POSTGRES_DSN (the same shared DSN as in the git2 repo)
make tests             # every test, including the Postgres ones
make run               # git-svc on :9090, Ctrl-C stops it
make lint
```

git-svc does not create its own tables: its schema comes from the git2 repo's migrations, applied after merge. [CLAUDE.md](CLAUDE.md) lists every variable and convention. Design decisions are in [docs/decisions](docs/decisions/).

## Contributing

- Work stays inside this repo's scope. Changes to shared contracts go through a PR approved by every affected owner.
- Every PR ships with tests and needs one human review before merge.
