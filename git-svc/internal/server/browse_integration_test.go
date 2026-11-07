package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
)

// TestBrowseOverGRPC reads a pushed repository through RefService and
// CommitService, end to end: the repository is created through the API
// and lives in PostgreSQL. Package browse tests the reading in detail.
func TestBrowseOverGRPC(t *testing.T) {
	forEachMode(t, func(t *testing.T, env *testEnv) {
		r := env.create(t, &gitv1.CreateRepositoryRequest{OwnerId: env.newOwner(t), Name: "payments"})
		id := r.GetId()

		// An empty repository is not an error for the code browser.
		tree, err := env.commits.GetTree(t.Context(), &gitv1.GetTreeRequest{RepositoryId: id})
		if err != nil || !tree.GetEmpty() || tree.GetRef() != "main" {
			t.Fatalf("GetTree(empty) = %v, %v; want empty on main", tree, err)
		}

		// Push one commit with a small and a large file.
		git := newGitClient(t)
		work := filepath.Join(t.TempDir(), "w")
		gitOut(t, git, "", "clone", "--quiet", fileURL(env.layout.RepoPath(uuid.MustParse(id))), work)
		large := bytes.Repeat([]byte("0123456789abcdef"), 300<<10/16) // 300 KiB: several stream messages
		for name, content := range map[string][]byte{"README.md": []byte("hello\n"), "docs/large.txt": large} {
			full := filepath.Join(work, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitOut(t, git, work, "add", "--all")
		gitOut(t, git, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "first\n\nbody")
		sha := gitOut(t, git, work, "rev-parse", "HEAD")
		gitOut(t, git, work, "push", "--quiet", "origin", "HEAD:refs/heads/main")

		branches, err := env.refs.ListBranches(t.Context(), &gitv1.ListBranchesRequest{RepositoryId: id})
		if err != nil || len(branches.GetBranches()) != 1 {
			t.Fatalf("ListBranches() = %v, %v", branches, err)
		}
		if b := branches.GetBranches()[0]; b.GetName() != "main" || !b.GetIsDefault() || b.GetCommit().GetSha() != sha ||
			b.GetCommit().GetTitle() != "first" || b.GetCommit().GetAuthor().GetEmail() != "test@example.com" ||
			b.GetCommit().GetAuthor().GetTime().AsTime().IsZero() {
			t.Errorf("branch = %v", b)
		}

		tree, err = env.commits.GetTree(t.Context(), &gitv1.GetTreeRequest{RepositoryId: id})
		if err != nil || tree.GetCommitSha() != sha || len(tree.GetEntries()) != 2 || tree.GetLastCommit().GetSha() != sha {
			t.Fatalf("GetTree() = %v, %v", tree, err)
		}
		docs, readme := tree.GetEntries()[0], tree.GetEntries()[1]
		if docs.GetType() != gitv1.TreeEntryType_TREE_ENTRY_TYPE_DIRECTORY || docs.Size != nil ||
			readme.GetType() != gitv1.TreeEntryType_TREE_ENTRY_TYPE_FILE || readme.GetSize() != 6 || readme.GetMode() != "100644" {
			t.Errorf("entries = %v", tree.GetEntries())
		}

		blob, err := env.commits.GetBlob(t.Context(), &gitv1.GetBlobRequest{RepositoryId: id, Path: "README.md"})
		if err != nil || string(blob.GetContent()) != "hello\n" || blob.GetName() != "README.md" || blob.GetCommitSha() != sha {
			t.Errorf("GetBlob() = %v, %v", blob, err)
		}

		stream, err := env.commits.GetRawBlob(t.Context(), &gitv1.GetRawBlobRequest{RepositoryId: id, Ref: sha, Path: "docs/large.txt"})
		if err != nil {
			t.Fatal(err)
		}
		var raw []byte
		var messages int
		for {
			msg, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if messages == 0 && (msg.GetSize() != int64(len(large)) || msg.GetName() != "large.txt") {
				t.Errorf("first message = size %d, name %q", msg.GetSize(), msg.GetName())
			}
			if len(msg.GetData()) > rawChunk {
				t.Errorf("message of %d bytes, want at most %d", len(msg.GetData()), rawChunk)
			}
			raw = append(raw, msg.GetData()...)
			messages++
		}
		if !bytes.Equal(raw, large) || messages < 2 {
			t.Errorf("GetRawBlob() = %d bytes in %d messages, want the %d-byte file", len(raw), messages, len(large))
		}

		commits, err := env.commits.ListCommits(t.Context(), &gitv1.ListCommitsRequest{RepositoryId: id, Path: "docs"})
		if err != nil || len(commits.GetCommits()) != 1 || commits.GetCommits()[0].GetMessage() != "first\n\nbody" {
			t.Errorf("ListCommits(docs) = %v, %v", commits, err)
		}

		commit, err := env.commits.GetCommit(t.Context(), &gitv1.GetCommitRequest{RepositoryId: id, Sha: sha[:10]})
		if err != nil {
			t.Fatal(err)
		}
		// large.txt is one long line without a newline: one added line.
		want := &gitv1.DiffStats{FilesChanged: 2, Additions: 2}
		if commit.GetCommit().GetSha() != sha || !proto.Equal(commit.GetStats(), want) || len(commit.GetFiles()) != 2 {
			t.Fatalf("GetCommit() = commit %s, stats %v, %d files", commit.GetCommit().GetSha(), commit.GetStats(), len(commit.GetFiles()))
		}
		// Files come in git's byte order: "README.md" before "docs/".
		if f := commit.GetFiles()[0]; f.GetNewPath() != "README.md" || f.GetStatus() != gitv1.FileStatus_FILE_STATUS_ADDED ||
			string(f.GetPatch()) != "@@ -0,0 +1 @@\n+hello\n" {
			t.Errorf("README.md diff = %v", f)
		}
		if f := commit.GetFiles()[1]; f.GetNewPath() != "docs/large.txt" || !f.GetTruncated() || f.GetPatch() != nil {
			t.Errorf("docs/large.txt diff = %v, want truncated (one line over 64 KiB)", f)
		}

		// Errors keep their codes and never leak internal details.
		_, err = env.commits.GetTree(t.Context(), &gitv1.GetTreeRequest{RepositoryId: id, Path: "nope"})
		wantCode(t, "GetTree(missing path)", err, codes.NotFound)
		_, err = env.commits.GetTree(t.Context(), &gitv1.GetTreeRequest{RepositoryId: id, Path: "../x"})
		wantCode(t, "GetTree(../x)", err, codes.InvalidArgument)
		_, err = env.commits.GetBlob(t.Context(), &gitv1.GetBlobRequest{RepositoryId: id, Ref: "nope", Path: "README.md"})
		wantCode(t, "GetBlob(unknown ref)", err, codes.NotFound)
		_, err = env.commits.GetCommit(t.Context(), &gitv1.GetCommitRequest{RepositoryId: id, Sha: strings.Repeat("0", 40)})
		wantCode(t, "GetCommit(unknown)", err, codes.NotFound)
		_, err = env.refs.ListTags(t.Context(), &gitv1.ListTagsRequest{RepositoryId: "nope"})
		wantCode(t, "ListTags(bad id)", err, codes.InvalidArgument)

		// A deleted repository is not served, even though its files remain
		// in the trash.
		if _, err := env.client.DeleteRepository(t.Context(), &gitv1.DeleteRepositoryRequest{Id: id}); err != nil {
			t.Fatal(err)
		}
		_, err = env.commits.GetBlob(t.Context(), &gitv1.GetBlobRequest{RepositoryId: id, Path: "README.md"})
		wantCode(t, "GetBlob(deleted repository)", err, codes.NotFound)
		raws, err := env.commits.GetRawBlob(t.Context(), &gitv1.GetRawBlobRequest{RepositoryId: id, Path: "README.md"})
		if err == nil {
			_, err = raws.Recv()
		}
		wantCode(t, "GetRawBlob(deleted repository)", err, codes.NotFound)
	})
}
