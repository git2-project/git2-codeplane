package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

func newStore(t *testing.T) (*Store, *Layout, *gitcmd.Runner) {
	t.Helper()
	l := NewLayout(t.TempDir())
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	git, err := gitcmd.New(l.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewStore(l, git), l, git
}

func TestInit(t *testing.T) {
	s, l, git := newStore(t)
	id := uuid.New()

	if err := s.Init(t.Context(), id, "trunk"); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	path := l.RepoPath(id)
	gitOutput := func(args ...string) string {
		t.Helper()
		out, err := git.Run(t.Context(), path, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := gitOutput("rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("is-bare-repository = %q, want true", got)
	}
	if got := gitOutput("symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Errorf("HEAD = %q, want refs/heads/trunk", got)
	}
	assertEmptyDir(t, l.TmpDir())
}

func TestInitFailureLeavesNothing(t *testing.T) {
	s, l, _ := newStore(t)
	id := uuid.New()

	// git rejects this name; callers validate first, but Init must still
	// clean up if git fails.
	if err := s.Init(t.Context(), id, "bad..name"); err == nil {
		t.Fatal("Init() error = nil, want an error for an invalid branch")
	}
	if _, err := os.Stat(l.RepoPath(id)); !os.IsNotExist(err) {
		t.Errorf("repository directory exists after failed Init (stat error: %v)", err)
	}
	assertEmptyDir(t, l.TmpDir())
}

func TestRemove(t *testing.T) {
	s, l, _ := newStore(t)
	id := uuid.New()
	if err := s.Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}

	if err := s.Remove(id); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(l.RepoPath(id)); !os.IsNotExist(err) {
		t.Errorf("repository directory still exists (stat error: %v)", err)
	}
	// Removing again is not an error.
	if err := s.Remove(id); err != nil {
		t.Errorf("second Remove() error = %v", err)
	}
}

func TestTrash(t *testing.T) {
	s, l, _ := newStore(t)
	id := uuid.New()
	if err := s.Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}

	if err := s.Trash(id); err != nil {
		t.Fatalf("Trash() error = %v", err)
	}
	if _, err := os.Stat(l.RepoPath(id)); !os.IsNotExist(err) {
		t.Errorf("repository directory still exists (stat error: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(l.TrashPath(id), "HEAD")); err != nil {
		t.Errorf("trashed repository is incomplete: %v", err)
	}
	// Nothing left to trash.
	if err := s.Trash(id); err == nil {
		t.Error("second Trash() error = nil, want an error")
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s is not empty: %v", dir, entries)
	}
}
