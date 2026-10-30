package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
	"github.com/git2-project/git2-codeplane/git-svc/internal/testdb"
)

// These tests run git-svc wired as in main: PostgreSQL from
// TEST_POSTGRES_DSN, real git, a storage root in a temp dir, and the gRPC
// server on an in-memory listener. Each test runs once per pgx mode
// (testdb.Modes) and uses fresh owner IDs, so tests never see each other's
// rows. They skip when TEST_POSTGRES_DSN is not set.

func TestCreateRepository(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		owner := env.newOwner(t)
		git := newGitClient(t)

		tests := []struct {
			req        *gitv1.CreateRepositoryRequest
			wantBranch string
			wantDesc   string
		}{
			{&gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments"}, "main", ""},
			{&gitv1.CreateRepositoryRequest{
				OwnerId: owner, Name: "Api.v2", DefaultBranch: proto.String("trunk"), Description: proto.String("REST API"),
			}, "trunk", "REST API"},
		}
		for _, tt := range tests {
			r := env.create(t, tt.req)
			id, err := uuid.Parse(r.GetId())
			if err != nil || id.Version() != 4 {
				t.Errorf("id = %q, want a random (v4) UUID", r.GetId())
			}
			if r.GetOwnerId() != owner || r.GetName() != tt.req.GetName() ||
				r.GetDefaultBranch() != tt.wantBranch || r.GetDescription() != tt.wantDesc {
				t.Errorf("created %v, want name %q, default branch %q, description %q",
					r, tt.req.GetName(), tt.wantBranch, tt.wantDesc)
			}
			created := r.GetCreatedAt().AsTime()
			if created.IsZero() || !created.Equal(r.GetUpdatedAt().AsTime()) {
				t.Errorf("created_at = %v, updated_at = %v, want equal and set", created, r.GetUpdatedAt().AsTime())
			}

			path := env.layout.RepoPath(id)
			if got := gitOut(t, git, path, "rev-parse", "--is-bare-repository"); got != "true" {
				t.Errorf("is-bare-repository = %q, want true", got)
			}
			if got := gitOut(t, git, path, "symbolic-ref", "HEAD"); got != "refs/heads/"+tt.wantBranch {
				t.Errorf("HEAD = %q, want refs/heads/%s", got, tt.wantBranch)
			}
			gitOut(t, git, path, "fsck", "--strict")

			got, err := env.client.GetRepository(t.Context(), &gitv1.GetRepositoryRequest{Id: r.GetId()})
			if err != nil {
				t.Fatalf("GetRepository() error = %v", err)
			}
			if !proto.Equal(got.GetRepository(), r) {
				t.Errorf("GetRepository() = %v, want %v", got.GetRepository(), r)
			}
		}
		assertEmptyDir(t, env.layout.TmpDir())
	})
}

func TestCloneAndPushOverFile(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		r := env.create(t, &gitv1.CreateRepositoryRequest{
			OwnerId: env.newOwner(t), Name: "payments", DefaultBranch: proto.String("trunk"),
		})
		bare := env.layout.RepoPath(uuid.MustParse(r.GetId()))
		git := newGitClient(t)
		work := t.TempDir()

		first := filepath.Join(work, "first")
		gitOut(t, git, "", "clone", "--quiet", fileURL(bare), first)
		if err := os.WriteFile(filepath.Join(first, "README.md"), []byte("hello\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitOut(t, git, first, "add", "README.md")
		gitOut(t, git, first, "-c", "user.name=Test", "-c", "user.email=test@example.com",
			"commit", "--quiet", "-m", "first commit")
		commit := gitOut(t, git, first, "rev-parse", "HEAD")
		gitOut(t, git, first, "push", "--quiet", "origin", "HEAD:refs/heads/trunk")

		if got := gitOut(t, git, bare, "rev-parse", "refs/heads/trunk"); got != commit {
			t.Errorf("trunk = %s after push, want %s", got, commit)
		}
		gitOut(t, git, bare, "fsck", "--strict")

		// A new clone checks out the default branch, with the pushed file.
		second := filepath.Join(work, "second")
		gitOut(t, git, "", "clone", "--quiet", fileURL(bare), second)
		if got := gitOut(t, git, second, "rev-parse", "--abbrev-ref", "HEAD"); got != "trunk" {
			t.Errorf("checked-out branch = %q, want trunk", got)
		}
		content, err := os.ReadFile(filepath.Join(second, "README.md"))
		if err != nil || string(content) != "hello\n" {
			t.Errorf("README.md = %q (error: %v), want %q", content, err, "hello\n")
		}
	})
}

