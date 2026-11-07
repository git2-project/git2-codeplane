package server

import (
	"bufio"
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/browse"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// rawChunk is the most file content one GetRawBlobResponse carries.
const rawChunk = 64 << 10

// refServer implements gitv1.RefServiceServer on top of browse.Service.
type refServer struct {
	gitv1.UnimplementedRefServiceServer

	reader *browse.Service
	log    *slog.Logger
}

func (s *refServer) ListBranches(ctx context.Context, req *gitv1.ListBranchesRequest) (*gitv1.ListBranchesResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	page, err := s.reader.ListBranches(ctx, id, req.GetQuery(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.ListBranchesResponse{
		Branches:      make([]*gitv1.Branch, 0, len(page.Branches)),
		NextPageToken: page.NextPageToken,
	}
	for _, b := range page.Branches {
		resp.Branches = append(resp.Branches, &gitv1.Branch{Name: b.Name, Commit: commitToProto(b.Commit), IsDefault: b.Default})
	}
	return resp, nil
}

func (s *refServer) ListTags(ctx context.Context, req *gitv1.ListTagsRequest) (*gitv1.ListTagsResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	page, err := s.reader.ListTags(ctx, id, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.ListTagsResponse{
		Tags:          make([]*gitv1.Tag, 0, len(page.Tags)),
		NextPageToken: page.NextPageToken,
	}
	for _, t := range page.Tags {
		tag := &gitv1.Tag{Name: t.Name}
		if t.Commit != nil {
			tag.Commit = commitToProto(*t.Commit)
		}
		if t.Annotated {
			tag.Message = proto.String(t.Message)
		}
		resp.Tags = append(resp.Tags, tag)
	}
	return resp, nil
}

// commitServer implements gitv1.CommitServiceServer on top of
// browse.Service.
type commitServer struct {
	gitv1.UnimplementedCommitServiceServer

	reader *browse.Service
	log    *slog.Logger
}

func (s *commitServer) GetTree(ctx context.Context, req *gitv1.GetTreeRequest) (*gitv1.GetTreeResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	tree, err := s.reader.GetTree(ctx, id, req.GetRef(), req.GetPath())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.GetTreeResponse{
		Ref:       tree.Ref,
		CommitSha: tree.CommitSHA,
		Path:      tree.Path,
		Empty:     tree.Empty,
		Entries:   make([]*gitv1.TreeEntry, 0, len(tree.Entries)),
		Truncated: tree.Truncated,
	}
	for _, e := range tree.Entries {
		entry := &gitv1.TreeEntry{Name: e.Name, Path: e.Path, Type: entryTypeToProto(e.Type), Mode: e.Mode, Sha: e.SHA}
		if e.Size >= 0 {
			entry.Size = proto.Int64(e.Size)
		}
		resp.Entries = append(resp.Entries, entry)
	}
	if tree.LastCommit != nil {
		resp.LastCommit = commitToProto(*tree.LastCommit)
	}
	return resp, nil
}

func (s *commitServer) GetBlob(ctx context.Context, req *gitv1.GetBlobRequest) (*gitv1.GetBlobResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	b, err := s.reader.GetBlob(ctx, id, req.GetRef(), req.GetPath())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.GetBlobResponse{
		Ref:       b.Ref,
		CommitSha: b.CommitSHA,
		Path:      b.Path,
		Name:      b.Name,
		Sha:       b.SHA,
		Mode:      b.Mode,
		Type:      entryTypeToProto(b.Type),
		Size:      b.Size,
		Binary:    b.Binary,
		Lfs:       b.LFS,
		Truncated: b.Truncated,
		Content:   b.Content,
	}, nil
}

func (s *commitServer) GetRawBlob(req *gitv1.GetRawBlobRequest, stream grpc.ServerStreamingServer[gitv1.GetRawBlobResponse]) error {
	ctx := stream.Context()
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return toStatus(ctx, s.log, err)
	}
	f, err := s.reader.OpenRaw(ctx, id, req.GetRef(), req.GetPath())
	if err != nil {
		return toStatus(ctx, s.log, err)
	}
	if err := stream.Send(&gitv1.GetRawBlobResponse{Size: f.Size, Name: f.Name}); err != nil {
		return err
	}
	// Writes from git arrive in small pieces; the buffer turns them into
	// messages of rawChunk bytes.
	w := bufio.NewWriterSize(&rawSender{stream: stream}, rawChunk)
	if err := s.reader.WriteRaw(ctx, f, w); err != nil {
		return toStatus(ctx, s.log, err)
	}
	if err := w.Flush(); err != nil {
		return toStatus(ctx, s.log, err)
	}
	return nil
}

// rawSender sends each write as one GetRawBlobResponse.
type rawSender struct {
	stream grpc.ServerStreamingServer[gitv1.GetRawBlobResponse]
}

func (r *rawSender) Write(p []byte) (int, error) {
	// Send keeps no reference to the message after it returns, but bufio
	// reuses p, so the message gets its own copy anyway.
	if err := r.stream.Send(&gitv1.GetRawBlobResponse{Data: append([]byte(nil), p...)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *commitServer) ListCommits(ctx context.Context, req *gitv1.ListCommitsRequest) (*gitv1.ListCommitsResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	page, err := s.reader.ListCommits(ctx, id, req.GetRef(), req.GetPath(), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.ListCommitsResponse{
		Commits:       make([]*gitv1.Commit, 0, len(page.Commits)),
		NextPageToken: page.NextPageToken,
	}
	for _, c := range page.Commits {
		resp.Commits = append(resp.Commits, commitToProto(c))
	}
	return resp, nil
}

func (s *commitServer) GetCommit(ctx context.Context, req *gitv1.GetCommitRequest) (*gitv1.GetCommitResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	d, err := s.reader.GetCommit(ctx, id, req.GetSha())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.GetCommitResponse{
		Commit: commitToProto(d.Commit),
		Stats: &gitv1.DiffStats{
			FilesChanged: clamp32(d.Stats.FilesChanged),
			Additions:    clamp32(d.Stats.Additions),
			Deletions:    clamp32(d.Stats.Deletions),
		},
		Files:     make([]*gitv1.FileDiff, 0, len(d.Files)),
		Truncated: d.Truncated,
	}
	for _, f := range d.Files {
		resp.Files = append(resp.Files, &gitv1.FileDiff{
			Status:    fileStatusToProto(f.Status),
			OldPath:   f.OldPath,
			NewPath:   f.NewPath,
			Additions: clamp32(f.Additions),
			Deletions: clamp32(f.Deletions),
			Binary:    f.Binary,
			Truncated: f.Truncated,
			Patch:     f.Patch,
		})
	}
	return resp, nil
}

func commitToProto(c browse.Commit) *gitv1.Commit {
	return &gitv1.Commit{
		Sha:        c.SHA,
		ParentShas: c.ParentSHAs,
		Author:     signatureToProto(c.Author),
		Committer:  signatureToProto(c.Committer),
		Title:      c.Title,
		Message:    c.Message,
	}
}

func signatureToProto(s browse.Signature) *gitv1.Signature {
	return &gitv1.Signature{Name: s.Name, Email: s.Email, Time: timestamppb.New(s.Time)}
}

func entryTypeToProto(t browse.EntryType) gitv1.TreeEntryType {
	switch t {
	case browse.EntryFile:
		return gitv1.TreeEntryType_TREE_ENTRY_TYPE_FILE
	case browse.EntryDirectory:
		return gitv1.TreeEntryType_TREE_ENTRY_TYPE_DIRECTORY
	case browse.EntrySymlink:
		return gitv1.TreeEntryType_TREE_ENTRY_TYPE_SYMLINK
	case browse.EntrySubmodule:
		return gitv1.TreeEntryType_TREE_ENTRY_TYPE_SUBMODULE
	}
	return gitv1.TreeEntryType_TREE_ENTRY_TYPE_UNSPECIFIED
}

func fileStatusToProto(s browse.FileStatus) gitv1.FileStatus {
	switch s {
	case browse.FileAdded:
		return gitv1.FileStatus_FILE_STATUS_ADDED
	case browse.FileModified:
		return gitv1.FileStatus_FILE_STATUS_MODIFIED
	case browse.FileDeleted:
		return gitv1.FileStatus_FILE_STATUS_DELETED
	case browse.FileRenamed:
		return gitv1.FileStatus_FILE_STATUS_RENAMED
	case browse.FileCopied:
		return gitv1.FileStatus_FILE_STATUS_COPIED
	}
	return gitv1.FileStatus_FILE_STATUS_UNSPECIFIED
}

// clamp32 fits a count into the contract's int32; a diff that large is
// not realistic, but the conversion must not wrap.
func clamp32(n int) int32 {
	return int32(min(n, 1<<31-1)) //nolint:gosec // G115: clamped above
}
