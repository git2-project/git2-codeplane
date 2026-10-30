# 0006. Database schema and migrations

- Status: Accepted (revised 2026-10-26: migrations moved to the git2 repo)
- Date: 2026-10-22, revised 2026-10-26

## Context

git-svc owns the PostgreSQL schema `git_svc`. On 2026-10-26 the team decided that no repository runs migrations on its own: every service's migrations live in the git2 repo, and a GitHub Action applies them to the shared database after a merge to `main`.

The shared dev database is a Supabase project, often reached through its transaction pooler (port 6543). The pooler hands each transaction to any server connection, so it keeps no session state.

## Decision

- **Migrations live in git2** as plain SQL files: `migrations/NNN_git_<what>.sql`. git2's rules apply:
  - the first line is a `-- ` comment, which `make show-migrations` prints;
  - an applied file is never edited; a change gets a new file;
  - git2's runner applies each file in one transaction and records it in `public.schema_migrations`, so there are no Down migrations;
  - statements are idempotent (`IF NOT EXISTS`), like git2's own.
- **A copy stays here.** This repo keeps a byte-identical copy in `migrations/`. Only the tests and the devcontainer's test database read it. When the git2 file changes (for example it is renumbered before merge), the copy changes with it.
- **git-svc never changes the schema.** At startup it checks that its tables exist and refuses to start otherwise, naming the missing tables.
- **Database access works through a transaction pooler:**
  - The pool settings and the pooler detection are the same as git2's `internal/database`. Behind a pooler, pgx uses exec mode: no server-side prepared statements.
  - No session state: no session advisory locks, no `LISTEN/NOTIFY`, no `SET` outside a transaction.
  - Arguments must work in exec mode, where pgx infers parameter types from Go types. JSON is sent as text and cast (`$5::jsonb`), never as `[]byte`, which would travel as `bytea`.

The first version of this ADR used goose with embedded migrations, run at startup under a session advisory lock. It was replaced before it was merged.

## Consequences

- A table or column needs a PR on git2 and its review; it cannot ship from this repo alone.
- Until the git2 PR is merged and applied, the shared dev database has no `git_svc` tables, so git-svc cannot start against it. Use the devcontainer's test database meanwhile; it loads the copy on its first start.
- The migration number is provisional until the merge: if another lane's migration merges first, ours is renumbered and the copy renamed.
- git-svc's runtime database role needs only data privileges: `USAGE` on the schema, `SELECT, INSERT, UPDATE` on its tables and `USAGE` on the outbox's id sequence. The GitHub Action's role runs the DDL.
