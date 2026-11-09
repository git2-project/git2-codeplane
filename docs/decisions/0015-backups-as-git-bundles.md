# 0015. Backups: repositories as Git bundles (BundleService)

- Status: Proposed; Deploy (Ceyda) reviews the contract with this change
- Date: 2026-11-09

## Context

backup-svc (WBS-12.1, Ceyda, W4–W5) writes one encrypted snapshot of the whole instance, and `git2ctl restore` (WBS-12.3, W7–W8) puts it back on a new server. The snapshot format in git2-deploy (`docs/api/snapshot-format.md`) keeps one `repositories/<repo-id>.bundle` per repository, next to a `pg_dump` of the database. Its assumption B-01 asks git-svc for the bundles, because only git-svc touches repository files.

Fixed limits:
- backup-svc reads only its own schema, so it cannot list `git_svc.repositories` itself.
- The snapshot runs `pg_dump` first and bundles the repositories after it.
- The devcontainer and the git-svc image run git 2.39.

## Decision

**A separate service.** `BundleService` in `git.v1` (`bundle.proto`), with three RPCs. It is not part of `RepositoryService`:
- It reads repositories whatever their owner, and `RestoreBundle` writes a repository from the caller's data.
- Only backup-svc and `git2ctl restore` may call it; dash-api and the gateway must not.
- git-svc authorizes nobody (ADR 0008), so the rule cannot be enforced in code. A service of its own makes a wrong caller easy to see in review, and it can be blocked at the network later if that is ever needed.

It is named `BundleService`, not `BackupService`, because `backup.v1.BackupService` is backup-svc's own API.

**ListAllRepositories** lists the live repositories of every owner, sorted by ID.
- Paging is keyset-based on the primary key, so it stays correct while repositories are created and deleted.
- Pages are larger than ADR 0008's: 100 by default, at most 1000. A backup reads all of them, and the rows are small.
- A token carries a marker, so a `ListRepositories` token is refused here, and the other way round.
- Deleted repositories are not listed.

**CreateBundle** runs `git bundle create --quiet - --all` and streams the output in messages of up to 64 KiB, as `GetRawBlob` does.
- `--all` takes every ref under `refs/` (branches, tags, notes and any other) plus HEAD when HEAD points to a commit.
- Git reads the refs once, at the start, and packs what they reach. Pushes are not blocked:
  - A push stores its objects before it moves its refs (receive-pack's quarantine).
  - git-svc never runs gc (ADR 0012).
  - So a bundle never misses an object: it is the repository as it was when the bundle started.
- The size is not known before the stream ends. git-svc does not write a temporary file to find it out.

**Empty repositories.** Git refuses to bundle a repository without refs ("Refusing to create empty bundle"), and still writes 17 bytes of header.
- git-svc checks for refs first. An empty repository answers with one message, `empty = true`, and no data.
- backup-svc stores that as an empty file.
- `RestoreBundle` with no data creates an empty repository.

One rule covers both directions: zero bytes means an empty repository.

**RestoreBundle** is a client stream. The first message names the repository, and the data follows in order.
- **The row must exist and be live**, because the database is restored first; otherwise `NOT_FOUND`. HEAD comes from the row's `default_branch`, not from the bundle: a bundle has no HEAD when the default branch was never pushed.
- **The files must be absent**, otherwise `ALREADY_EXISTS`. This is checked before any data is read, and again before the rename. Directories appear only through an atomic rename (ADR 0005), so existing files are a complete repository, and a retry can count `ALREADY_EXISTS` as done.
- **Steps:**
  1. Save the stream to `tmp/<id>.restore-*/repository.bundle`. `git fetch` reads a bundle only from a file.
  2. Next to it, `git init --bare --initial-branch=<default_branch>`.
  3. `git fetch --no-write-fetch-head --no-auto-maintenance --recurse-submodules=no <file> '+refs/*:refs/*'`.
  4. Check that at least one ref arrived.
  5. Rename the repository into `repositories/`.

  The staging directory is removed in every case. The new repository has no remote and no `FETCH_HEAD`.
- **The fetch is the check.** `git bundle verify` reads only the header and passes a bundle that was cut short. The fetch does more:
  - git checks every object and the pack's checksum while it unpacks it;
  - the connectivity check makes sure every ref's objects arrived;
  - a missing prerequisite commit fails it.

  A bundle cut before its first ref, or one with only HEAD, fetches nothing without an error; the ref check catches it.
- **A failed fetch is `INVALID_ARGUMENT`**, and git's message goes to the log. Known limit: a server fault during the fetch, such as a full disk, is reported the same way; the log tells them apart.
- **No event and no hooks.** A restore is not a push: a `RepositoryPushed` would start CI for every restored repository. The new repository has only git's sample hooks, and git-svc's push hook is set only for receive-pack (ADR 0012).
- Two restores of the same ID use separate staging directories; the second one fails at the rename with `ALREADY_EXISTS`.

**Time and load.** There is no fixed timeout: the caller's deadline bounds each call, and git is stopped when the call ends. There is no limit on parallel calls; backup-svc bundles one repository at a time.

**Logging.** Streaming RPCs now get the same one-line log as unary ones, so every backup and restore shows in git-svc's log. This also covers `GetRawBlob`.

## Consequences

- **Deploy (Ceyda):**
  - backup-svc calls `ListAllRepositories`, then `CreateBundle` for each repository, after `pg_dump`.
  - A tar entry needs its size first, so backup-svc writes each bundle to a temporary file, as it must for the `pg_dump` output anyway.
  - `git2ctl restore` loads the database, then calls `RestoreBundle` for each bundle.
  - backup-svc and the restore need to reach git-svc's gRPC port on the instance network.
- **The pg_dump window.** The rows come from the moment of `pg_dump`, and the bundles from a little later.
  - A repository **created** in between is bundled but has no row. `RestoreBundle` answers `NOT_FOUND`, and the restore skips it. No orphan directory is created, so the sweeper the snapshot format mentions is not needed for this case.
  - A repository **deleted** in between has a live row but no bundle. After the bundles, `git2ctl restore` can call `ListAllRepositories` and send `RestoreBundle` with no data for each repository: those already restored answer `ALREADY_EXISTS`, and the missing ones come back empty. Deploy decides whether to do this.
- **Disk.** A restore needs free space under the storage root for the bundle and the repository at the same time, about twice the repository's size.
- **`tmp/`** now also holds restores in progress (`tmp/<id>.restore-*`), next to creates (ADR 0005). A crash can leave one behind, like a create (`TODO(sweeper)`).
- **Not done:**
  - bundles of deleted repositories in `trash/`;
  - incremental bundles;
  - a size limit;
  - a checksum in the stream, since the snapshot's manifest keeps a sha256 per file.
