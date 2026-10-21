# Every recipe is a single command, with no shell syntax, so it runs the same
# under sh (Linux, macOS, Git Bash) and cmd.exe (GNU make on Windows without sh).

GO     ?= go
GOTOOL := $(GO) tool -modfile=tools.mod

# Local PostgreSQL for development. Matching URL:
#   GIT2_DATABASE_URL=postgres://git2:git2@localhost:5432/git2?sslmode=disable
DEV_DB_NAME  ?= git2-dev-postgres
DEV_DB_PORT  ?= 5432
DEV_DB_IMAGE ?= postgres:18-alpine

.PHONY: build test test-integration lint proto migrate dev-db dev-db-stop

## build: compile git-svc into bin/
build:
	$(GO) build -o bin/ ./git-svc/cmd/git-svc

## test: unit tests (no Docker needed)
test:
	$(GO) test ./...

## test-integration: unit + integration tests (needs Docker for PostgreSQL)
test-integration:
	$(GO) test -tags=integration -count=1 ./...

## lint: golangci-lint (Go) and buf lint (proto)
lint:
	$(GOTOOL) golangci-lint run ./...

## proto: lint the proto files and regenerate gen/go
proto:
	$(GOTOOL) buf lint
	$(GOTOOL) buf generate

## migrate: apply database migrations to GIT2_DATABASE_URL
migrate:
	$(GO) run ./git-svc/cmd/git-svc migrate

## dev-db: start a throwaway PostgreSQL container
dev-db:
	docker run -d --name $(DEV_DB_NAME) -e POSTGRES_USER=git2 -e POSTGRES_PASSWORD=git2 -e POSTGRES_DB=git2 -p $(DEV_DB_PORT):5432 $(DEV_DB_IMAGE)

## dev-db-stop: remove the PostgreSQL container and its data
dev-db-stop:
	docker rm -f $(DEV_DB_NAME)
