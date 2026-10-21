# 0001. Module layout and dev tooling

- Status: Accepted
- Date: 2026-10-21

## Context

This repo holds one service (`git-svc/`) and its contract (`proto/git/v1/`). Team members work on Windows, macOS and Linux; CI and containers are Linux. We want `go test ./...` from the root to cover everything, and as few tools as possible to install by hand.

## Decision

- One Go module at the repo root: `github.com/git2-project/git2-codeplane`.
  - Service code lives under `git-svc/`. Its `internal/` packages cannot be imported from outside `git-svc/`.
  - Generated proto code lives under `gen/go/` and is importable by other repos (see ADR 0003).
- Dev tools (buf, protoc-gen-go, protoc-gen-go-grpc, golangci-lint) are pinned in a separate `tools.mod` and run with `go tool -modfile=tools.mod <tool>`. Keeping them out of `go.mod` stops their large dependency trees from changing the service's dependency versions.
- Makefile recipes are single commands with no shell syntax, so they run under `sh` and under `cmd.exe`. GNU make on Windows falls back to `cmd.exe` when no `sh` is on PATH.
- `.gitattributes` forces LF line endings.

## Consequences

- Prerequisites are only Go, git, Docker and make. The first `make lint` compiles golangci-lint (about a minute); later runs use the build cache.
- golangci-lint's docs recommend binary installs over `go tool`. The separate modfile removes their main concern (dependency conflicts). If it ever breaks, run the official binary or Docker image instead.
- If the repo later gains a second service, it can stay in this module or move to a `go.work` multi-module setup.
