package server

import (
	"context"
	"log/slog"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/merge"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// operationServer implements gitv1.OperationServiceServer on top of
// merge.Service.
type operationServer struct {
	gitv1.UnimplementedOperationServiceServer

	merger *merge.Service
	log    *slog.Logger
}

func (s *operationServer) CheckMerge(ctx context.Context, req *gitv1.CheckMergeRequest) (*gitv1.CheckMergeResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	c, err := s.merger.CheckMerge(ctx, id, req.GetSourceBranch(), req.GetTargetBranch())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.CheckMergeResponse{
		SourceSha:          c.SourceSHA,
		TargetSha:          c.TargetSHA,
		MergeBaseSha:       c.MergeBaseSHA,
		Status:             mergeStatusToProto(c.Status),
		CanFastForward:     c.CanFastForward,
		Conflicts:          conflictsToProto(c.Conflicts),
		ConflictsTruncated: c.ConflictsTruncated,
	}, nil
}

func (s *operationServer) MergeBranches(ctx context.Context, req *gitv1.MergeBranchesRequest) (*gitv1.MergeBranchesResponse, error) {
	id, err := repo.ParseID("repository_id", req.GetRepositoryId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	merger, err := repo.ParseID("merger_id", req.GetMergerId())
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	res, err := s.merger.MergeBranches(ctx, merge.Request{
		RepositoryID:      id,
		SourceBranch:      req.GetSourceBranch(),
		TargetBranch:      req.GetTargetBranch(),
		Strategy:          strategyFromProto(req.GetStrategy()),
		ExpectedSourceSHA: req.GetExpectedSourceSha(),
		Message:           req.GetMessage(),
		Author:            identityFromProto(req.GetAuthor()),
		Committer:         identityFromProto(req.GetCommitter()),
		MergerID:          merger,
	})
	if err != nil {
		return nil, toStatus(ctx, s.log, err)
	}
	return &gitv1.MergeBranchesResponse{OldTargetSha: res.OldTargetSHA, NewTargetSha: res.NewTargetSHA}, nil
}

func mergeStatusToProto(s merge.Status) gitv1.MergeStatus {
	switch s {
	case merge.StatusClean:
		return gitv1.MergeStatus_MERGE_STATUS_CLEAN
	case merge.StatusConflicts:
		return gitv1.MergeStatus_MERGE_STATUS_CONFLICTS
	case merge.StatusAlreadyMerged:
		return gitv1.MergeStatus_MERGE_STATUS_ALREADY_MERGED
	case merge.StatusUnrelated:
		return gitv1.MergeStatus_MERGE_STATUS_UNRELATED
	default:
		return gitv1.MergeStatus_MERGE_STATUS_UNSPECIFIED
	}
}

// strategyFromProto maps the strategy; an unknown or unset one becomes 0,
// which merge.Request rejects.
func strategyFromProto(s gitv1.MergeStrategy) merge.Strategy {
	switch s {
	case gitv1.MergeStrategy_MERGE_STRATEGY_MERGE_COMMIT:
		return merge.StrategyMergeCommit
	case gitv1.MergeStrategy_MERGE_STRATEGY_SQUASH:
		return merge.StrategySquash
	case gitv1.MergeStrategy_MERGE_STRATEGY_FAST_FORWARD:
		return merge.StrategyFastForward
	default:
		return 0
	}
}

// identityFromProto returns nil for an unset identity.
func identityFromProto(id *gitv1.CommitIdentity) *merge.Identity {
	if id == nil {
		return nil
	}
	return &merge.Identity{Name: id.GetName(), Email: id.GetEmail()}
}
