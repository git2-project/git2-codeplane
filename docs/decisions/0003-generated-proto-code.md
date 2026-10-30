# 0003. Generated proto code is committed

- Status: Proposed (needs consumers' owners)
- Date: 2026-10-21

## Context

The `git.v1` contract is used by git-svc (server) and by Go services in the `git2` repo (clients). Either we commit the generated Go code here, or every consumer runs buf against our `.proto` files.

## Decision (proposed)

- Commit generated Go code in `gen/go/git/v1` (package `gitv1`).
- `make proto` regenerates it.
- A check fails when the generated code is stale: `buf generate` followed by `git diff --exit-code gen/`. This becomes a CI step once CI exists.
- `.proto` files carry no `go_package`; buf's managed mode sets it. The proto files stay language-neutral.

## Consequences

- Go consumers only need `go get github.com/git2-project/git2-codeplane@<version>` and import `.../gen/go/git/v1`. They don't need buf or protoc.
- Diffs include generated files. `.gitattributes` marks them `linguist-generated`, so GitHub collapses them.
- Non-Go consumers can still generate from `proto/` with buf.
