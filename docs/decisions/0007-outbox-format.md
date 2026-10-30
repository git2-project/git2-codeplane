# 0007. Outbox row format

- Status: Accepted
- Date: 2026-10-22

## Context

git-svc publishes domain events (`events.proto`) with a transactional outbox: the event row is written in the same database transaction as the change. In W2 a relay (WBS-1.3) will deliver pending rows to consumers such as ci-svc. The row format must be fixed now so both sides agree.

## Decision

`git_svc.outbox` columns:

| Column | Content |
|---|---|
| `id` | `bigserial`, the relay's reading order |
| `event_id` | random UUID per event; consumers use it to drop duplicates |
| `aggregate_type` | kind of entity: `repository` |
| `aggregate_id` | the entity's ID (the repository ID) |
| `event_type` | full protobuf message name, e.g. `git.v1.RepositoryCreated` |
| `payload` | the event message in canonical protobuf JSON (`jsonb`) |
| `created_at` | when the row was written |
| `published_at` | `NULL` until the relay delivers the event |

- **Payload encoding:** `protojson` with default options, so field names are lowerCamelCase (`ownerId`, `occurredAt`). Consumers decode with `protojson.Unmarshal` into the type named by `event_type`.
- **Timestamps:** `occurred_at` in the payload comes from the row the event describes (`created_at` or `deleted_at`), so the event and the data share one clock.
- **Transaction:** `outbox.Write` takes a `pgx.Tx`, not a pool. Writing an event outside the data transaction is a compile error.
- **Pending rows:** a partial index on `id WHERE published_at IS NULL` lets the relay find them without scanning delivered ones.

## Consequences

- An event exists if and only if its change was committed. Delivery is at least once, so consumers must be idempotent and use `event_id`.
- `id` values come from a sequence and are assigned before commit. A transaction that commits later can therefore hold a smaller `id` than one already published, and rolled-back transactions leave gaps. The relay must not assume that `id` order is commit order, nor that there are no gaps.
- Renaming a field in `events.proto` breaks consumers even though it is wire-compatible for gRPC, because JSON uses field names. `buf breaking` (FILE rules) catches renames.
- The table grows without bound until the relay (W2) and a cleanup of delivered rows exist.
