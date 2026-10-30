# 0008. API conventions for git.v1

- Status: Accepted
- Date: 2026-10-22

## Context

The task sketch for contract v0 had `CreateRepository` return `Repository` and `DeleteRepository` return an empty message. Several details were left open: ID format, name matching, pagination and error codes. Consumers (gateway, dash-api, ci-svc) code against the contract before the implementation exists, so these rules must be fixed in the contract now.

## Decision

- **Request and response wrappers.** Every RPC has its own `XRequest` and `XResponse` message, as buf's STANDARD lint requires.
  - Create, Get and Find return `{ Repository repository = 1; }`.
  - Delete returns an empty `DeleteRepositoryResponse`, not `google.protobuf.Empty`.
  - Wrappers let us add response fields later without breaking callers.
- **Comments are mandatory.** buf lint runs the COMMENTS category, so every service, RPC, message and field must be documented.
- **IDs** are UUIDs as canonical strings. A malformed ID is `INVALID_ARGUMENT`, not `NOT_FOUND`.
- **Names** keep the case they were created with. Uniqueness and `FindRepository` are case-insensitive, so `Payments` and `payments` clash. The `.git` suffix ban is also case-insensitive.
- **Descriptions** are at most 2000 characters of valid UTF-8.
- **Pagination** follows Google AIP-158:
  - `page_size` 0 means 50; above 100 is treated as 100; negative is `INVALID_ARGUMENT`.
  - `page_token` is opaque and tied to the `owner_id` it was issued for.
  - Results are sorted by name, case-insensitive.
- **Errors.** `INVALID_ARGUMENT`, `NOT_FOUND` and `ALREADY_EXISTS` for caller problems; `INTERNAL` with a generic message for git-svc failures. No paths or internal details reach clients.
- **No authorization in git-svc.** Callers check permissions before calling (ADR 0002).
- **No disk paths in the API.** Repositories are identified only by ID.
- **Events** live in `events.proto`. The event type is the full message name (`git.v1.RepositoryCreated`) and the payload is canonical protobuf JSON. `RepositoryPushed` is defined but not emitted until push over HTTPS exists.

## Consequences

- Callers write `resp.GetRepository()` instead of using the response directly.
- Adding a field to a response or request is a compatible change. Renaming or renumbering fields is not and is caught by `buf breaking`.
- Clamping `page_size` means a caller asking for 1000 silently gets 100. Callers must follow `next_page_token` rather than assume one page holds everything.
