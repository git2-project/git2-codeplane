package server

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

// TestBundleRoundTripOverGRPC backs a pushed repository up and restores it
// as on a new server: the row stays, the files go, RestoreBundle brings them
// back. Package storage tests the bundles themselves in detail.
func TestBundleRoundTripOverGRPC(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		owner := env.newOwner(t)
		r := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "payments", DefaultBranch: proto.String("trunk")})
		id := uuid.MustParse(r.GetId())
		bare := env.layout.RepoPath(id)

		git := newGitClient(t)
		work := filepath.Join(t.TempDir(), "w")
		gitOut(t, git, "", "clone", "--quiet", fileURL(bare), work)
		// Random bytes do not compress, so the bundle needs several messages.
		large := make([]byte, 300<<10)
		if _, err := rand.Read(large); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "data.bin"), large, 0o600); err != nil {
			t.Fatal(err)
		}
		gitOut(t, git, work, "add", "data.bin")
		gitOut(t, git, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "first")
		gitOut(t, git, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "tag", "-a", "-m", "release", "v1")
		gitOut(t, git, work, "push", "--quiet", "origin", "HEAD:refs/heads/trunk", "HEAD:refs/heads/feature", "refs/tags/v1")
		wantRefs := gitOut(t, git, bare, "for-each-ref", "--format=%(objectname) %(refname)")

		msgs, err := env.createBundle(t, r.GetId())
		if err != nil {
			t.Fatalf("CreateBundle() error = %v", err)
		}
		var bundle []byte
		for _, m := range msgs {
			if m.GetEmpty() || len(m.GetData()) == 0 || len(m.GetData()) > bundleChunk {
				t.Errorf("message with empty = %v and %d bytes, want data of 1 to %d bytes", m.GetEmpty(), len(m.GetData()), bundleChunk)
			}
			bundle = append(bundle, m.GetData()...)
		}
		if len(msgs) < 2 {
			t.Errorf("%d-byte bundle in %d messages, want several", len(bundle), len(msgs))
		}

		// A new server: the database came back, the files did not.
		if err := os.RemoveAll(bare); err != nil {
			t.Fatal(err)
		}
		events := env.count(t, `SELECT count(*) FROM git_svc.outbox WHERE aggregate_id = $1`, id)

		// Pieces of another size than CreateBundle's.
		if err := env.restoreBundle(t, r.GetId(), bundle, 100<<10); err != nil {
			t.Fatalf("RestoreBundle() error = %v", err)
		}
		if got := gitOut(t, git, bare, "for-each-ref", "--format=%(objectname) %(refname)"); got != wantRefs {
			t.Errorf("restored refs:\n%s\nwant:\n%s", got, wantRefs)
		}
		if got := gitOut(t, git, bare, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
			t.Errorf("HEAD = %q, want refs/heads/trunk", got)
		}
		gitOut(t, git, bare, "fsck", "--strict")

		// A restore is not a push: no event, so no CI pipeline.
		if got := env.count(t, `SELECT count(*) FROM git_svc.outbox WHERE aggregate_id = $1`, id); got != events {
			t.Errorf("%d outbox rows for the repository after the restore, want %d", got, events)
		}
		// git-svc serves the restored repository.
		blob, err := env.commits.GetBlob(t.Context(), &gitv1.GetBlobRequest{RepositoryId: r.GetId(), Ref: "v1", Path: "data.bin"})
		if err != nil || blob.GetSize() != int64(len(large)) {
			t.Errorf("GetBlob() after restore = size %d, error %v; want %d bytes", blob.GetSize(), err, len(large))
		}

		// Restoring again changes nothing.
		err = env.restoreBundle(t, r.GetId(), bundle, bundleChunk)
		wantCode(t, "second RestoreBundle", err, codes.AlreadyExists)
		assertEmptyDir(t, env.layout.TmpDir())
	})
}

