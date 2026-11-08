# 0014. The outbox relay: pushing events to EventSink subscribers

- Status: Accepted
- Date: 2026-11-08
- Agreed with: Osman (CI) on git2-runner #33; recorded on their side as git2-runner ADR 0010

## Context

git-svc writes its events to `git_svc.outbox` (ADR 0007), but nothing delivered them yet. ci-svc needs `RepositoryPushed` to start pipelines (WBS-1.3, issue #4), and other consumers will follow.

Fixed limits:
- Each service reads only its own Postgres schema (git2's `CLAUDE.md`), so consumers cannot read our outbox.
- The shared database may sit behind Supabase's transaction pooler: no `LISTEN/NOTIFY` and no session locks, so the relay polls.
- Delivery is at least once. Consumers drop repeats by `event_id`.

On git2-runner #33 we agreed on five points:
1. ci-svc decides which refs start CI; the relay passes events through without looking inside.
2. Producers push events to consumers over gRPC, and the list of consumers is producer configuration.
3. One contract, `events.v1.EventSink`, defined here in `proto/events/v1/`, with a pointer in git2's `contracts/events/`.
4. Fixed meanings for the answer codes, and a 10 s deadline. `Deliver` only records the event; the work happens afterwards.
5. ci-svc runs its own relay over `ci_svc.outbox` the same way, with a copy of this code for now.

## Decision

**Contract.** `events.v1.EventSink` has one RPC, `Deliver(DeliverRequest)`. The request carries the outbox row: `event_id`, `event_type`, `aggregate_type`, `aggregate_id`, `occurred_at` (the row's `created_at`, which equals the payload's `occurred_at`) and `payload` (protobuf JSON as text). The service keeps the name agreed with ci-svc instead of ending in `Service`, so `buf.yaml` excludes this one file from `SERVICE_SUFFIX`.

**Subscriptions.** `GIT2_GITSVC_EVENT_SINKS` lists the consumers: `host:port=type|type`, separated by commas, for example `ci-svc:9090=git.v1.RepositoryPushed`.
- A consumer receives only the types it lists. Every type must be one git-svc writes; anything else stops git-svc at startup.
- An event that no consumer subscribed to is marked published at once.
- When the variable is empty, the relay does not run and events wait in the outbox. Subscribing to a type later does not bring back events that were already marked.

**Answers.**

| Answer | What the relay does |
|---|---|
| `OK` | Delivered to this consumer |
| `INVALID_ARGUMENT`, `FAILED_PRECONDITION` | The consumer can never handle it. Log at ERROR, never send it to this consumer again |
| Any other code | Try again later, never drop |

This differs from the table first agreed on #33 in one place: **`UNIMPLEMENTED` is retried, not permanent.** gRPC also answers `UNIMPLEMENTED` when the server does not serve `EventSink` at all: an older consumer, a wrong port, or ci-svc before it builds `Deliver` (git2-runner #20, W4, after this relay). Treating it as permanent would mark every pending push delivered and drop it. A consumer that gets a type it does not handle answers `INVALID_ARGUMENT` instead; with subscriptions it should never happen. `UNKNOWN`, which a Go handler's plain error becomes, is retried for the same reason: nothing is dropped by mistake. Proposed to ci-svc in the review of git2-runner #42.

**Loop.**
- Every second, the relay reads up to 100 rows `WHERE published_at IS NULL ORDER BY id`. It sends each event to every subscriber of its type, then marks the finished ones in one `UPDATE`. When a batch was full and something was marked, it reads the next batch at once.
- There is no cursor. ADR 0007 warns that `id` order is not commit order: a row that commits late with a smaller `id` is simply picked up by the next pass.
- No transaction is open while the relay waits for a subscriber; each statement stands alone, which also suits the pooler.
- An event is finished when every subscriber of its type answered `OK` or refused it for good.

**Failures.**
- A retryable answer pauses that subscriber: 1 s after the first failure, doubling up to 1 minute, reset by the next answer that shows it is up. While it waits, its later events wait too, so a consumer that is down is not called once per event. Other subscribers carry on within the batch. Once a full batch (100) waits for the failing subscriber, the relay reads the same rows again, and later events wait until it recovers, also those of other subscribers and those nobody receives. With one subscriber today, only events nobody receives are marked later.
- `git_svc.outbox` has one `published_at` per row. If one subscriber stored an event and another failed, the event stays pending and the next attempt goes to both; the first drops the repeat by `event_id`. Per-subscriber marks would need a migration, and today there is only one subscriber.
- If git-svc stops after a subscriber stored an event but before the mark, the event is delivered again after the restart (FR-04). On SIGTERM the relay stops sending, still marks what it delivered, and only then does the database pool close.
- If the database fails, the relay logs it and tries again with the same growing pause. Events wait in the outbox meanwhile, and git-svc keeps serving.
- No row is locked. One git-svc runs per storage root (ADR 0005); if two relays ever overlap, an event is delivered twice, which consumers handle anyway.

**Connections.** Plain gRPC (`grpc.NewClient`, no TLS), because subscribers are reachable only on the instance network, as git-svc itself is.

## Consequences

- **CI (Osman).** ci-svc serves `events.v1.EventSink` on its internal port (git2-runner ADR 0010) and answers `INVALID_ARGUMENT`, not `UNIMPLEMENTED`, for a type it does not handle. Until it serves `EventSink`, events stay pending instead of being lost.
- **Deploy (Ceyda).** git-svc gets `GIT2_GITSVC_EVENT_SINKS` once ci-svc runs in the instance; until then the relay is off. git-svc must reach ci-svc's internal port, nothing new is published.
- **Backlog.** Events written before the relay first runs are delivered then, including those from earlier test pushes on the shared database. ci-svc treats a commit it cannot read (`NOT_FOUND` from `GetBlob`) as "no CI", so old pushes to deleted repositories start nothing.
- **Contract.** `events.v1` is a contract change and needs CI's approval. After the merge, a codeplane tag (`v0.3.0`) and a pointer in git2's `contracts/events/` follow.
- **Not done here.** Delivered rows are never deleted, so the outbox keeps growing (ADR 0007); a cleanup comes later. There are no metrics yet: a stuck subscriber shows only in the logs.
