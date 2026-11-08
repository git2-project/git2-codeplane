package smarthttp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
	"github.com/git2-project/git2-codeplane/git-svc/internal/testdb"
)

// These tests serve repositories that repo.Service keeps in PostgreSQL
// (TEST_POSTGRES_DSN), once per pgx mode. They skip without the database.

func TestPushWritesRepositoryPushed(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			env, svc, pool := newDBEnv(t, mode)
			r, err := svc.Create(t.Context(), repo.CreateParams{OwnerID: newOwner(t, pool), Name: "payments"})
			if err != nil {
				t.Fatal(err)
			}
			work := env.clone(t, r.ID)
			sha := env.commit(t, work, "README.md", "hello\n")
			before := time.Now()
			env.gitOK(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")

			rows, err := pool.Query(t.Context(), `
SELECT aggregate_type, payload::text, published_at IS NULL
FROM git_svc.outbox
WHERE aggregate_id = $1 AND event_type = 'git.v1.RepositoryPushed'`, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			type event struct {
				aggregateType, payload string
				pending                bool
			}
			var events []event
			for rows.Next() {
				var e event
				if err := rows.Scan(&e.aggregateType, &e.payload, &e.pending); err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatalf("%d RepositoryPushed events, want 1", len(events))
			}
			e := events[0]
			if e.aggregateType != "repository" || !e.pending {
				t.Errorf("aggregate_type = %q, unpublished = %v; want repository, true", e.aggregateType, e.pending)
			}

			got := &gitv1.RepositoryPushed{}
			if err := protojson.Unmarshal([]byte(e.payload), got); err != nil {
				t.Fatalf("decode payload %s: %v", e.payload, err)
			}
			// occurred_at is the database's clock; allow for clock skew.
			if at := got.GetOccurredAt().AsTime(); at.Before(before.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
				t.Errorf("occurred_at = %v, want about now", at)
			}
			got.OccurredAt = nil
			want := &gitv1.RepositoryPushed{
				RepositoryId: r.ID.String(),
				PusherId:     env.user.String(),
				RefUpdates:   []*gitv1.RefUpdate{{Ref: "refs/heads/main", OldSha: zeroID, NewSha: sha, Forced: proto.Bool(false)}},
			}
			if !proto.Equal(got, want) {
				t.Errorf("payload = %v, want %v", got, want)
			}
		})
	}
}

func TestDeletedRepositoryIsNotServed(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			env, svc, pool := newDBEnv(t, mode)
			r, err := svc.Create(t.Context(), repo.CreateParams{OwnerID: newOwner(t, pool), Name: "payments"})
			if err != nil {
				t.Fatal(err)
			}
			work := env.clone(t, r.ID)
			env.commit(t, work, "a.txt", "1\n")

			if err := svc.Delete(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}

			cloneErr := env.gitFails(t, "", "clone", "--quiet", env.repoURL(r.ID), filepath.Join(t.TempDir(), "again"))
			pushErr := env.gitFails(t, work, "-c", env.userHeader(), "push", "--quiet", "origin", "HEAD:refs/heads/main")
			for _, err := range []string{cloneErr.Stderr, pushErr.Stderr} {
				if !strings.Contains(err, "repository not found") {
					t.Errorf("git stderr = %q, want 'repository not found'", err)
				}
			}
		})
	}
}

// newDBEnv is a testEnv whose repositories and pushes go through
// repo.Service and PostgreSQL.
func newDBEnv(t *testing.T, mode testdb.Mode) (*testEnv, *repo.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.Open(t, mode)
	var svc *repo.Service
	env := newTestEnv(t, func(o *Options) {
		svc = repo.NewService(pool, storage.NewStore(o.Layout, o.Git), o.Log)
		o.Repos, o.Pushes = svc, svc
	})
	return env, svc, pool
}

// newOwner returns a fresh owner ID and deletes the owner's repositories
// and their events when the test ends.
func newOwner(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	owner := uuid.New()
	t.Cleanup(func() {
		// t.Context is already canceled when cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, `
DELETE FROM git_svc.outbox
WHERE aggregate_id IN (SELECT id FROM git_svc.repositories WHERE owner_id = $1)`, owner)
		if err != nil {
			t.Errorf("delete events of owner %s: %v", owner, err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM git_svc.repositories WHERE owner_id = $1`, owner); err != nil {
			t.Errorf("delete repositories of owner %s: %v", owner, err)
		}
	})
	return owner
}