func TestBundleOfEmptyRepositoryOverGRPC(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		r := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: env.newOwner(t), Name: "payments"})

		msgs, err := env.createBundle(t, r.GetId())
		if err != nil {
			t.Fatalf("CreateBundle() error = %v", err)
		}
		if len(msgs) != 1 || !msgs[0].GetEmpty() || len(msgs[0].GetData()) != 0 {
			t.Fatalf("CreateBundle(empty repository) = %v, want one message with empty = true and no data", msgs)
		}

		bare := env.layout.RepoPath(uuid.MustParse(r.GetId()))
		if err := os.RemoveAll(bare); err != nil {
			t.Fatal(err)
		}
		if err := env.restoreBundle(t, r.GetId(), nil, bundleChunk); err != nil {
			t.Fatalf("RestoreBundle(no data) error = %v", err)
		}
		tree, err := env.commits.GetTree(t.Context(), &gitv1.GetTreeRequest{RepositoryId: r.GetId()})
		if err != nil || !tree.GetEmpty() || tree.GetRef() != "main" {
			t.Errorf("GetTree() after restore = %v, %v; want an empty repository on main", tree, err)
		}
		if got := gitOut(t, newGitClient(t), bare, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
			t.Errorf("HEAD = %q, want refs/heads/main", got)
		}
	})
}

func TestBundleErrors(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		owner := env.newOwner(t)
		live := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "live"})
		deleted := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: "deleted"})
		if _, err := env.client.DeleteRepository(t.Context(), &gitv1.DeleteRepositoryRequest{Id: deleted.GetId()}); err != nil {
			t.Fatal(err)
		}

		for _, tt := range []struct {
			name string
			id   string
			want codes.Code
		}{
			{"malformed id", "../x", codes.InvalidArgument},
			{"unknown repository", uuid.NewString(), codes.NotFound},
			{"deleted repository", deleted.GetId(), codes.NotFound},
		} {
			_, err := env.createBundle(t, tt.id)
			wantCode(t, "CreateBundle with "+tt.name, err, tt.want)
			err = env.restoreBundle(t, tt.id, []byte("# v2 git bundle\n"), bundleChunk)
			wantCode(t, "RestoreBundle with "+tt.name, err, tt.want)
		}

		// The files of a live repository are in place.
		err := env.restoreBundle(t, live.GetId(), nil, bundleChunk)
		wantCode(t, "RestoreBundle over existing files", err, codes.AlreadyExists)

		// The next cases need a repository whose files are gone.
		liveID := uuid.MustParse(live.GetId())
		if err := os.RemoveAll(env.layout.RepoPath(liveID)); err != nil {
			t.Fatal(err)
		}

		stream, err := env.bundles.RestoreBundle(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = stream.CloseAndRecv()
		wantCode(t, "RestoreBundle without messages", err, codes.InvalidArgument)

		// Without data this would restore an empty repository, so only the
		// repeated ID can make it fail.
		stream, err = env.bundles.RestoreBundle(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range []*gitv1.RestoreBundleRequest{
			{RepositoryId: live.GetId()},
			{RepositoryId: live.GetId()},
		} {
			if err := stream.Send(msg); err != nil {
				break // the server answered early; CloseAndRecv has its status
			}
		}
		_, err = stream.CloseAndRecv()
		wantCode(t, "RestoreBundle with repository_id in a later message", err, codes.InvalidArgument)

		err = env.restoreBundle(t, live.GetId(), []byte("# v2 git bundle\n0000000000000000000000000000000000000000 refs/heads/main\n\nPACK"), bundleChunk)
		wantCode(t, "RestoreBundle with a cut-off bundle", err, codes.InvalidArgument)

		// Nothing was left behind, and the repository can still be restored.
		if _, err := os.Stat(env.layout.RepoPath(liveID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("repository directory exists after failed restores (stat error: %v)", err)
		}
		assertEmptyDir(t, env.layout.TmpDir())
		if err := env.restoreBundle(t, live.GetId(), nil, bundleChunk); err != nil {
			t.Errorf("RestoreBundle(no data) after failed restores: %v", err)
		}
	})
}

