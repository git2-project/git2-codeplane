package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "storage")
	l := NewLayout(root)

	// Twice: the second call must be a no-op, not an error.
	for range 2 {
		if err := l.EnsureDirs(); err != nil {
			t.Fatalf("EnsureDirs() error = %v", err)
		}
	}

	for _, dir := range []string{l.ReposDir(), l.TmpDir(), l.TrashDir(), l.HomeDir()} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", dir, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
		if filepath.Dir(dir) != root {
			t.Errorf("%s is not directly under the root %s", dir, root)
		}
	}
}
