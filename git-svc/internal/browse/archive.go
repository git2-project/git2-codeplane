package browse

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

// Archive is a commit found by OpenArchive, ready to be written by
// WriteArchive.
type Archive struct {
	Ref       string // the request's ref, or the default branch
	CommitSHA string
	dir       string
}

// OpenArchive finds the commit at ref (the default branch if empty), for
// WriteArchive. A repository without commits has no ref to archive.
func (s *Service) OpenArchive(ctx context.Context, id uuid.UUID, ref string) (Archive, error) {
	r, err := s.open(ctx, id)
	if err != nil {
		return Archive{}, err
	}
	res, err := s.resolve(ctx, r, ref)
	if errors.Is(err, errEmpty) {
		err = ErrRefNotFound
	}
	if err != nil {
		return Archive{}, err
	}
	return Archive{Ref: res.ref, CommitSHA: res.commit, dir: r.dir}, nil
}

// WriteArchive writes the files of a's commit to w as a tar archive, exactly
// as committed (ADR 0016). If w fails, git stops and the error is returned.
//
// git archive applies the .gitattributes of the archived tree by default:
// export-ignore leaves files out, and export-subst and line-ending
// attributes change content. With --worktree-attributes it reads them from
// the working tree instead, which a bare repository does not have. git-svc
// writes no info/attributes and no global attributes file either, so
// nothing is applied. tar.umask=022 gives the modes of a usual checkout:
// 0644, and 0755 for executables and directories.
func (s *Service) WriteArchive(ctx context.Context, a Archive, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	failed := &failingWriter{w: w}
	err := s.git.Stream(ctx, gitcmd.StreamOptions{Dir: a.dir, Stdout: failed},
		"-c", "tar.umask=022", "archive", "--format=tar", "--worktree-attributes", a.CommitSHA)
	if failed.err != nil {
		return failed.err
	}
	if err != nil {
		return fmt.Errorf("archive commit: %w", err)
	}
	return nil
}
