# 0016. Commit archives for CI jobs (CommitService.GetArchive)

- Status: Proposed; CI (Osman) reviews the contract with this change
- Date: 2026-11-15

## Context

CI jobs start in an empty `/workspace`. git2-runner's ADR 0004 decides how a job gets its source in v0:
- ci-svc fetches the pushed commit from git-svc over gRPC, as a tar archive;
- it hands the archive to the runner, which copies it into the job's volume through Docker's API;
- runners never talk to git-svc (assumption A-04).

Issue #13 asks for that RPC. Runner #19 and the M3 demo (11 Dec) wait for it.

## Decision

**One more RPC in CommitService.** `GetArchive(repository_id, ref)` reads a commit's content, like the other CommitService RPCs.
- It takes the same `ref` (ADR 0013): a branch, a tag, a full ref or a full SHA, and empty for the default branch. ci-svc sends the pushed commit's full SHA.
- A repository without commits answers `NOT_FOUND`, as `GetBlob` does.

**A server stream, like `GetRawBlob`.** The first message carries the commit's SHA and no data. The archive follows in messages of up to 64 KiB. Its size is not known before git finishes, so none is sent.

**`git archive --format=tar` of the commit, with two settings:**
- **`--worktree-attributes`.** By default, git archive applies the `.gitattributes` of the tree it archives:
  - `export-ignore` leaves files out;
  - `export-subst` rewrites `$Format:…$` placeholders;
  - `text` and `eol` attributes change line endings.

  Release tarballs want that, but a CI checkout does not: projects often mark `tests/` as export-ignore, and their tests would silently be missing. With `--worktree-attributes`, git reads attributes from the working tree instead, and a bare repository has none. git-svc writes no `info/attributes` and no global attributes file either, and git reads no system one (`GIT_ATTR_NOSYSTEM=1` on every git call), so the base image cannot add any. The archive holds the files exactly as committed.
- **`-c tar.umask=022`.** git's default umask (002) makes files group-writable. 022 gives the modes of a usual checkout: 0644, and 0755 for executables and directories.

Everything else is git archive's own format:
- files at the root, with no prefix;
- symlinks as symlinks, and a submodule as an empty directory;
- every entry with the committer time;
- a pax header that names the commit.

**Untrusted content.** git-svc does not run `fsck` on pushed objects (`receive.fsckObjects` is off). So a tree can hold names that git itself would refuse to check out: a push with an entry named `..` was accepted, and git archive wrote it as `sub/..`. Symlinks can also point anywhere.
- The archive is safe to extract only with a tool that never writes outside its target.
- Docker's copy into a container is such a tool. A hand-written extractor on the runner's host is not.
- The contract says so.

**Partial streams.** A tar cut between two entries reads as a complete archive in many readers: Go's `archive/tar` returns `io.EOF`. Only a stream that ends with `OK` holds the whole archive, and the contract tells clients to discard anything else.

**Limits.**
- No size limit, as #13 asks: ci-svc stops reading at its own limit.
- git gets 10 minutes, like `GetRawBlob`, and stops when the client goes away.
- git archive streams, so memory stays small whatever the repository's size.

## Consequences

- **CI (Osman):** ci-svc calls `GetArchive` with the pushed SHA. It gives the archive to the runner only when the stream ended with `OK`. The runner extracts it into `/workspace` with Docker's API.
- **Differences from a clone**, to note in git2-runner's `docs/api/ci-yaml.md`:
  - there is no `.git` directory, so `git describe` and `git log` fail (runner ADR 0004);
  - submodules are empty directories;
  - Git LFS files are pointers;
  - line-ending attributes (`eol=crlf`) and other checkout filters are not applied. On Linux runners this matters only for files with an explicit `eol=crlf`, such as `.bat` scripts.
- **Follow-up:** turning on `receive.fsckObjects` would refuse such trees at push time. It changes which pushes git-svc accepts, also for repositories with harmless old faults, so it needs a decision of its own.
- **Not done:** compression, archiving a single directory, and LFS content.
