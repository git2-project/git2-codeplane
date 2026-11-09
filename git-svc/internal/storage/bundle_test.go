package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

func TestBundleRoundTrip(t *testing.T) {
	src, srcLayout, git := newStore(t)
	id := uuid.New()
	if err := src.Init(t.Context(), id, "trunk"); err != nil {
		t.Fatal(err)
	}
	work := newWorkRepo(t, git)
	run(t, git, work, "push", "--quiet", "--mirror", srcLayout.RepoPath(id))
	wantRefs := refs(t, git, srcLayout.RepoPath(id))
	for _, ref := range []string{"refs/heads/trunk", "refs/heads/feature", "refs/tags/v1", "refs/tags/light", "refs/notes/commits", "refs/custom/kept"} {
		if !strings.Contains(wantRefs, " "+ref+"\n") {
			t.Fatalf("source repository lacks %s; refs:\n%s", ref, wantRefs)
		}
	}

	var bundle bytes.Buffer
	empty, err := src.WriteBundle(t.Context(), id, &bundle)
	if err != nil || empty {
		t.Fatalf("WriteBundle() = empty %v, error %v; want a bundle", empty, err)
	}

	// Restore on another storage root, as on a new server.
	dst, dstLayout, _ := newStore(t)
	if err := dst.Restore(t.Context(), id, "trunk", &bundle); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	path := dstLayout.RepoPath(id)
	if got := refs(t, git, path); got != wantRefs {
		t.Errorf("restored refs:\n%s\nwant:\n%s", got, wantRefs)
	}
	if got := run(t, git, path, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Errorf("HEAD = %q, want refs/heads/trunk", got)
	}
	if got := run(t, git, path, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("is-bare-repository = %q, want true", got)
	}
	run(t, git, path, "fsck", "--strict", "--no-dangling")
	// Restoring copies objects and refs only: no remote, no FETCH_HEAD.
	if out, err := git.Run(t.Context(), path, "config", "--get-regexp", "^remote\\."); err == nil {
		t.Errorf("restored repository has remote config: %s", out)
	}
	if _, err := os.Stat(filepath.Join(path, "FETCH_HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restored repository has FETCH_HEAD (stat error: %v)", err)
	}
	assertEmptyDir(t, srcLayout.TmpDir())
	assertEmptyDir(t, dstLayout.TmpDir())
}

func TestBundleOfEmptyRepository(t *testing.T) {
	s, l, git := newStore(t)
	id := uuid.New()
	if err := s.Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	empty, err := s.WriteBundle(t.Context(), id, &out)
	if err != nil || !empty || out.Len() != 0 {
		t.Fatalf("WriteBundle() = empty %v, %d bytes, error %v; want empty, no bytes", empty, out.Len(), err)
	}

	// No data restores an empty repository with HEAD on the given branch.
	dst, dstLayout, _ := newStore(t)
	if err := dst.Restore(t.Context(), id, "trunk", strings.NewReader("")); err != nil {
		t.Fatalf("Restore(no data) error = %v", err)
	}
	path := dstLayout.RepoPath(id)
	if got := refs(t, git, path); got != "" {
		t.Errorf("refs of the restored empty repository:\n%s", got)
	}
	if got := run(t, git, path, "symbolic-ref", "HEAD"); got != "refs/heads/trunk" {
		t.Errorf("HEAD = %q, want refs/heads/trunk", got)
	}
	assertEmptyDir(t, l.TmpDir())
	assertEmptyDir(t, dstLayout.TmpDir())
}

func TestRestoreRejectsBadBundles(t *testing.T) {
	_, _, git := newStore(t)
	work := newWorkRepo(t, git)
	dir := t.TempDir()
	bundleOf := func(name string, revs ...string) []byte {
		t.Helper()
		file := filepath.Join(dir, name)
		run(t, git, work, append([]string{"bundle", "create", "--quiet", file}, revs...)...)
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	full := bundleOf("full.bundle", "--all")

	tests := []struct {
		name string
		data []byte
	}{
		{"not a bundle", bytes.Repeat([]byte("garbage\n"), 100)},
		{"cut in the pack", full[:len(full)-300]},
		{"cut before the first ref", full[:20]},
		{"header only", full[:bytes.Index(full, []byte("\n\n"))+2]},
		{"only HEAD", bundleOf("head.bundle", "HEAD")},
		{"needs a commit it lacks", bundleOf("thin.bundle", "trunk~1..trunk")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, l, _ := newStore(t)
			id := uuid.New()
			err := s.Restore(t.Context(), id, "main", bytes.NewReader(tt.data))
			if !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Restore() error = %v, want ErrInvalidBundle", err)
			}
			if _, err := os.Stat(l.RepoPath(id)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("repository directory exists after a failed restore (stat error: %v)", err)
			}
			assertEmptyDir(t, l.TmpDir())
		})
	}
}

func TestRestoreOverExistingRepository(t *testing.T) {
	s, l, git := newStore(t)
	id := uuid.New()
	if err := s.Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(l.RepoPath(id), "description")
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}

	err = s.Restore(t.Context(), id, "trunk", readerFunc(func([]byte) (int, error) {
		t.Error("Restore read the bundle of a repository that exists")
		return 0, io.EOF
	}))
	if !errors.Is(err, ErrExists) {
		t.Fatalf("Restore() error = %v, want ErrExists", err)
	}
	// The repository is untouched.
	if got := run(t, git, l.RepoPath(id), "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("HEAD = %q, want refs/heads/main", got)
	}
	if after, err := os.ReadFile(marker); err != nil || !bytes.Equal(after, before) {
		t.Errorf("description changed: %q (error %v)", after, err)
	}
	assertEmptyDir(t, l.TmpDir())
}

