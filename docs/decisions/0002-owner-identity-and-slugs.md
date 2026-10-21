# 0002. Owner identity and URL slugs

- Status: Proposed (needs gateway and auth-svc owners)
- Date: 2026-10-21

## Context

git-svc stores only `owner_id`, a UUID issued by auth-svc for a user or organization. Git URLs will use human slugs: `https://git2.example.com/acme/payments.git`. Something must turn `acme` into an `owner_id` and `payments` into a repository ID. The caller must also be authorized for that repository.

Options:

1. **Gateway resolves.** The gateway asks auth-svc for `acme`'s ID, then calls `FindRepository(owner_id, "payments")`, checks permissions and proxies to git-svc as `/<repo-id>.git/...`.
2. **Denormalized slug in git-svc.** git-svc stores `owner_slug` and updates it from auth-svc events (user or org renamed).
3. **git-svc calls auth-svc** on each request to resolve the slug.

## Decision (proposed)

Option 1. The gateway already authenticates every request, and it must ask auth-svc about the caller's access to the owner anyway, so it can resolve the slug in the same step. git-svc stays ID-only: it has no dependency on auth-svc and no rename handling. Smart HTTP in git-svc is addressed as `/<repo-id>.git/...`.

## Consequences

- The gateway needs a slug lookup on every Git HTTP request. It can be cached (Redis, short TTL).
- Renaming an owner needs no change in git-svc.
- Name lookups go through `FindRepository`, which is case-insensitive, matching the unique index.
- If the team picks option 2 instead, it is an additive change: a new column, an event consumer and a new RPC.
