package repo_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
	"github.com/git2-project/git2-codeplane/git-svc/internal/testdb"
)

// RecordPush end to end, from a real push, is tested in package
// smarthttp. These tests cover what a push cannot easily arrange.

func TestRecordPushForDeletedRepository(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			svc, pool := newService(t, mode)
			r, err := svc.Create(t.Context(), repo.CreateParams{OwnerID: newOwner(t, pool), Name: "payments"})
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Delete(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}

			err = svc.RecordPush(t.Context(), repo.Push{RepositoryID: r.ID, PusherID: uuid.New(), RefUpdates: []repo.RefUpdate{
				{Ref: "refs/heads/main", OldSHA: strings.Repeat("0", 40), NewSHA: strings.Repeat("a", 40)},
			}})
			if !errors.Is(err, repo.ErrNotFound) {
				t.Errorf("RecordPush() error = %v, want ErrNotFound", err)
			}
			if n := countPushEvents(t, pool, r.ID); n != 0 {
				t.Errorf("%d RepositoryPushed events, want 0", n)
			}
		})
	}
}

func TestRecordPushWithoutUpdates(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			svc, pool := newService(t, mode)
			r, err := svc.Create(t.Context(), repo.CreateParams{OwnerID: newOwner(t, pool), Name: "payments"})
			if err != nil {
				t.Fatal(err)
			}

			if err := svc.RecordPush(t.Context(), repo.Push{RepositoryID: r.ID, PusherID: uuid.New()}); err != nil {
				t.Errorf("RecordPush() error = %v, want nil", err)
			}
			if n := countPushEvents(t, pool, r.ID); n != 0 {
				t.Errorf("%d RepositoryPushed events, want 0", n)
			}
		})
	}
}

// An update whose kind git-svc could not tell has no "forced" in the payload,
// so that a consumer can tell it from an update known not to be forced.
func TestRecordPushForcedStates(t *testing.T) {
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			svc, pool := newService(t, mode)
			r, err := svc.Create(t.Context(), repo.CreateParams{OwnerID: newOwner(t, pool), Name: "payments"})
			if err != nil {
				t.Fatal(err)
			}
			a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)

			err = svc.RecordPush(t.Context(), repo.Push{RepositoryID: r.ID, PusherID: uuid.New(), RefUpdates: []repo.RefUpdate{
				{Ref: "refs/heads/yes", OldSHA: a, NewSHA: b, Forced: repo.ForcedYes},
				{Ref: "refs/heads/no", OldSHA: a, NewSHA: b, Forced: repo.ForcedNo},
				{Ref: "refs/heads/unknown", OldSHA: a, NewSHA: b, Forced: repo.ForcedUnknown},
			}})
			if err != nil {
				t.Fatal(err)
			}

			// ->> reads a missing key as NULL.
			rows, err := pool.Query(t.Context(), `
SELECT u->>'ref', u->>'forced'
FROM git_svc.outbox, jsonb_array_elements(payload->'refUpdates') AS u
WHERE aggregate_id = $1 AND event_type = 'git.v1.RepositoryPushed'`, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]*string{}
			for rows.Next() {
				var ref string
				var forced *string
				if err := rows.Scan(&ref, &forced); err != nil {
					t.Fatal(err)
				}
				got[ref] = forced
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}

			if len(got) != 3 {
				t.Fatalf("%d ref updates in the payload, want 3: %v", len(got), got)
			}
			for ref, want := range map[string]string{"refs/heads/yes": "true", "refs/heads/no": "false", "refs/heads/unknown": ""} {
				switch forced := got[ref]; {
				case forced == nil && want != "":
					t.Errorf("%s: forced is missing, want %s", ref, want)
				case forced != nil && *forced != want:
					t.Errorf("%s: forced = %s, want %q", ref, *forced, want)
				}
			}
		})
	}
}

func newService(t *testing.T, mode testdb.Mode) (*repo.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.Open(t, mode)
	layout := storage.NewLayout(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	git, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	return repo.NewService(pool, storage.NewStore(layout, git), slog.New(slog.DiscardHandler)), pool
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

func countPushEvents(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM git_svc.outbox
WHERE aggregate_id = $1 AND event_type = 'git.v1.RepositoryPushed'`, id).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