func TestListAllRepositories(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		ctx := t.Context()
		var want []string
		for _, owner := range []string{env.newOwner(t), env.newOwner(t)} {
			for _, name := range []string{"alpha", "beta", "gamma"} {
				want = append(want, env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: owner, Name: name}).GetId())
			}
		}
		deleted := want[1]
		if _, err := env.client.DeleteRepository(ctx, &gitv1.DeleteRepositoryRequest{Id: deleted}); err != nil {
			t.Fatal(err)
		}
		want = slices.Delete(want, 1, 2)
		slices.Sort(want) // canonical UUID text sorts as the UUIDs do

		// Other tests share the database, so walk every page and look only
		// at this test's repositories. The first pages are small, then the
		// same token continues with larger ones.
		var (
			all, mine []string
			token     string
			firstNext string
		)
		for pages := 0; ; pages++ {
			size := int32(2)
			if pages >= 3 {
				size = 1000
			}
			resp, err := env.bundles.ListAllRepositories(ctx, &gitv1.ListAllRepositoriesRequest{PageSize: size, PageToken: token})
			if err != nil {
				t.Fatalf("ListAllRepositories(page %d) error = %v", pages+1, err)
			}
			if len(resp.GetRepositories()) > int(size) {
				t.Errorf("page %d has %d repositories, want at most %d", pages+1, len(resp.GetRepositories()), size)
			}
			for _, r := range resp.GetRepositories() {
				all = append(all, r.GetId())
				if slices.Contains(want, r.GetId()) || r.GetId() == deleted {
					mine = append(mine, r.GetId())
				}
			}
			if pages == 0 {
				firstNext = resp.GetNextPageToken()
			}
			if token = resp.GetNextPageToken(); token == "" {
				break
			}
		}
		if !slices.Equal(mine, want) {
			t.Errorf("this test's repositories in the listing = %v, want %v (sorted, without the deleted one)", mine, want)
		}
		if !slices.IsSorted(all) || len(slices.Compact(slices.Clone(all))) != len(all) {
			t.Errorf("listing is not sorted by ID without repeats: %v", all)
		}

		for _, tt := range []struct {
			name string
			req  *gitv1.ListAllRepositoriesRequest
		}{
			{"negative page size", &gitv1.ListAllRepositoriesRequest{PageSize: -1}},
			{"garbled token", &gitv1.ListAllRepositoriesRequest{PageToken: "not-a-token"}},
		} {
			_, err := env.bundles.ListAllRepositories(ctx, tt.req)
			wantCode(t, "ListAllRepositories with "+tt.name, err, codes.InvalidArgument)
		}
		if firstNext != "" {
			_, err := env.client.ListRepositories(ctx, &gitv1.ListRepositoriesRequest{OwnerId: uuid.NewString(), PageToken: firstNext})
			wantCode(t, "ListRepositories with a ListAllRepositories token", err, codes.InvalidArgument)
		}
	})
}

// createBundle calls CreateBundle and returns every message of the stream.
func (e *testEnv) createBundle(t *testing.T, id string) ([]*gitv1.CreateBundleResponse, error) {
	t.Helper()
	stream, err := e.bundles.CreateBundle(t.Context(), &gitv1.CreateBundleRequest{RepositoryId: id})
	if err != nil {
		return nil, err
	}
	var msgs []*gitv1.CreateBundleResponse
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return msgs, nil
		}
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, msg)
	}
}

// restoreBundle calls RestoreBundle with data in pieces of chunk bytes; the
// first message also carries the ID.
func (e *testEnv) restoreBundle(t *testing.T, id string, data []byte, chunk int) error {
	t.Helper()
	stream, err := e.bundles.RestoreBundle(t.Context())
	if err != nil {
		return err
	}
	msg := &gitv1.RestoreBundleRequest{RepositoryId: id}
	for {
		n := min(chunk, len(data))
		msg.Data, data = data[:n], data[n:]
		if err := stream.Send(msg); err != nil {
			break // the server answered early; CloseAndRecv has its status
		}
		if len(data) == 0 {
			break
		}
		msg = &gitv1.RestoreBundleRequest{}
	}
	_, err = stream.CloseAndRecv()
	return err
}
