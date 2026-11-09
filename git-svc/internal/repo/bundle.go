package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// WriteBundle writes the live repository id to w as a Git bundle of all its
// refs, for a backup (ADR 0015). A repository without refs has no bundle:
// then WriteBundle writes nothing and returns empty = true.
//
// Only ctx bounds the call. If w fails, its error is returned.
func (s *Service) WriteBundle(ctx context.Context, id uuid.UUID, w io.Writer) (empty bool, err error) {
	if _, err := s.Get(ctx, id); err != nil {
		return false, err
	}
	empty, err = s.store.WriteBundle(ctx, id, w)
	if err != nil {
		return false, fmt.Errorf("bundle repository %s: %w", id, err)
	}
	return empty, nil
}

// RestoreBundle recreates the files of the live repository id from the Git
// bundle read from bundle, with HEAD on the repository's default branch. No
// data at all creates an empty repository (ADR 0015).
//
// The row must exist, because the database is restored first, and the files
// must not: else it returns ErrNotFound or ErrFilesExist, without reading
// bundle. A bundle that git cannot use gives a *ValidationError for "data",
// and git's reason is logged. A restore writes no event: it is not a push.
func (s *Service) RestoreBundle(ctx context.Context, id uuid.UUID, bundle io.Reader) error {
	r, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	err = s.store.Restore(ctx, id, r.DefaultBranch, bundle)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrExists):
		return fmt.Errorf("restore repository %s: %w", id, ErrFilesExist)
	case errors.Is(err, storage.ErrInvalidBundle):
		s.log.WarnContext(ctx, "bundle rejected",
			slog.String("repository_id", id.String()), slog.String("err", err.Error()))
		return invalid("data", "is not a complete Git bundle with at least one ref")
	}
	return fmt.Errorf("restore repository %s: %w", id, err)
}
