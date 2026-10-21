# 0004. Where the git.v1 contract lives

- Status: Proposed (team decision, affects WBS-1.1)
- Date: 2026-10-21

## Context

The project plan (WBS-1.1) says contracts v0 are "merged in `contracts/`". This repo's README says `proto/git/v1/` here is git-svc's API. Both can't be the source of truth.

## Decision (proposed)

`proto/git/v1/` in this repo is the source of truth for the `git.v1` gRPC contract and its event messages.

- The contract changes in the same PR as the implementation, so they cannot drift.
- Contract changes still need approval from every affected owner (CLAUDE.md scope rules).
- `git2/contracts/` can reference it without copying, through a buf `git` input or a buf workspace pointing at a tag of this repo. If the team prefers one directory for everything, a copy-and-check job is the fallback.

## Consequences

- One place to review and version git-svc's API. Breaking changes can be checked with `buf breaking` against `main`.
- Mock servers for WBS-1.1 are generated from this repo's protos.
- If the team decides on `contracts/` as the only source, this repo will consume generated code from there, and `proto/` moves out in one PR.
