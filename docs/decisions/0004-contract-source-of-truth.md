# 0004. Where the git.v1 contract lives

- Status: Accepted
- Date: 2026-10-21 (proposed), 2026-10-22 (accepted)

## Context

The project plan (WBS-1.1) says contracts v0 are "merged in `contracts/`" of the `git2` repo. This repo's README says `proto/git/v1/` here is git-svc's API. If both held a copy, the copies would drift.

## Decision

- **Source of truth:** `proto/git/v1/` in this repo, for the `git.v1` gRPC contract and its event messages.
- **`git2/contracts/` holds a reference, not a copy.** `contracts/git/README.md` there says where the contract lives, which tag to use, and how to consume it.
- **Consumers pin a tag of this repo,** for example `v0.1.0`:
  - Go services use the committed generated code (ADR 0003) with `go get`.
  - Mocks and other languages generate from the protos with a buf git input:
    `buf generate https://github.com/git2-project/git2-codeplane.git#tag=v0.1.0,subdir=proto`.
- **Contract changes** are made by PR in this repo and need approval from every affected owner (CLAUDE.md scope rules). After merge, a new tag is cut and the reference in `contracts/` is bumped.

## Consequences

- The contract changes in the same PR as the implementation, so code and contract cannot drift.
- `contracts/` still lists every service's contract in one place, as WBS-1.1 asks.
- Consumers need network access to GitHub (and repo access if it is private) to generate code or fetch the module.
- Breaking changes can be checked with `buf breaking` against the previous tag.
