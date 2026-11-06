package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/outbox"
)

// Forced says whether a ref update rewrote the ref's history.
type Forced int

const (
	// ForcedUnknown means git-svc could not tell: the objects are not commits,
	// or the check did not finish in time.
	ForcedUnknown Forced = iota
	// ForcedNo means a fast-forward, or a ref that was created or deleted.
	ForcedNo
	// ForcedYes means the update was not a fast-forward: the old object is
	// not an ancestor of the new one.
	ForcedYes
)

// proto returns the value of RefUpdate.forced in the event, which is left
// out when git-svc could not tell.
func (f Forced) proto() *bool {
	switch f {
	case ForcedYes:
		return proto.Bool(true)
	case ForcedNo:
		return proto.Bool(false)
	default:
		return nil
	}
}

// RefUpdate is one ref changed by a push. Object IDs are lowercase hex; an
// ID made only of zeros means "no object": the ref was created or deleted.
type RefUpdate struct {
	Ref    string
	OldSHA string
	NewSHA string
	Forced Forced
}

// Push is a push that changed refs of a repository.
type Push struct {
	RepositoryID uuid.UUID
	PusherID     uuid.UUID
	RefUpdates   []RefUpdate
}

// pushSQL locks the live repository row against a concurrent delete and
// returns the transaction's time.
const pushSQL = `
SELECT now()
FROM git_svc.repositories
WHERE id = $1 AND deleted_at IS NULL
FOR SHARE`

// RecordPush writes a RepositoryPushed event for a push that changed refs.
// A push without ref updates writes nothing.
//
// Git has already updated the refs on disk when this runs, so the event
// cannot share a transaction with the change: if git-svc stops in between,
// the push happened without an event (ADR 0012). The repository row is
// locked against a concurrent delete, so the event of a push always comes
// before the RepositoryDeleted of its repository. If the repository was
// deleted meanwhile, RecordPush writes nothing and returns ErrNotFound.
//
// The events of two pushes to one repository have no guaranteed order: a
// push can reach this point after a later one, whatever is locked here
// (ADR 0012). Consumers follow old_sha and new_sha instead.
func (s *Service) RecordPush(ctx context.Context, p Push) error {
	if len(p.RefUpdates) == 0 {
		return nil
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var at time.Time
		err := tx.QueryRow(ctx, pushSQL, p.RepositoryID).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock repository: %w", err)
		}

		event := &gitv1.RepositoryPushed{
			RepositoryId: p.RepositoryID.String(),
			PusherId:     p.PusherID.String(),
			OccurredAt:   timestamppb.New(at),
			RefUpdates:   make([]*gitv1.RefUpdate, 0, len(p.RefUpdates)),
		}
		for _, u := range p.RefUpdates {
			event.RefUpdates = append(event.RefUpdates, &gitv1.RefUpdate{
				Ref: u.Ref, OldSha: u.OldSHA, NewSha: u.NewSHA, Forced: u.Forced.proto(),
			})
		}
		return outbox.Write(ctx, tx, outbox.AggregateRepository, p.RepositoryID, event)
	})
	if err != nil {
		return fmt.Errorf("record push to repository %s: %w", p.RepositoryID, err)
	}
	return nil
}
