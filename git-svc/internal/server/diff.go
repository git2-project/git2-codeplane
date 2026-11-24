package server

import (
	"context"
	"log/slog"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/browse"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// diffServer implements gitv1.DiffServiceServer on top of browse.Service.
type diffServer struct {
	gitv1.UnimplementedDiffServiceServer

	reader *browse.Service
	log    *slog.Logger
}

func (s *diffServer) CompareRefs(ctx context.Context, req *gitv1.CompareRefsRequest) (*gitv1.CompareRefsResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	c, err := s.reader.CompareRefs(ctx, id, req.GetBase(), req.GetHead())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	resp := &gitv1.CompareRefsResponse{
		BaseSha:          c.BaseSHA,
		HeadSha:          c.HeadSHA,
		MergeBaseSha:     c.MergeBaseSHA,
		AheadBy:          clamp32(c.AheadBy),
		BehindBy:         clamp32(c.BehindBy),
		Commits:          make([]*gitv1.Commit, 0, len(c.Commits)),
		CommitsTruncated: c.CommitsTruncated,
		Stats:            statsToProto(c.Stats),
		Files:            filesToProto(c.Files),
		FilesTruncated:   c.FilesTruncated,
	}
	for _, commit := range c.Commits {
		resp.Commits = append(resp.Commits, commitToProto(commit))
	}
	return resp, nil
}

// statsToProto converts the totals of a diff; GetCommit and CompareRefs
// share it.
func statsToProto(s browse.DiffStats) *gitv1.DiffStats {
	return &gitv1.DiffStats{
		FilesChanged: clamp32(s.FilesChanged),
		Additions:    clamp32(s.Additions),
		Deletions:    clamp32(s.Deletions),
	}
}

// filesToProto converts the changed files of a diff; GetCommit and
// CompareRefs share it.
func filesToProto(files []browse.FileDiff) []*gitv1.FileDiff {
	out := make([]*gitv1.FileDiff, 0, len(files))
	for _, f := range files {
		out = append(out, &gitv1.FileDiff{
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
	return out
}
