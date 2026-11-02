# 0012. Git over HTTPS (smart HTTP) in git-svc

- Status: Accepted
- Date: 2026-10-31

## Context

WBS-5.2 asks that a standard `git` client clones and pushes over HTTPS, with integration tests that use real git (M1 demo, 13 Nov). It is on the critical path, and risk R-13 says the Git protocol may be harder than estimated; its mitigation is to use git's own plumbing instead of writing the protocol ourselves.

The gateway contract v0 (git2-deploy, `docs/api/gateway.md`) fixes the outside:
- `https://<instance>/<owner>/<repo>.git/*` is signed in with HTTP Basic;
- the gateway resolves the names to the repository ID and forwards to git-svc as `/<repo-id>.git/*`;
- it sets `X-Git2-User-Id` to the signed-in user and removes that header if a client sent it;
- it does not cap the body size, so push limits are git-svc's.

A push must also produce `git.v1.RepositoryPushed`, which ci-svc turns into pipelines.

## Decision

**The protocol is git's.** git-svc has its own HTTP handler. For every request it runs `git upload-pack` (clone, fetch) or `git receive-pack` (push) with `--stateless-rpc`, as git's own `http-backend` does, and streams the request body into git and git's output to the client. We did not choose:
- `git http-backend` behind Go's CGI handler, because git-svc needs to look repositories up by ID and to control limits, timeouts, logs and how a push ends;
- a Go implementation of the protocol (go-git), because of R-13.

**A port of its own.** The HTTP server listens on `GIT2_GITSVC_HTTP_ADDR` (default `:9080`), next to gRPC on `:9090`. It serves:

| Route | Purpose |
|---|---|
| `GET /<id>.git/info/refs?service=git-upload-pack` | refs, for clone and fetch |
| `POST /<id>.git/git-upload-pack` | clone and fetch |
| `GET /<id>.git/info/refs?service=git-receive-pack` | refs, for push |
| `POST /<id>.git/git-receive-pack` | push |
| `GET /healthz` | the process is up (Deploy's assumption B-04) |
| `GET /readyz` | the database answers a ping |

- Only the smart protocol is served. A request for the old "dumb" protocol (no `?service=`) gets `403`.
- A path names the repository only by ID. A malformed, unknown or deleted ID gets `404`, so a deleted repository is not served even if moving its directory to the trash failed.
- Errors are plain text: git prints them to the user as `remote: …`.

**Trust.** git-svc does not authenticate, as on its gRPC port. Only the gateway can reach the port.
- **Push** needs `X-Git2-User-Id` with a UUID; without it the answer is `403`.
- **Fetch** works without the header, and the header is logged when present.
- **Permissions** (may this user read or push here?) are checked before git-svc is called (assumption A-10).

**Protocol details.**
- The `Git-Protocol` header becomes git's `GIT_PROTOCOL`, so clients use protocol v2 for fetches.
- Request bodies may be gzip-compressed.
- Responses carry the same no-cache headers as `http-backend`.

**Push events through a hook.** git-svc runs `receive-pack` with `core.hooksPath` set to `<storage root>/hooks`. At startup it writes a `post-receive` hook there. After a push, git runs that hook with one line per updated ref, and the hook copies the lines to a file that git-svc created for that push. When `receive-pack` exits, git-svc:
1. reads the file;
2. checks each update with `git merge-base --is-ancestor` to set `forced`;
3. writes `RepositoryPushed` in a transaction that locks the repository row (`FOR SHARE`). A delete running at the same time waits, so its `RepositoryDeleted` comes after the push event. If the repository is already deleted, no event is written.

This gives three properties:
- Refs that the server rejected are never in the event, because git passes only the updated ones to the hook.
- A hook inside a repository directory never runs, because `core.hooksPath` replaces the repository's hooks.
- The hook is a two-line `sh` script, so pushes need a storage root on which files can be executed (not mounted `noexec`).

**The event is written after the push.** Git updates the refs on disk, then git-svc writes the event. The two cannot share a transaction.
- If git-svc stops, or its database fails, in between, the push has happened without an event. git-svc logs this at ERROR level (`RepositoryPushed is lost`).
- The opposite never happens: there is no event without a push.
- We considered writing the event from a `pre-receive` hook, before the refs change. Then a push that fails later would still have an event, and ci-svc would build a commit that no branch points to. A lost event is the smaller problem; a reconciler can find it later.

**A push finishes even if the client goes away.** Once git has the whole push, the push completes and is recorded:
- the push runs on a context owned by the server, not by the request;
- output for a client that is gone is dropped instead of failing git.

A clone or fetch stops as soon as its client disconnects.

**Limits and timeouts.**

| | Limit |
|---|---|
| Pack of one push | 2 GiB (`receive.maxInputSize`; git tells the client why the push failed) |
| Body of a fetch request, after decompression | 64 MiB, then `413`; it holds only the client's wants and haves, and `http-backend` allows 10 MB |
| One clone, fetch or push | 2 hours |
| Request headers | 10 s |
| Idle keep-alive connection | 2 minutes |
| Shutdown (SIGTERM) | in-flight requests get the shared 20 s; then git gets SIGTERM, which lets it remove a push's quarantined objects, and is killed 2 s later |

The limits are constants. They become configuration when someone needs to change them.

**No maintenance inside a push.** `receive.autogc` is off, so a push never starts `git gc`. Repositories are not repacked yet; a maintenance job that runs `git maintenance` outside requests is a TODO, next to the sweeper of ADR 0005.

## Consequences

- **Deploy** exposes port 9080 on the internal network and routes `/<owner>/<repo>.git/*` there. The gateway must:
  - stream bodies in both directions without buffering them;
  - forward `Content-Type`, `Content-Encoding`, `Git-Protocol` and `Accept`;
  - set `X-Git2-User-Id` and `X-Request-Id`.

  Probes can use `/healthz` and `/readyz`.
- **ci-svc** receives `RepositoryPushed` as soon as the outbox relay (WBS-1.3) exists.
- **Without a gateway** (development and tests), a push must set the header itself:
  ```sh
  git -c http.extraHeader="X-Git2-User-Id: <user-uuid>" push
  ```
- **Repositories grow** one pack per push until maintenance exists.
- **Tests** run real git on both sides over HTTP: without a database through fakes, and with PostgreSQL in both pgx modes. On a Windows host the hook runs through Git for Windows' `sh`.
