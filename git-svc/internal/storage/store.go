package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

// Store creates, trashes and removes bare repositories on disk.
type Store struct {
	layout *Layout
	git    *gitcmd.Runner
}

// NewStore returns a Store that keeps repositories in layout and runs git
// through git.
func NewStore(layout *Layout, git *gitcmd.Runner) *Store {
	return &Store{layout: layout, git: git}
}

// Init creates an empty bare repository for id whose HEAD points to
// refs/heads/<branch>. The branch name must already be validated.
//
// The repository is built in the tmp directory and then renamed into
// place. A rename is atomic, so RepoPath(id) either holds a complete
// repository or nothing. On failure nothing is left behind.
func (s *Store) Init(ctx context.Context, id uuid.UUID, branch string) (err error) {
	tmp, final := s.layout.TmpPath(id), s.layout.RepoPath(id)
	if err := within(s.layout.TmpDir(), tmp); err != nil {
		return err
	}
	if err := within(s.layout.ReposDir(), final); err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp) // best effort: git may have created part of it
		}
	}()

	// The branch is attached to its flag, and "--" ends the options, so
	// neither value can be read as another option.
	if _, err := s.git.Run(ctx, "", "init", "--quiet", "--bare", "--initial-branch="+branch, "--", tmp); err != nil {
		return fmt.Errorf("init bare repository: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return fmt.Errorf("create shard directory: %w", err)
	}
	// IDs are random, so this cannot happen in practice. Check anyway:
	// on Linux, rename would silently replace an empty directory.
	if err := mustNotExist(final); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("move repository into place: %w", err)
	}
	return nil
}

// Remove deletes the repository directory of id for good. It is used to
// undo Init when the database insert fails. A missing directory is not an
// error.
func (s *Store) Remove(id uuid.UUID) error {
	path := s.layout.RepoPath(id)
	if err := within(s.layout.ReposDir(), path); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove repository directory: %w", err)
	}
	return nil
}

// Trash moves the repository directory of id to the trash, where it waits
// to be purged.
func (s *Store) Trash(id uuid.UUID) error {
	from, to := s.layout.RepoPath(id), s.layout.TrashPath(id)
	if err := within(s.layout.ReposDir(), from); err != nil {
		return err
	}
	if err := within(s.layout.TrashDir(), to); err != nil {
		return err
	}
	if err := mustNotExist(to); err != nil {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("move repository to trash: %w", err)
	}
	return nil
}

// mustNotExist returns an error if path exists or cannot be checked.
func mustNotExist(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%s already exists", path)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("check %s: %w", path, err)
	}
	return nil
}
