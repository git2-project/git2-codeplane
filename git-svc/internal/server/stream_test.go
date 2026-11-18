package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/browse"
	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// TestStreamCanceledByClient stops reading GetRawBlob and GetArchive in the
// middle, as ci-svc does at its size limit. The server must end the call
// as CANCELED, not as INTERNAL with an error in its log. The repository's
// row comes from a fake, so no database is needed.
func TestStreamCanceledByClient(t *testing.T) {
	layout := storage.NewLayout(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	git, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err := storage.NewStore(layout, git).Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}

	// Far more than gRPC's flow control lets the server send before the
	// client reads (its window grows to at most 16 MiB), so the server is
	// still sending when the client cancels.
	big := bytes.Repeat([]byte("0123456789abcdef"), 20<<20/16)
	client := newGitClient(t)
	work := filepath.Join(t.TempDir(), "w")
	gitOut(t, client, "", "clone", "--quiet", fileURL(layout.RepoPath(id)), work)
	if err := os.WriteFile(filepath.Join(work, "big.txt"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, client, work, "add", "--all")
	gitOut(t, client, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "big")
	gitOut(t, client, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")

	// Server logs show up when the test fails or with -v; the test reads
	// them from logs.
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(t.Output(), logs), nil))
	srv := New(log, nil, browse.NewService(oneRepo{id: id}, layout, git), Options{})
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
	commits := gitv1.NewCommitServiceClient(conn)

	calls := []struct {
		method string
		open   func(ctx context.Context) (recv func() error, err error)
	}{
		{"/git.v1.CommitService/GetRawBlob", func(ctx context.Context) (func() error, error) {
			stream, err := commits.GetRawBlob(ctx, &gitv1.GetRawBlobRequest{RepositoryId: id.String(), Ref: "main", Path: "big.txt"})
			return func() error { _, err := stream.Recv(); return err }, err
		}},
		{"/git.v1.CommitService/GetArchive", func(ctx context.Context) (func() error, error) {
			stream, err := commits.GetArchive(ctx, &gitv1.GetArchiveRequest{RepositoryId: id.String(), Ref: "main"})
			return func() error { _, err := stream.Recv(); return err }, err
		}},
	}
	for _, c := range calls {
		ctx, cancel := context.WithCancel(t.Context())
		recv, err := c.open(ctx)
		if err != nil {
			t.Fatalf("%s error = %v", c.method, err)
		}
		// The first message carries no data, the second the first piece.
		for range 2 {
			if err := recv(); err != nil {
				t.Fatalf("%s: Recv() error = %v", c.method, err)
			}
		}
		cancel()
		line := waitRPC(t, logs, c.method)
		if !strings.Contains(line, " level=INFO ") || !strings.Contains(line, " code=Canceled ") {
			t.Errorf("%s ended with %q, want code=Canceled at level INFO", c.method, line)
		}
	}
	if strings.Contains(logs.String(), "request failed") {
		t.Errorf("a canceled stream was logged as a failure:\n%s", logs.String())
	}
}

// oneRepo is a browse.Repositories with one live repository, whose default
// branch is main.
type oneRepo struct {
	id uuid.UUID
}

func (o oneRepo) Get(_ context.Context, id uuid.UUID) (repo.Repository, error) {
	if id != o.id {
		return repo.Repository{}, repo.ErrNotFound
	}
	return repo.Repository{ID: id, DefaultBranch: "main"}, nil
}

// waitRPC waits until the server has logged the end of a call of method,
// for example "/git.v1.CommitService/GetArchive", and returns the first
// such log line.
func waitRPC(t *testing.T, logs *syncBuffer, method string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for line := range strings.Lines(logs.String()) {
			if strings.Contains(line, " msg=rpc method="+method+" ") {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server logged no end of %s", method)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a bytes.Buffer that the server's goroutines can write to
// while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
