# git2-codeplane: local development. Run inside the devcontainer, which
# provides Go, git, psql, the env from .env and a throwaway PostgreSQL for
# tests (see CLAUDE.md).
#
# Target names match the git2 repo: make run / make tests / make build.
# Every recipe is a single command with no shell syntax, so it also runs on
# a Windows host, where GNU make falls back to cmd.exe.

GO     ?= go
GOTOOL := $(GO) tool -modfile=tools.mod

.PHONY: run tests build lint proto proto-check

## run: run git-svc on GIT2_GITSVC_GRPC_ADDR (default :9090). Ctrl-C stops it.
run:
	$(GO) run ./git-svc/cmd/git-svc

## tests: every test; the Postgres ones run only when TEST_POSTGRES_DSN is set
tests:
	$(GO) test -count=1 ./...

## build: compile git-svc into bin/
build:
	$(GO) build -o bin/ ./git-svc/cmd/git-svc

## lint: golangci-lint (Go), buf lint and buf format (proto)
lint:
	$(GOTOOL) golangci-lint run ./...
	$(GOTOOL) buf lint
	$(GOTOOL) buf format -d --exit-code

## proto: lint the proto files and regenerate gen/go
proto:
	$(GOTOOL) buf lint
	$(GOTOOL) buf generate

## proto-check: fail if gen/go is out of date with proto/ (compares with the git index)
proto-check:
	$(GOTOOL) buf generate
	git diff --exit-code -- gen/
