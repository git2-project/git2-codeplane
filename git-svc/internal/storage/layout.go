// Package storage owns git-svc's on-disk layout under the storage root:
//
//	<root>/repositories/<shard>/<id>.git  live bare repositories
//	<root>/tmp/<id>                       repositories being created
//	<root>/trash/<id>.git                 deleted repositories awaiting purge
//	<root>/home/                          HOME for git processes
//
// <shard> is the first two hex characters of the repository ID, which
// spreads repositories over 256 directories. Paths are derived only from
// repository IDs, never from names or other user input.
//
// All directories sit under one root, so a rename between them never
// crosses a filesystem and is atomic.
package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// dirPerm is the permission for every directory git-svc creates.
const dirPerm = 0o750

// Layout maps git-svc's directories under a storage root.
type Layout struct {
	root string
}

// NewLayout returns the layout under root, which must be an absolute,
// cleaned path.
func NewLayout(root string) *Layout {
	return &Layout{root: root}
}

// Root returns the storage root.
func (l *Layout) Root() string { return l.root }

// ReposDir returns the directory that holds live repositories.
func (l *Layout) ReposDir() string { return filepath.Join(l.root, "repositories") }

// TmpDir returns the staging directory for repositories being created.
func (l *Layout) TmpDir() string { return filepath.Join(l.root, "tmp") }

// TrashDir returns the directory that holds deleted repositories.
func (l *Layout) TrashDir() string { return filepath.Join(l.root, "trash") }

// HomeDir returns the HOME directory for git processes.
func (l *Layout) HomeDir() string { return filepath.Join(l.root, "home") }

// EnsureDirs creates the layout's directories if they do not exist.
func (l *Layout) EnsureDirs() error {
	for _, dir := range []string{l.ReposDir(), l.TmpDir(), l.TrashDir(), l.HomeDir()} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("create storage directory: %w", err)
		}
	}
	return nil
}

// RepoPath returns where the live repository id is stored:
// <root>/repositories/<shard>/<id>.git.
func (l *Layout) RepoPath(id uuid.UUID) string {
	s := id.String()
	return filepath.Join(l.ReposDir(), s[:2], s+".git")
}

// TmpPath returns where the repository id is built before it is moved to
// RepoPath: <root>/tmp/<id>.
func (l *Layout) TmpPath(id uuid.UUID) string {
	return filepath.Join(l.TmpDir(), id.String())
}

// TrashPath returns where the deleted repository id is kept until it is
// purged: <root>/trash/<id>.git.
func (l *Layout) TrashPath(id uuid.UUID) string {
	return filepath.Join(l.TrashDir(), id.String()+".git")
}

// within returns an error unless path lies strictly inside dir. Paths built
// from UUIDs always do; the check guards against future layout changes
// that could let a path escape the storage root.
func within(dir, path string) error {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("path %q is not inside %q", path, dir)
	}
	return nil
}