func TestDuplicateNameIsCaseInsensitive(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		owner := env.newOwner(t)
		first := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "Payments"})

		for _, name := range []string{"Payments", "payments", "PAYMENTS"} {
			_, err := env.client.CreateRepository(t.Context(), &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: name})
			if status.Code(err) != codes.AlreadyExists {
				t.Errorf("CreateRepository(%q) error = %v, want AlreadyExists", name, err)
			}
		}

		// Another owner may use the same name.
		other := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: env.newOwner(t), Name: "payments"})

		// The failed creates left nothing on disk.
		assertRepoDirs(t, env.layout, first.GetId(), other.GetId())
		assertEmptyDir(t, env.layout.TmpDir())
	})
}

func TestFindRepository(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		owner := env.newOwner(t)
		created := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "Payments"})

		for _, name := range []string{"Payments", "payments", "PAYMENTS"} {
			resp, err := env.client.FindRepository(t.Context(), &gitv1.FindRepositoryRequest{OwnerId: owner, Name: name})
			if err != nil {
				t.Errorf("FindRepository(%q) error = %v", name, err)
				continue
			}
			if !proto.Equal(resp.GetRepository(), created) {
				t.Errorf("FindRepository(%q) = %v, want %v", name, resp.GetRepository(), created)
			}
		}

		for _, req := range []*gitv1.FindRepositoryRequest{
			{OwnerId: owner, Name: "billing"},
			{OwnerId: uuid.NewString(), Name: "payments"}, // someone else's name space
		} {
			_, err := env.client.FindRepository(t.Context(), req)
			if status.Code(err) != codes.NotFound {
				t.Errorf("FindRepository(%v) error = %v, want NotFound", req, err)
			}
		}
	})
}

func TestDeleteRepository(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		ctx := t.Context()
		owner := env.newOwner(t)
		first := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments"})
		id := uuid.MustParse(first.GetId())

		if _, err := env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: first.GetId()}); err != nil {
			t.Fatalf("DeleteRepository() error = %v", err)
		}

		// Gone from every read, and a second delete finds nothing.
		_, err := env.client.GetRepository(ctx, &gitv1.GetRepositoryRequest{Id: first.GetId()})
		wantCode(t, "GetRepository", err, codes.NotFound)
		_, err = env.client.FindRepository(ctx, &gitv1.FindRepositoryRequest{OwnerId: owner, Name: "payments"})
		wantCode(t, "FindRepository", err, codes.NotFound)
		_, err = env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: first.GetId()})
		wantCode(t, "second DeleteRepository", err, codes.NotFound)
		list, err := env.client.ListRepositories(ctx, &gitv1.ListRepositoriesRequest{OwnerId: owner})
		if err != nil || len(list.GetRepositories()) != 0 {
			t.Errorf("ListRepositories() = %v, %v; want no repositories", list.GetRepositories(), err)
		}

		// The directory moved to the trash, complete.
		if _, err := os.Stat(env.layout.RepoPath(id)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("repository directory still in place (stat error: %v)", err)
		}
		git := newGitClient(t)
		if got := gitOut(t, git, env.layout.TrashPath(id), "rev-parse", "--is-bare-repository"); got != "true" {
			t.Errorf("trashed repository: is-bare-repository = %q, want true", got)
		}

		// The row stays, marked deleted.
		if n := env.count(t, `SELECT count(*) FROM git_svc.repositories WHERE id = $1 AND deleted_at IS NOT NULL`, id); n != 1 {
			t.Errorf("soft-deleted rows = %d, want 1", n)
		}

		// The name is free again, for a new repository.
		second := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments"})
		if second.GetId() == first.GetId() {
			t.Errorf("recreated repository reuses ID %s", first.GetId())
		}
		found, err := env.client.FindRepository(ctx, &gitv1.FindRepositoryRequest{OwnerId: owner, Name: "payments"})
		if err != nil || found.GetRepository().GetId() != second.GetId() {
			t.Errorf("FindRepository() = %v, %v; want the new repository %s", found.GetRepository(), err, second.GetId())
		}
		assertRepoDirs(t, env.layout, second.GetId())
	})
}

