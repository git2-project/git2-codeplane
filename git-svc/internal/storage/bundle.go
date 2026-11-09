package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

// Errors of the Store, matched with errors.Is.
var (
	// ErrExists means a directory the Store was about to create is already
	// there.
	ErrExists = errors.New("already exists")
	// ErrInvalidBundle means git could not build a repository from the data
	// given to Restore: it is not a bundle, it is cut short or damaged, it
	// needs commits it does not carry, or it has no refs.
	ErrInvalidBundle = errors.New("not a complete git bundle")
)

// Names inside a restore's staging directory.
const (
	stagedBundle = "repository.bundle"
	stagedRepo   = "repository.git"
)

// WriteBundle writes the repository id to w as a Git bundle of every ref
// and HEAD (`git bundle create --all`). Git cannot bundle a repository
// without refs: then WriteBundle writes nothing and returns empty = true.
//
// Only ctx bounds the call; a large repository can take minutes. If w
// fails, git is stopped and w's error is returned.
func (s *Store) WriteBundle(ctx context.Context, id uuid.UUID, w io.Writer) (empty bool, err error) {
	dir := s.layout.RepoPath(id)
	if err := within(s.layout.ReposDir(), dir); err != nil {
		return false, err
	}
	hasRefs, err := s.hasRefs(ctx, dir)
	if err != nil {
		return false, err
	}
	if !hasRefs {
		return true, nil
	}

	out := &writeErrorKeeper{w: w}
	err = s.git.Stream(ctx, gitcmd.StreamOptions{Dir: dir, Stdout: out}, "bundle", "create", "--quiet", "-", "--all")
	if out.err != nil {
		return false, out.err
	}
	if err != nil {
		return false, fmt.Errorf("create bundle: %w", err)
	}
	return false, nil
}

// Restore creates the repository id from the Git bundle read from bundle,
// with HEAD pointing to refs/heads/<branch>; the branch name must already
// be valid. Every ref of the bundle under refs/ is written as it is. No data
// at all creates an empty repository, as Init does.
//
// The bundle is saved to a staging directory under tmp, git builds the
// repository next to it, and the repository is then renamed into place, so
// RepoPath(id) holds a complete repository or nothing. The staging
// directory is removed in any case.
//
// Restore returns an error matching ErrExists if RepoPath(id) is taken,
// before it reads anything, and one matching ErrInvalidBundle, with git's
// message, if git cannot use the bundle. A read error of bundle is returned
// wrapped. Only ctx bounds the call.
func (s *Store) Restore(ctx context.Context, id uuid.UUID, branch string, bundle io.Reader) error {
	final := s.layout.RepoPath(id)
	if err := within(s.layout.ReposDir(), final); err != nil {
		return err
	}
	if err := mustNotExist(final); err != nil {
		return err
	}

	// A directory of its own, so that two restores of the same ID cannot
	// share files; the second one fails at the rename.
	staging, err := os.MkdirTemp(s.layout.TmpDir(), id.String()+".restore-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // best effort

	file := filepath.Join(staging, stagedBundle)
	size, err := saveFile(file, bundle)
	if err != nil {
		return fmt.Errorf("receive bundle: %w", err)
	}

	repo := filepath.Join(staging, stagedRepo)
	// As in Init: the branch is attached to its flag and "--" ends the
	// options.
	if _, err := s.git.Run(ctx, "", "init", "--quiet", "--bare", "--initial-branch="+branch, "--", repo); err != nil {
		return fmt.Errorf("init bare repository: %w", err)
	}
	if size > 0 {
		if err := s.fetchBundle(ctx, repo, file); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return fmt.Errorf("create shard directory: %w", err)
	}
	if err := mustNotExist(final); err != nil {
		return err
	}
	if err := os.Rename(repo, final); err != nil {
		// Another restore of the same ID got there first.
		if _, statErr := os.Lstat(final); statErr == nil {
			return fmt.Errorf("%s %w", final, ErrExists)
		}
		return fmt.Errorf("move repository into place: %w", err)
	}
	return nil
}

// fetchBundle copies every ref under refs/ of the bundle in file, with its
// objects, into the bare repository dir.
func (s *Store) fetchBundle(ctx context.Context, dir, file string) error {
	// git checks the bundle while it reads it: a pack that is cut short or
	// damaged, or a missing prerequisite commit, makes the fetch fail.
	// `git bundle verify` is not enough, as it reads only the header. No
	// hooks run: the new repository has only git's samples.
	err := s.git.Stream(ctx, gitcmd.StreamOptions{Dir: dir},
		"fetch", "--quiet", "--no-write-fetch-head", "--no-auto-maintenance", "--recurse-submodules=no",
		file, "+refs/*:refs/*")
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("fetch from bundle: %w", err)
		}
		return fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}

	// A bundle cut off before its first ref, or one with only HEAD, fetches
	// nothing without an error.
	hasRefs, err := s.hasRefs(ctx, dir)
	if err != nil {
		return err
	}
	if !hasRefs {
		return fmt.Errorf("%w: no refs under refs/", ErrInvalidBundle)
	}
	return nil
}

// hasRefs reports whether the repository in dir has at least one ref.
func (s *Store) hasRefs(ctx context.Context, dir string) (bool, error) {
	out, err := s.git.Run(ctx, dir, "for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil {
		return false, fmt.Errorf("list refs: %w", err)
	}
	return len(out) > 0, nil
}

// saveFile copies r to a new file at path and returns the number of bytes
// written.
func saveFile(path string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// writeErrorKeeper remembers the first error of w, for example a client that
// went away, so that it is reported instead of git's reaction to it.
type writeErrorKeeper struct {
	w   io.Writer
	err error
}

func (k *writeErrorKeeper) Write(p []byte) (int, error) {
	if k.err != nil {
		return 0, k.err
	}
	n, err := k.w.Write(p)
	k.err = err
	return n, err
}
