# 0005. Storage layout and repository lifecycle

- Status: Accepted
- Date: 2026-10-22

## Context

git-svc keeps every repository as a bare Git repository on disk and its metadata in PostgreSQL. A disk change and a database change cannot happen in one transaction, so the order of steps decides what a crash can leave behind. Paths must never be built from user input.

## Decision

**Layout.** Everything lives under `GIT2_STORAGE_ROOT`:

```
repositories/<shard>/<id>.git   live repositories
tmp/<id>                        repositories being created
trash/<id>.git                  deleted repositories, waiting to be purged
home/                           HOME for git processes
```

- `<shard>` is the first two hex characters of the repository ID, which spreads repositories over up to 256 directories.
- IDs are random (v4) UUIDs. Time-ordered v7 IDs would put all repositories created on the same day into the same shard.
- Paths are derived only from the ID. The path functions take a `uuid.UUID`, and every disk operation also checks that the path lies inside its directory.

**Create.**
1. Validate the input and generate an ID.
2. `git init --bare --initial-branch=<b>` into `tmp/<id>`.
3. Rename it to `repositories/<shard>/<id>.git`. The rename is atomic because both directories are on the same filesystem.
4. In one transaction, insert the row and the `RepositoryCreated` outbox event.
5. If the transaction fails, remove the directory. A unique-index violation becomes `ALREADY_EXISTS`.

Disk comes before the database, so in normal operation a row never points to a missing directory.

**Delete.**
1. In one transaction, soft-delete the row (`deleted_at`) and insert the `RepositoryDeleted` event.
2. After the commit, move the directory to `trash/<id>.git`.

If the move fails, the error is logged and the RPC still succeeds. The delete is already committed, and a retry would only get `NOT_FOUND`.

**Orphans** are accepted for now and marked `TODO(sweeper)` in the code:
- a directory without a row (crash between steps 3 and 4 of Create, or a failed cleanup);
- a deleted repository still in `repositories/` (failed move to trash);
- in a rare case, a row without a directory: if the connection drops during `COMMIT`, git-svc cannot know whether the commit happened. It treats it as failed and removes the directory.

A sweeper will reconcile disk and database later. Purging `trash/` also comes later; until then a deleted repository can be recovered by hand.

**One git-svc instance per storage root.** Nothing coordinates two processes writing the same root.

## Consequences

- A repository on disk is always complete or absent; a client never sees a half-initialized repository.
- Empty shard directories can remain after removals and moves. They are harmless, and there are at most 256 of them.
- Scaling out would need one storage root (volume) per git-svc instance plus routing by repository. High-availability Git storage is out of scope in the project plan.
- **Windows development:** during a push, git writes objects about 100 characters below the repository path, and Windows limits paths to 260 characters by default. Keep the storage root short on a Windows host (for example `C:\git2-data`) or enable `core.longpaths` in Git for Windows. Linux, the production target, has no such limit. In the devcontainer the storage root is a Docker volume, so this does not apply there.
