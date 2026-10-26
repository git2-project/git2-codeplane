package server

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// toStatus turns an error from package repo into a gRPC status error.
//
// Client-facing messages are fixed strings or a *repo.ValidationError,
// which never repeats user input. Any other error can contain paths, SQL
// or git output, so it is logged here and the client only sees a generic
// INTERNAL error.
func toStatus(ctx context.Context, log *slog.Logger, err error) error {
	var invalid *repo.ValidationError
	switch {
	case errors.As(err, &invalid):
		return status.Error(codes.InvalidArgument, invalid.Error())
	case errors.Is(err, repo.ErrNotFound):
		return status.Error(codes.NotFound, "repository not found")
	case errors.Is(err, repo.ErrAlreadyExists):
		return status.Error(codes.AlreadyExists, "the owner already has a repository with this name")
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	}

	method, _ := grpc.Method(ctx)
	log.ErrorContext(ctx, "request failed", slog.String("method", method), slog.String("err", err.Error()))
	return errInternal
}
