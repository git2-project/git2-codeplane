# 0013. Reading repository content: RefService and CommitService

- Status: Accepted, pending Bilge's review of the code-browsing part
- Date: 2026-11-03

## Context

Dash's code browser (WBS-6.1, Bilge, W2–W3, zero float) shows branches, tags, directories, files and commits. The dash-api contract v0 (git2 PR #17, `contracts/dash-api/openapi.yaml`) defines its endpoints: `/branches`, `/tags`, `/tree`, `/blob`, `/raw`, `/commits` and `/commits/{sha}`. Only git-svc may touch repository files, so each endpoint needs a git-svc RPC behind it. ci-svc also needs to read one file (`.git2-ci.yml`) at a commit.

The dash-api contract left some details open. Dash had no code-browser PR open yet, so we assumed the answers below and ask Bilge to review them with this change.

## Decision

**Two services**, as announced in `repository.proto` since W1:
- `RefService` (`ref.proto`): `ListBranches`, `ListTags`.
- `CommitService` (`commit.proto`): `GetTree`, `GetBlob`, `GetRawBlob` (server streaming), `ListCommits`, `GetCommit`.

Each request names the repository by ID; dash-api resolves owner and name and checks access first, as for `RepositoryService`.

**Assumed with Dash (to review):**
1. **Order of work.** Branches and directories first, then files, then commits. All seven RPCs ship together in this change.
2. **Language detection** (`language` in the dash-api `Blob`) is dash-api's job: it is a UI concern, decided from the file name.
3. **Diff limits.** At most 300 files per commit, as in the dash-api contract. Patches are cut at **64 KiB per file and 1 MiB per commit**. A cut file has `truncated = true` and no patch, and once the commit's 1 MiB is used up, git is stopped and the remaining files are cut too.
4. **No copy detection.** Renames are detected (git's default 50% similarity); copies are not, so `FILE_STATUS_COPIED` is reserved but never returned. Copy detection reads every unchanged file and is slow.
5. **No `--follow` in history.** `ListCommits` with a path does not follow renames: it does not work for directories and breaks paging.
6. **Tag order.** Newest first by the tag's date (annotated) or its commit's date (lightweight), then by name. Branches: the default branch first, then by name in byte order.

Commit IDs are 40-character SHA-1, as the dash-api contract assumes: git-svc creates every repository with SHA-1 and a push cannot change that.

**Refs.** `ref` is a branch, a tag, `refs/heads/…`, `refs/tags/…` or a full SHA; empty means the default branch. A branch wins over a tag with the same name (git itself prefers the tag; the code browser's branch picker should win). Revision syntax such as `main~1`, `main:path` or `@{…}` is rejected, which keeps every ref a plain name. Each answer returns the commit it read, so links stay stable when a branch moves.

A short SHA works only in `GetCommit`, not as a `ref`: links are built from the full SHA that answers return, and a prefix would add its own rules (a branch named like one, a prefix that matches several objects). Accepting it later breaks nobody, so it can wait for a real need.

**Paths** are relative, without empty, `.` or `..` parts, at most 4096 bytes. They reach git only after `--` and with `--literal-pathspecs`, so `*` or `[1]` are file names, not patterns.

**Answers built for the UI:**
- `GetTree`: one directory, directories first, at most 10 000 entries (`truncated`), with the last commit that changed the directory. A repository without commits answers `empty = true` instead of an error.
- `GetBlob`: up to 1 MiB of content, `truncated` beyond. `binary` is git's own test (a NUL in the first 8000 bytes); binary files come without content. `lfs` marks a Git LFS pointer.
- `GetRawBlob` streams any file in messages of at most 64 KiB; the first message carries size and name.
- Contents and patches are `bytes`: files are not always UTF-8. Names and messages are strings; bytes that are not UTF-8 become U+FFFD.

**Paging.** `page_size` and tokens follow ADR 0008. Ref tokens hold the last ref's sort key, so pages stay correct when refs change. Commit tokens hold the first page's commit and an offset into its history, which never changes, so later pages ignore new pushes.

**Git, not go-git.** Every read runs git plumbing through `gitcmd` (`for-each-ref`, `rev-parse`, `ls-tree`, `cat-file`, `log`, `diff-tree`), like the rest of git-svc. Large reads stream: `GetBlob` stops git after 1 MiB, and `GetCommit` stops git once its patches are full, so memory stays bounded whatever the file or commit size. One git process per step is fast enough for a code browser; a long-running `git cat-file --batch` can come later if profiling asks for it.

**Splitting patches.** `GetCommit` lists the changed files with `diff-tree --raw --numstat`, then reads `diff-tree -p` once and splits it at its `diff --git` lines, in the same order. A type change (file to symlink) prints two sections, which both go to one file. Each section's header is checked against the expected paths, so a mismatch fails the request instead of attaching a patch to the wrong file.

## Consequences

- dash-api implements the code browser on these RPCs. The `Blob.language`, `Signature.user` and `Branch.protected` fields of its contract are filled by dash-api (`protected` is false until WBS-7.1).
- ci-svc can read `.git2-ci.yml` with `GetBlob` (`ref` = the pushed commit).
- The diff code is the base for `DiffService` (WBS-8.1, W4): comparing branches uses the same parsing and limits.
- The default branch does not exist until something is pushed to it. A repository with commits only on other branches (`git push origin master` when the default is `main`) is not empty, but an empty `ref` answers `NOT_FOUND`, and Dash offers the branches instead. Moving HEAD on the first push would be a new rule for every repository, so it is not done here.
- A file or ref whose name is not UTF-8 is listed with U+FFFD and cannot be opened. git2 does not create such names; only a push can.
- Creating and deleting branches and tags through the API is still to come; today they change only by push.