func TestRestoreReadError(t *testing.T) {
	s, l, _ := newStore(t)
	id := uuid.New()
	errCut := errors.New("connection lost")
	sent := false
	err := s.Restore(t.Context(), id, "main", readerFunc(func(p []byte) (int, error) {
		if sent {
			return 0, errCut
		}
		sent = true
		return copy(p, "# v2 git bundle\n"), nil
	}))
	if !errors.Is(err, errCut) {
		t.Fatalf("Restore() error = %v, want the read error", err)
	}
	if _, err := os.Stat(l.RepoPath(id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("repository directory exists after a failed restore (stat error: %v)", err)
	}
	assertEmptyDir(t, l.TmpDir())
}

func TestWriteBundleStopsWhenWriterFails(t *testing.T) {
	s, l, git := newStore(t)
	id := uuid.New()
	if err := s.Init(t.Context(), id, "trunk"); err != nil {
		t.Fatal(err)
	}
	run(t, git, newWorkRepo(t, git), "push", "--quiet", "--mirror", l.RepoPath(id))

	errGone := errors.New("client went away")
	_, err := s.WriteBundle(t.Context(), id, writerFunc(func([]byte) (int, error) { return 0, errGone }))
	if !errors.Is(err, errGone) {
		t.Errorf("WriteBundle() error = %v, want the writer's error", err)
	}
}

// TestBundleWhilePushing takes bundles while pushes land. Each bundle must
// restore to a complete repository whose refs are those of one moment.
func TestBundleWhilePushing(t *testing.T) {
	s, l, git := newStore(t)
	id := uuid.New()
	if err := s.Init(t.Context(), id, "trunk"); err != nil {
		t.Fatal(err)
	}
	bare := l.RepoPath(id)
	work := newWorkRepo(t, git)
	run(t, git, work, "push", "--quiet", "--mirror", bare)

	const pushes = 15
	done := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait) // also when the test stops early
	wg.Go(func() {
		defer close(done)
		// Not the test goroutine: report with t.Error, never t.Fatal.
		for i := range pushes {
			name := "f" + strconv.Itoa(i)
			if err := os.WriteFile(filepath.Join(work, name), bytes.Repeat([]byte(name), 5000), 0o600); err != nil {
				t.Error(err)
				return
			}
			for _, args := range [][]string{
				{"add", "--", name},
				{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "add " + name},
				{"push", "--quiet", bare, "HEAD:refs/heads/trunk", "HEAD:refs/heads/b" + strconv.Itoa(i)},
			} {
				if _, err := git.Run(t.Context(), work, args...); err != nil {
					t.Error(err)
					return
				}
			}
		}
	})

	// Bundle until the pushes are over, then once more.
	var bundles int
	var lastRefs string
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		var b bytes.Buffer
		if _, err := s.WriteBundle(t.Context(), id, &b); err != nil {
			t.Fatalf("WriteBundle() during pushes: %v", err)
		}
		dst, dstLayout, _ := newStore(t)
		if err := dst.Restore(t.Context(), id, "trunk", &b); err != nil {
			t.Fatalf("Restore() of bundle %d: %v", bundles, err)
		}
		run(t, git, dstLayout.RepoPath(id), "fsck", "--strict", "--connectivity-only")
		lastRefs = refs(t, git, dstLayout.RepoPath(id))
		bundles++
	}
	wg.Wait()

	// The last bundle was taken after the last push, so it has every ref.
	if want := refs(t, git, bare); lastRefs != want {
		t.Errorf("refs of the last bundle:\n%s\nwant:\n%s", lastRefs, want)
	}
	t.Logf("%d bundles taken during %d pushes", bundles, pushes)
}

// newWorkRepo returns a non-bare repository with two branches, an annotated
// and a lightweight tag, a note and a ref outside heads and tags.
func newWorkRepo(t *testing.T, git *gitcmd.Runner) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	run(t, git, "", "init", "--quiet", "--initial-branch=trunk", work)
	for _, name := range []string{"README.md", "main.go"} {
		if err := os.WriteFile(filepath.Join(work, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		commit(t, git, work, name)
	}
	run(t, git, work, "branch", "feature", "HEAD~1")
	run(t, git, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "tag", "-a", "-m", "release 1", "v1")
	run(t, git, work, "tag", "light", "HEAD~1")
	run(t, git, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "notes", "add", "-m", "a note")
	run(t, git, work, "update-ref", "refs/custom/kept", "HEAD~1")
	return work
}

// commit stages name in work and commits it.
func commit(t *testing.T, git *gitcmd.Runner, work, name string) {
	t.Helper()
	run(t, git, work, "add", "--", name)
	run(t, git, work, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "add "+name)
}

// refs lists every ref of the repository in dir with its object, one per
// line.
func refs(t *testing.T, git *gitcmd.Runner, dir string) string {
	t.Helper()
	out, err := git.Run(t.Context(), dir, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// run runs git in dir and returns its trimmed stdout, failing the test if
// git fails.
func run(t *testing.T, git *gitcmd.Runner, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
