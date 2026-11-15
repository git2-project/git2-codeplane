package relay

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pendingSQL reads the oldest unpublished rows. The partial index
// outbox_unpublished_idx covers it, so delivered rows are never scanned.
// No row is locked: one git-svc runs per storage root (ADR 0005), and if
// two relays ever overlap, an event is delivered twice, which consumers
// handle anyway.
const pendingSQL = `
SELECT id, event_id, event_type, aggregate_type, aggregate_id, payload::text, created_at
FROM git_svc.outbox
WHERE published_at IS NULL
ORDER BY id
LIMIT $1`

const markSQL = `
UPDATE git_svc.outbox
SET published_at = now()
WHERE id = ANY($1) AND published_at IS NULL`

// PGStore is the Store over git_svc.outbox. Each method is one statement,
// so no transaction is held while the relay waits for subscribers.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store that uses pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// Pending implements Store.
func (s *PGStore) Pending(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx, pendingSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("query outbox: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.EventID, &e.EventType, &e.AggregateType, &e.AggregateID, &e.Payload, &e.OccurredAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	return events, nil
}

// MarkPublished implements Store.
func (s *PGStore) MarkPublished(ctx context.Context, ids []int64) error {
	if _, err := s.pool.Exec(ctx, markSQL, ids); err != nil {
		return fmt.Errorf("update outbox: %w", err)
	}
	return nil
}