func TestOutboxEvents(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		ctx := t.Context()
		owner := env.newOwner(t)
		r := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments"})
		id := uuid.MustParse(r.GetId())

		// Requests that fail write no events.
		_, err := env.client.CreateRepository(ctx, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "PAYMENTS"})
		wantCode(t, "duplicate CreateRepository", err, codes.AlreadyExists)
		_, err = env.client.CreateRepository(ctx, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "bad/name"})
		wantCode(t, "invalid CreateRepository", err, codes.InvalidArgument)

		if _, err := env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: r.GetId()}); err != nil {
			t.Fatalf("DeleteRepository() error = %v", err)
		}
		_, err = env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: r.GetId()})
		wantCode(t, "second DeleteRepository", err, codes.NotFound)

		// occurred_at comes from the row the event describes.
		var createdAt, deletedAt time.Time
		err = env.pool.QueryRow(ctx, `SELECT created_at, deleted_at FROM git_svc.repositories WHERE id = $1`, id).
			Scan(&createdAt, &deletedAt)
		if err != nil {
			t.Fatal(err)
		}
		want := []proto.Message{
			&gitv1.RepositoryCreated{Id: r.GetId(), OwnerId: owner, Name: "payments", OccurredAt: timestamppb.New(createdAt)},
			&gitv1.RepositoryDeleted{Id: r.GetId(), OwnerId: owner, Name: "payments", OccurredAt: timestamppb.New(deletedAt)},
		}

		rows, err := env.pool.Query(ctx, `
SELECT event_id, aggregate_type, aggregate_id, event_type, payload::text, jsonb_typeof(payload), published_at IS NULL
FROM git_svc.outbox
WHERE payload->>'ownerId' = $1
ORDER BY id`, owner)
		if err != nil {
			t.Fatal(err)
		}
		type event struct {
			eventID, aggregateID                  uuid.UUID
			aggregateType, eventType, payload, js string
			pending                               bool
		}
		var events []event
		for rows.Next() {
			var e event
			if err := rows.Scan(&e.eventID, &e.aggregateType, &e.aggregateID, &e.eventType, &e.payload, &e.js, &e.pending); err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(events) != len(want) {
			t.Fatalf("%d outbox rows for the owner, want %d: %+v", len(events), len(want), events)
		}

		for i, e := range events {
			wantType := string(proto.MessageName(want[i]))
			if e.aggregateType != "repository" || e.aggregateID != id || e.eventType != wantType {
				t.Errorf("row %d: aggregate %s/%s, event_type %s; want repository/%s, %s",
					i, e.aggregateType, e.aggregateID, e.eventType, id, wantType)
			}
			if e.js != "object" {
				t.Errorf("row %d: payload is a JSON %s, want an object", i, e.js)
			}
			if !e.pending {
				t.Errorf("row %d: published_at is set, want NULL until the relay delivers it", i)
			}
			got := want[i].ProtoReflect().New().Interface()
			if err := protojson.Unmarshal([]byte(e.payload), got); err != nil {
				t.Errorf("row %d: decode payload %s: %v", i, e.payload, err)
			} else if !proto.Equal(got, want[i]) {
				t.Errorf("row %d: payload = %v, want %v", i, got, want[i])
			}
		}
		if events[0].eventID == events[1].eventID {
			t.Errorf("both events have event_id %s", events[0].eventID)
		}
	})
}

