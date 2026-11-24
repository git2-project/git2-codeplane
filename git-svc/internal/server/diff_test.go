package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/browse"
	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// These tests call DiffService over gRPC, as dash-api does. The
// repository's row comes from a fake, so no database is needed; package
// browse tests the engine itself.

// branchRepo is the one live repository of these tests.
type branchRepo struct {
	id uuid.UUID
}

func (b *branchRepo) Get(_ context.Context, id uuid.UUID) (repo.Repository, error) {
	if id != b.id {
		return repo.Repository{}, repo.ErrNotFound
	}
	return repo.Repository{ID: id, DefaultBranch: "main"}, nil
}

// branchFixture is a served repository where, from a common base, main
// changes f.txt, feature adds n.txt and conflict changes f.txt differently.
type branchFixture struct {
	repo  *branchRepo
	diff  gitv1.DiffServiceClient
	base  string
	main  string
	feat  string
	confl string
}

func newBranchFixture(t *testing.T) *branchFixture {
	t.Helper()
	layout := storage.NewLayout(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	git, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &branchFixture{repo: &branchRepo{id: uuid.New()}}
	if err := storage.NewStore(layout, git).Init(t.Context(), f.repo.id, "main"); err != nil {
		t.Fatal(err)
	}

	client := newGitClient(t)
	work := filepath.Join(t.TempDir(), "w")
	gitOut(t, client, "", "init", "--quiet", "--initial-branch=main", work)
	commit := func(file, content string) string {
		if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		gitOut(t, client, work, "add", "--all")
		gitOut(t, client, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", file)
		return gitOut(t, client, work, "rev-parse", "HEAD")
	}
	f.base = commit("f.txt", "one\ntwo\n")
	gitOut(t, client, work, "switch", "--quiet", "-c", "feature")
	f.feat = commit("n.txt", "new\n")
	gitOut(t, client, work, "switch", "--quiet", "-c", "conflict", f.base)
	f.confl = commit("f.txt", "one\nFEAT\n")
	gitOut(t, client, work, "switch", "--quiet", "main")
	f.main = commit("f.txt", "one\nMAIN\n")
	gitOut(t, client, work, "push", "--quiet", fileURL(layout.RepoPath(f.repo.id)),
		"main:refs/heads/main", "feature:refs/heads/feature", "conflict:refs/heads/conflict")

	log := newTestLogger(t)
	srv := New(log, nil, browse.NewService(f.repo, layout, git), Options{})
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
	f.diff = gitv1.NewDiffServiceClient(conn)
	return f
}

func TestCompareRefsRPC(t *testing.T) {
	f := newBranchFixture(t)

	resp, err := f.diff.CompareRefs(t.Context(), &gitv1.CompareRefsRequest{RepositoryId: f.repo.id.String(), Base: "main", Head: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetBaseSha() != f.main || resp.GetHeadSha() != f.feat || resp.GetMergeBaseSha() != f.base ||
		resp.GetAheadBy() != 1 || resp.GetBehindBy() != 1 {
		t.Errorf("CompareRefs = %v", resp)
	}
	if len(resp.GetCommits()) != 1 || resp.GetCommits()[0].GetSha() != f.feat {
		t.Errorf("commits = %v, want feature's one commit", resp.GetCommits())
	}
	if len(resp.GetFiles()) != 1 || resp.GetFiles()[0].GetNewPath() != "n.txt" ||
		resp.GetFiles()[0].GetStatus() != gitv1.FileStatus_FILE_STATUS_ADDED || resp.GetStats().GetFilesChanged() != 1 {
		t.Errorf("files = %v, stats %v; want n.txt added", resp.GetFiles(), resp.GetStats())
	}

	_, err = f.diff.CompareRefs(t.Context(), &gitv1.CompareRefsRequest{RepositoryId: f.repo.id.String(), Base: "main", Head: "nope"})
	wantCode(t, "CompareRefs with an unknown head", err, codes.NotFound)
	_, err = f.diff.CompareRefs(t.Context(), &gitv1.CompareRefsRequest{RepositoryId: f.repo.id.String(), Head: "feature"})
	wantCode(t, "CompareRefs without base", err, codes.InvalidArgument)
}

func TestNoMergeBaseIsFailedPrecondition(t *testing.T) {
	err := toStatus(t.Context(), newTestLogger(t), browse.ErrNoMergeBase)
	wantCode(t, "toStatus(ErrNoMergeBase)", err, codes.FailedPrecondition)
}

// newTestLogger logs to the test's output, so server logs show up when a
// test fails or with -v.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(t.Output(), nil))
}
