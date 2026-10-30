package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
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

func TestPaths(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	id := uuid.MustParse("3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5f")

	tests := []struct {
		got, want string
	}{
		{l.RepoPath(id), filepath.Join(root, "repositories", "3f", "3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5f.git")},
		{l.TmpPath(id), filepath.Join(root, "tmp", "3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5f")},
		{l.TrashPath(id), filepath.Join(root, "trash", "3f2b8c1e-9d4a-4c2b-8e1f-0a1b2c3d4e5f.git")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("path = %s, want %s", tt.got, tt.want)
		}
	}
}

func TestPathsStayInside(t *testing.T) {
	l := NewLayout(t.TempDir())
	for range 1000 {
		id := uuid.New()
		if err := within(l.ReposDir(), l.RepoPath(id)); err != nil {
			t.Fatal(err)
		}
		if err := within(l.TmpDir(), l.TmpPath(id)); err != nil {
			t.Fatal(err)
		}
		if err := within(l.TrashDir(), l.TrashPath(id)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWithin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repositories")
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"child", filepath.Join(dir, "3f", "x.git"), true},
		{"direct child", filepath.Join(dir, "x"), true},
		{"dir itself", dir, false},
		{"parent", filepath.Dir(dir), false},
		{"sibling", filepath.Join(filepath.Dir(dir), "trash", "x.git"), false},
		{"dot-dot escape", dir + string(filepath.Separator) + filepath.Join("..", "..", "etc"), false},
		{"unrelated absolute", filepath.Join(t.TempDir(), "elsewhere"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := within(dir, tt.path); (err == nil) != tt.ok {
				t.Errorf("within(%s, %s) error = %v, want ok=%v", dir, tt.path, err, tt.ok)
			}
		})
	}
}
