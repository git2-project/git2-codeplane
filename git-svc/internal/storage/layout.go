// Package storage owns git-svc's on-disk layout under the storage root:
//
//	<root>/repositories/  live bare repositories
//	<root>/tmp/           repositories being created
//	<root>/trash/         deleted repositories awaiting purge
//	<root>/home/          HOME for git processes
//
// All of them sit under one root, so a rename between them never crosses a
// filesystem and is atomic.
package storage

import (
	"fmt"
	"os"
	"path/filepath"
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
