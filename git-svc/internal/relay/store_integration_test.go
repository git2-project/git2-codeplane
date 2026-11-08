package relay_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/outbox"
	"github.com/git2-project/git2-codeplane/git-svc/internal/relay"
	"github.com/git2-project/git2-codeplane/git-svc/internal/testdb"
)

// Other packages' tests write outbox rows into the same database at the
// same time, so these tests look only at their own rows (one random
// aggregate ID) and mark only those (ADR 0011).

func TestPGStorePendingAndMarkPublished(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			pool := testdb.Open(t, mode)
			store := relay.NewPGStore(pool)
			repoID := uuid.New()
			written := []proto.Message{
				&gitv1.RepositoryCreated{Id: repoID.String(), OwnerId: uuid.NewString(), Name: "payments", OccurredAt: timestamppb.Now()},
				&gitv1.RepositoryPushed{RepositoryId: repoID.String(), PusherId: uuid.NewString(), OccurredAt: timestamppb.Now(),
					RefUpdates: []*gitv1.RefUpdate{{Ref: "refs/heads/main", OldSha: zeros, NewSha: shaA, Forced: proto.Bool(false)}}},
				&gitv1.RepositoryPushed{RepositoryId: repoID.String(), PusherId: uuid.NewString(), OccurredAt: timestamppb.Now(),
					RefUpdates: []*gitv1.RefUpdate{{Ref: "refs/tags/v1", OldSha: zeros, NewSha: shaA}}},
			}
			for _, ev := range written {
				writeEvent(t, pool, repoID, ev)
			}

			got := ownPending(t, store, repoID)
			if len(got) != len(written) {
				t.Fatalf("Pending() has %d of our events, want %d", len(got), len(written))
			}
			for i, ev := range got {
				if i > 0 && ev.ID <= got[i-1].ID {
					t.Errorf("events not in ID order: %d after %d", ev.ID, got[i-1].ID)
				}
				if ev.EventID == uuid.Nil || ev.AggregateType != outbox.AggregateRepository || ev.AggregateID != repoID {
					t.Errorf("event %d: id %s, aggregate %s %s", i, ev.EventID, ev.AggregateType, ev.AggregateID)
				}
				if want := string(proto.MessageName(written[i])); ev.EventType != want {
					t.Errorf("event %d: type %q, want %q", i, ev.EventType, want)
				}
				if time.Since(ev.OccurredAt) > time.Minute || time.Until(ev.OccurredAt) > time.Minute {
					t.Errorf("event %d: occurred at %s, want about now", i, ev.OccurredAt)
				}
				// The payload is jsonb, so it comes back reformatted; it must
				// still decode into the message that was written.
				decoded := written[i].ProtoReflect().New().Interface()
				if err := protojson.Unmarshal([]byte(ev.Payload), decoded); err != nil {
					t.Errorf("event %d: payload %s does not decode: %v", i, ev.Payload, err)
				} else if !proto.Equal(decoded, written[i]) {
					t.Errorf("event %d: payload decodes to %v, want %v", i, decoded, written[i])
				}
			}

			first := []int64{got[0].ID, got[1].ID}
			if err := store.MarkPublished(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			if left := ownPending(t, store, repoID); len(left) != 1 || left[0].ID != got[2].ID {
				t.Errorf("after marking two, Pending() has %v of ours, want only the third", ids(left))
			}

			// Marking again changes nothing: the first published_at stays.
			before := publishedAt(t, pool, first)
			if err := store.MarkPublished(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			if after := publishedAt(t, pool, first); !slices.Equal(after, before) {
				t.Errorf("published_at changed from %v to %v on a second mark", before, after)
			}
		})
	}
}

const (
	zeros = "0000000000000000000000000000000000000000"
	shaA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// writeEvent writes ev the way git-svc does, and deletes the test's rows
// when it ends.
func writeEvent(t *testing.T, pool *pgxpool.Pool, aggregateID uuid.UUID, ev proto.Message) {
	t.Helper()
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		return outbox.Write(t.Context(), tx, outbox.AggregateRepository, aggregateID, ev)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `DELETE FROM git_svc.outbox WHERE aggregate_id = $1`, aggregateID)
		if err != nil {
			t.Errorf("clean up outbox: %v", err)
		}
	})
}

// ownPending returns the pending events of aggregateID. The limit is far
// above what concurrent tests leave pending.
func ownPending(t *testing.T, store *relay.PGStore, aggregateID uuid.UUID) []relay.Event {
	t.Helper()
	all, err := store.Pending(t.Context(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	var own []relay.Event
	for _, ev := range all {
		if ev.AggregateID == aggregateID {
			own = append(own, ev)
		}
	}
	return own
}

func publishedAt(t *testing.T, pool *pgxpool.Pool, ids []int64) []time.Time {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT published_at FROM git_svc.outbox WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[*time.Time])
	if err != nil {
		t.Fatal(err)
	}
	var out []time.Time
	for _, p := range got {
		if p == nil {
			t.Fatal("published_at is NULL after MarkPublished")
		}
		out = append(out, *p)
	}
	return out
}

func ids(events []relay.Event) []int64 {
	var out []int64
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}
