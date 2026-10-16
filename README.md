# git2-codeplane

> The Git engine behind [git2](https://github.com/git2-project/git2): stores repositories and speaks the Git protocols.

**Repos:** [git2](https://github.com/git2-project/git2) · [git2-codeplane](https://github.com/git2-project/git2-codeplane) · [git2-runner](https://github.com/git2-project/git2-runner) · [git2-deploy](https://github.com/git2-project/git2-deploy)

## What lives here

| Path | What it is |
| :--- | :--- |
| `git-svc/` | The Git service: repo storage on disk, push and clone, diff, merge, branch protection |
| `proto/git/v1/` | This service's own gRPC API, consumed by the core services in [git2](https://github.com/git2-project/git2) |

## Responsibilities

- **Smart HTTP**: `git clone` / `git push` over HTTPS.
- **SSH**: the same operations over SSH keys.
- **Git LFS**: large file storage, with objects kept in the instance bucket.
- **pre-receive**: server-side hooks that enforce branch protection and emit `repo.pushed` events.
- **Diff and merge**: the engine behind merge requests in Dash.

Built on `git` and `go-git`. Callers never touch repository files directly; they go through the `git/v1` gRPC API.

## Status

Early development, on the 9-week plan in the [org README](https://github.com/git2-project). Nothing here is ready to run yet; build and test commands will land in this file and in `CLAUDE.md` as the code does.

## Contributing

- Work stays inside this repo's scope. Changes to shared contracts go through a PR approved by every affected owner.
- Every PR ships with tests and needs one human review before merge.