func TestConcurrentCreatesWithSameName(t *testing.T) {
	const n = 10
	forEachMode(t, func(t *testing.T, env *testEnv) {
		owner := env.newOwner(t)

		type result struct {
			repo *gitv1.Repository
			err  error
		}
		results := make(chan result, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				<-start
				resp, err := env.client.CreateRepository(t.Context(),
					&gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments"})
				results <- result{resp.GetRepository(), err}
			})
		}
		close(start)
		wg.Wait()
		close(results)

		var winners []*gitv1.Repository
		for r := range results {
			switch status.Code(r.err) {
			case codes.OK:
				winners = append(winners, r.repo)
			case codes.AlreadyExists:
			default:
				t.Errorf("CreateRepository() error = %v, want OK or AlreadyExists", r.err)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("%d of %d creates succeeded, want exactly 1", len(winners), n)
		}

		// One directory, one row, one event; the losers cleaned up.
		assertRepoDirs(t, env.layout, winners[0].GetId())
		assertEmptyDir(t, env.layout.TmpDir())
		if got := env.count(t, `SELECT count(*) FROM git_svc.repositories WHERE owner_id = $1`, uuid.MustParse(owner)); got != 1 {
			t.Errorf("%d rows for the owner, want 1", got)
		}
		if got := env.count(t, `SELECT count(*) FROM git_svc.outbox WHERE payload->>'ownerId' = $1`, owner); got != 1 {
			t.Errorf("%d outbox rows for the owner, want 1", got)
		}
	})
}

func TestListRepositories(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		ctx := t.Context()
		owner := env.newOwner(t)
		for _, name := range []string{"zeta", "beta", "Alpha", "gamma", "delta", "Epsilon"} {
			env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: name})
		}
		// Neither another owner's repositories nor deleted ones are listed.
		env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: env.newOwner(t), Name: "aaa"})
		deleted, err := env.client.FindRepository(ctx, &gitv1.FindRepositoryRequest{OwnerId: owner, Name: "delta"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: deleted.GetRepository().GetId()}); err != nil {
			t.Fatal(err)
		}
		want := []string{"Alpha", "beta", "Epsilon", "gamma", "zeta"}

		var (
			names     []string
			pages     int
			token     string
			firstNext string
		)
		for {
			resp, err := env.client.ListRepositories(ctx, &gitv1.ListRepositoriesRequest{OwnerId: owner, PageSize: 2, PageToken: token})
			if err != nil {
				t.Fatalf("ListRepositories(page %d) error = %v", pages+1, err)
			}
			pages++
			if len(resp.GetRepositories()) > 2 {
				t.Errorf("page %d has %d repositories, want at most 2", pages, len(resp.GetRepositories()))
			}
			for _, r := range resp.GetRepositories() {
				names = append(names, r.GetName())
			}
			token = resp.GetNextPageToken()
			if pages == 1 {
				firstNext = token
			}
			if token == "" || pages > len(want) {
				break
			}
		}
		if !slices.Equal(names, want) || pages != 3 {
			t.Errorf("listed %v in %d pages, want %v in 3", names, pages, want)
		}

		// Without a page size, the default page holds all five.
		all, err := env.client.ListRepositories(ctx, &gitv1.ListRepositoriesRequest{OwnerId: owner})
		if err != nil || len(all.GetRepositories()) != len(want) || all.GetNextPageToken() != "" {
			t.Errorf("ListRepositories(default size) = %d repositories, token %q, error %v; want %d, no token",
				len(all.GetRepositories()), all.GetNextPageToken(), err, len(want))
		}

		for _, tt := range []struct {
			name string
			req  *gitv1.ListRepositoriesRequest
		}{
			{"token of another owner", &gitv1.ListRepositoriesRequest{OwnerId: uuid.NewString(), PageToken: firstNext}},
			{"garbled token", &gitv1.ListRepositoriesRequest{OwnerId: owner, PageToken: "not-a-token"}},
			{"negative page size", &gitv1.ListRepositoriesRequest{OwnerId: owner, PageSize: -1}},
			{"malformed owner", &gitv1.ListRepositoriesRequest{OwnerId: "not-a-uuid"}},
		} {
			_, err := env.client.ListRepositories(ctx, tt.req)
			wantCode(t, "ListRepositories with "+tt.name, err, codes.InvalidArgument)
		}
	})
}

