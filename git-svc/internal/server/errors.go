package server

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/browse"
	"github.com/git2-project/git2-codeplane/git-svc/internal/merge"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// toStatus turns an error from package repo, browse or merge into a gRPC
// status error.
//
// Client-facing messages are fixed strings or a *repo.ValidationError,
// which never repeats user input. Any other error can contain paths, SQL
// or git output, so it is logged here and the client only sees a generic
// INTERNAL error.
func toStatus(ctx context.Context, log *slog.Logger, err error) error {
	var invalid *repo.ValidationError
	var refusal *merge.Refusal
	switch {
	case errors.As(err, &invalid):
		return status.Error(codes.InvalidArgument, invalid.Error())
	case errors.As(err, &refusal):
		return refusalStatus(ctx, log, refusal)
	case errors.Is(err, repo.ErrNotFound):
		return status.Error(codes.NotFound, "repository not found")
	case errors.Is(err, browse.ErrRefNotFound):
		return status.Error(codes.NotFound, "ref not found")
	case errors.Is(err, merge.ErrBranchNotFound):
		return status.Error(codes.NotFound, "branch not found")
	case errors.Is(err, browse.ErrNoMergeBase):
		return status.Error(codes.FailedPrecondition, "base and head have no common history")
	case errors.Is(err, merge.ErrTargetBusy):
		return status.Error(codes.Aborted, "the target branch kept moving; try again")
	case errors.Is(err, browse.ErrPathNotFound):
		return status.Error(codes.NotFound, "path not found")
	case errors.Is(err, browse.ErrCommitNotFound):
		return status.Error(codes.NotFound, "commit not found")
	case errors.Is(err, repo.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, "the owner already has a repository with this name")
	case errors.Is(err, repo.ErrFilesExist):
		return status.Error(codes.AlreadyExists, "the repository already has its files")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}

	method, _ := grpc.Method(ctx)
	log.ErrorContext(ctx, "request failed", slog.String("method", method), slog.String("err", err.Error()))
	return errInternal
}

// refusalStatus is FAILED_PRECONDITION with one gitv1.MergeError detail, as
// operation.proto promises for a merge that is not possible.
func refusalStatus(ctx context.Context, log *slog.Logger, r *merge.Refusal) error {
	detail := &gitv1.MergeError{
		Reason:             mergeReasonToProto(r.Reason),
		SourceSha:          r.SourceSHA,
		TargetSha:          r.TargetSHA,
		Conflicts:          conflictsToProto(r.Conflicts),
		ConflictsTruncated: r.ConflictsTruncated,
	}
	st, err := status.New(codes.FailedPrecondition, r.Error()).WithDetails(detail)
	if err != nil {
		// Only a detail that cannot be marshaled fails here.
		log.ErrorContext(ctx, "attach merge error detail", slog.String("err", err.Error()))
		return errInternal
	}
	return st.Err()
}

func mergeReasonToProto(r merge.Reason) gitv1.MergeErrorReason {
	switch r {
	case merge.ReasonConflicts:
		return gitv1.MergeErrorReason_MERGE_ERROR_REASON_CONFLICTS
	case merge.ReasonFastForwardImpossible:
		return gitv1.MergeErrorReason_MERGE_ERROR_REASON_FAST_FORWARD_IMPOSSIBLE
	case merge.ReasonSourceMoved:
		return gitv1.MergeErrorReason_MERGE_ERROR_REASON_SOURCE_MOVED
	case merge.ReasonAlreadyMerged:
		return gitv1.MergeErrorReason_MERGE_ERROR_REASON_ALREADY_MERGED
	case merge.ReasonUnrelated:
		return gitv1.MergeErrorReason_MERGE_ERROR_REASON_UNRELATED
	default:
		return gitv1.MergeErrorReason_MERGE_ERROR_REASON_UNSPECIFIED
	}
}

func conflictsToProto(paths []string) []*gitv1.ConflictFile {
	files := make([]*gitv1.ConflictFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, &gitv1.ConflictFile{Path: p})
	}
	return files
}

// streamError returns the context's error if a stream failed because the
// call ended, so that toStatus reports it as such instead of INTERNAL. When
// a client cancels, Send and Recv return a gRPC status error that toStatus
// does not know, while the stream's context holds the reason.
func streamError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, io.EOF) {
		return ctxErr
	}
	return err
}