func TestInvalidRequestsChangeNothing(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		ctx := t.Context()
		owner := env.newOwner(t)
		for _, tt := range []struct {
			name string
			req  *gitv1.CreateRepositoryRequest
		}{
			{"malformed owner", &gitv1.CreateRepositoryRequest{OwnerId: "not-a-uuid", Name: "payments"}},
			{"empty name", &gitv1.CreateRepositoryRequest{OwnerId: owner}},
			{"slash in name", &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "a/b"}},
			{"dot-dot name", &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: ".."}},
			{".git suffix", &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments.GIT"}},
			{"invalid branch", &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments", DefaultBranch: proto.String("a..b")}},
			{"branch like an option", &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments", DefaultBranch: proto.String("--bare")}},
			{"long description", &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments", Description: proto.String(strings.Repeat("x", 2001))}},
		} {
			_, err := env.client.CreateRepository(ctx, tt.req)
			wantCode(t, "CreateRepository with "+tt.name, err, codes.InvalidArgument)
		}
		_, err := env.client.GetRepository(ctx, &gitv1.GetRepositoryRequest{Id: "not-a-uuid"})
		wantCode(t, "GetRepository with a malformed id", err, codes.InvalidArgument)
		_, err = env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: "../../etc"})
		wantCode(t, "DeleteRepository with a path as id", err, codes.InvalidArgument)

		assertRepoDirs(t, env.layout)
		assertEmptyDir(t, env.layout.TmpDir())
		assertEmptyDir(t, env.layout.TrashDir())
		if got := env.count(t, `SELECT count(*) FROM git_svc.repositories WHERE owner_id = $1`, uuid.MustParse(owner)); got != 0 {
			t.Errorf("%d rows for the owner, want 0", got)
		}
	})
}

// testEnv is one git-svc instance for one test.
type testEnv struct {
	client gitv1.RepositoryServiceClient
	pool   *pgxpool.Pool
	layout *storage.Layout
}

// forEachMode runs test as a subtest per pgx mode, each with its own
// testEnv.
func forEachMode(t *testing.T, test func(t *testing.T, env *testEnv)) {
	t.Helper()
	for _, mode := range testdb.Modes {
		t.Run(string(mode), func(t *testing.T) {
			test(t, newTestEnv(t, mode))
		})
	}
}

func newTestEnv(t *testing.T, mode testdb.Mode) *testEnv {
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
	// Server logs show up when a test fails or with -v.
	log := slog.New(slog.NewTextHandler(t.Output(), nil))
	srv := New(log, repo.NewService(pool, storage.NewStore(layout, git), log), Options{})

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &testEnv{client: gitv1.NewRepositoryServiceClient(conn), pool: pool, layout: layout}
}

// newOwner returns a fresh owner ID and deletes the owner's rows when the
// test ends.
func (e *testEnv) newOwner(t *testing.T) string {
	t.Helper()
	owner := uuid.New()
	t.Cleanup(func() {
		// t.Context is already canceled when cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := e.pool.Exec(ctx, `DELETE FROM git_svc.outbox WHERE payload->>'ownerId' = $1`, owner.String()); err != nil {
			t.Errorf("delete outbox rows of owner %s: %v", owner, err)
		}
		if _, err := e.pool.Exec(ctx, `DELETE FROM git_svc.repositories WHERE owner_id = $1`, owner); err != nil {
			t.Errorf("delete repositories of owner %s: %v", owner, err)
		}
	})
	return owner.String()
}

// create calls CreateRepository and fails the test on error.
func (e *testEnv) create(t *testing.T, req *gitv1.CreateRepositoryRequest) *gitv1.Repository {
	t.Helper()
	resp, err := e.client.CreateRepository(t.Context(), req)
	if err != nil {
		t.Fatalf("CreateRepository(%v) error = %v", req, err)
	}
	return resp.GetRepository()
}

// count runs a SELECT count(*) query.
func (e *testEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// newGitClient returns a git runner for the test's own git commands, with a
// HOME separate from git-svc's.
func newGitClient(t *testing.T) *gitcmd.Runner {
	t.Helper()
	git, err := gitcmd.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return git
}

// gitOut runs git in dir and returns its trimmed stdout. It fails the test
// if git fails.
func gitOut(t *testing.T, git *gitcmd.Runner, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// fileURL turns an absolute path into a file:// URL, also on Windows
// (file:///C:/...).
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

func wantCode(t *testing.T, call string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: error = %v, want %v", call, err, want)
	}
}

// assertRepoDirs checks that repositories/ holds exactly the repositories
// with the given IDs.
func assertRepoDirs(t *testing.T, layout *storage.Layout, ids ...string) {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(layout.ReposDir(), "*", "*.git"))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(ids))
	for _, id := range ids {
		want = append(want, layout.RepoPath(uuid.MustParse(id)))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("repository directories = %v, want %v", got, want)
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s is not empty: %v", dir, entries)
	}
}
