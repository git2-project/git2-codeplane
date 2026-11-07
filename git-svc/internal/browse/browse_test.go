package browse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// These tests read real repositories, built with real git and pushed over
// file://. The repository metadata comes from a fake, so no database is
// needed; package server tests the RPCs against PostgreSQL.

// env is a Service with its storage, plus a git client of its own.
type env struct {
	svc    *Service
	repos  *fakeRepos
	store  *storage.Store
	layout *storage.Layout
	client *gitcmd.Runner // the test's git, with a HOME of its own
}

func newEnv(t *testing.T) *env {
	t.Helper()
	layout := storage.NewLayout(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	git, err := gitcmd.New(layout.HomeDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := gitcmd.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repos := &fakeRepos{live: map[uuid.UUID]string{}}
	return &env{
		svc:    NewService(repos, layout, git),
		repos:  repos,
		store:  storage.NewStore(layout, git),
		layout: layout,
		client: client,
	}
}

// newRepo creates an empty repository with default branch main.
func (e *env) newRepo(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := e.store.Init(t.Context(), id, "main"); err != nil {
		t.Fatal(err)
	}
	e.repos.add(id, "main")
	return id
}

// work is a working copy that pushes to one repository.
type work struct {
	e   *env
	dir string
	url string
}

func (e *env) newWork(t *testing.T, id uuid.UUID) *work {
	t.Helper()
	w := &work{e: e, dir: filepath.Join(t.TempDir(), "w"), url: fileURL(e.layout.RepoPath(id))}
	w.git(t, 0, "", "init", "--quiet", "--initial-branch=main", w.dir)
	return w
}

// git runs the client's git in the working copy (or in dir when dir is
// not ""), with author and committer date at (Unix seconds) when at is
// not 0, and returns its trimmed output.
func (w *work) git(t *testing.T, at int64, dir string, args ...string) string {
	t.Helper()
	if dir == "" && len(args) > 0 && args[0] != "init" {
		dir = w.dir
	}
	env := []string{"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Cy", "GIT_COMMITTER_EMAIL=cy@example.com"}
	if at != 0 {
		date := fmt.Sprintf("@%d +0000", at)
		env = append(env, "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	}
	var out bytes.Buffer
	if err := w.e.client.Stream(t.Context(), gitcmd.StreamOptions{Dir: dir, Env: env, Stdout: &out}, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

// commit writes files (path to content), stages them and commits at time
// at. It returns the commit's SHA.
func (w *work) commit(t *testing.T, at int64, message string, files map[string]string) string {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(w.dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		w.git(t, 0, "", "add", "--", p)
	}
	w.git(t, at, "", "commit", "--quiet", "--allow-empty", "-m", message)
	return w.git(t, 0, "", "rev-parse", "HEAD")
}

// addSpecial stages an entry that cannot be written portably to the
// working tree: a symlink to target (mode 120000) or a submodule pointing
// at commit (mode 160000).
func (w *work) addSpecial(t *testing.T, mode, p, target string) {
	t.Helper()
	sha := target
	if mode == "120000" {
		sha = w.stdin(t, target, "hash-object", "-w", "--stdin")
	}
	w.git(t, 0, "", "update-index", "--add", "--cacheinfo", mode+","+sha+","+p)
}

// stdin runs the client's git in the working copy with input on its standard
// input, and returns its trimmed output.
func (w *work) stdin(t *testing.T, input string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	opts := gitcmd.StreamOptions{Dir: w.dir, Stdin: strings.NewReader(input), Stdout: &out}
	if err := w.e.client.Stream(t.Context(), opts, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out.String())
}

// mktree writes a tree of files (name to content) and returns its ID. It uses
// plumbing, so names work that a file system or the index would refuse.
func (w *work) mktree(t *testing.T, files map[string]string) string {
	t.Helper()
	var entries strings.Builder
	for name, content := range files {
		blob := w.stdin(t, content, "hash-object", "-w", "--stdin")
		fmt.Fprintf(&entries, "100644 blob %s\t%s\x00", blob, name)
	}
	return w.stdin(t, entries.String(), "mktree", "-z")
}

// addPackedRefs adds refs that point to sha to a repository without packed
// refs, by writing the file: a name that is not UTF-8 cannot be a loose ref on
// every system.
func (e *env) addPackedRefs(t *testing.T, id uuid.UUID, sha string, refs ...string) {
	t.Helper()
	content := "# pack-refs with: peeled fully-peeled \n"
	for _, ref := range refs {
		content += sha + " " + ref + "\n"
	}
	path := filepath.Join(e.layout.RepoPath(id), "packed-refs")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *work) push(t *testing.T, refspecs ...string) {
	t.Helper()
	w.git(t, 0, "", append([]string{"push", "--quiet", w.url}, refspecs...)...)
}

// fileURL turns an absolute path into a file:// URL, also on Windows.
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// sample is a repository with some history:
//
//	c1 (t=1000) README.md, docs/guide.md, src/main.go, bin.dat, a[1].txt, a1.txt
//	c2 (t=2000) README.md changed, docs/guide.md renamed to docs/manual.md,
//	            bin.dat deleted
//	c3 (t=3000) src/main.go changed, a1.txt changed, link and vendor/lib added
type sample struct {
	id         uuid.UUID
	w          *work
	c1, c2, c3 string
}

func newSample(t *testing.T, e *env) sample {
	t.Helper()
	s := sample{id: e.newRepo(t)}
	s.w = e.newWork(t, s.id)
	s.c1 = s.w.commit(t, 1000, "first\n\nwith a body", map[string]string{
		"README.md":     "hello\n",
		"docs/guide.md": strings.Repeat("guide line\n", 20),
		"src/main.go":   "package main\n",
		"bin.dat":       "\x00\x01binary",
		"a[1].txt":      "brackets\n",
		"a1.txt":        "plain\n",
	})
	s.w.git(t, 0, "", "mv", "docs/guide.md", "docs/manual.md")
	s.w.git(t, 0, "", "rm", "--quiet", "bin.dat")
	s.c2 = s.w.commit(t, 2000, "second", map[string]string{
		"README.md":      "hello\nworld\n",
		"docs/manual.md": strings.Repeat("guide line\n", 20) + "one more\n",
	})
	s.w.addSpecial(t, "120000", "link", "README.md")
	s.w.addSpecial(t, "160000", "vendor/lib", s.c1)
	s.c3 = s.w.commit(t, 3000, "third", map[string]string{
		"src/main.go": "package main\n\nfunc main() {}\n",
		"a1.txt":      "plain, changed\n",
	})
	s.w.push(t, "HEAD:refs/heads/main")
	return s
}

func wantErr(t *testing.T, call string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: error = %v, want %v", call, err, want)
	}
}

// fakeRepos knows which repositories are live and their default branch.
type fakeRepos struct {
	mu   sync.Mutex
	live map[uuid.UUID]string
}

func (f *fakeRepos) Get(_ context.Context, id uuid.UUID) (repo.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	branch, ok := f.live[id]
	if !ok {
		return repo.Repository{}, repo.ErrNotFound
	}
	return repo.Repository{ID: id, DefaultBranch: branch}, nil
}

func (f *fakeRepos) add(id uuid.UUID, branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[id] = branch
}

func TestUnknownRepository(t *testing.T) {
	e := newEnv(t)
	id := uuid.New()
	_, err := e.svc.GetTree(t.Context(), id, "", "")
	wantErr(t, "GetTree", err, repo.ErrNotFound)
	_, err = e.svc.ListBranches(t.Context(), id, "", 0, "")
	wantErr(t, "ListBranches", err, repo.ErrNotFound)
	_, err = e.svc.GetCommit(t.Context(), id, strings.Repeat("a", 40))
	wantErr(t, "GetCommit", err, repo.ErrNotFound)
}

func TestEmptyRepository(t *testing.T) {
	e := newEnv(t)
	id := e.newRepo(t)

	tree, err := e.svc.GetTree(t.Context(), id, "", "")
	if err != nil || !tree.Empty || tree.Ref != "main" || len(tree.Entries) != 0 || tree.LastCommit != nil {
		t.Errorf("GetTree() = %+v, %v; want empty for ref main", tree, err)
	}
	// Any ref and path: the repository has nothing to show yet.
	if tree, err := e.svc.GetTree(t.Context(), id, "dev", "docs"); err != nil || !tree.Empty {
		t.Errorf("GetTree(dev, docs) = %+v, %v; want empty", tree, err)
	}
	if page, err := e.svc.ListCommits(t.Context(), id, "", "", 0, ""); err != nil || len(page.Commits) != 0 {
		t.Errorf("ListCommits() = %+v, %v; want no commits", page, err)
	}
	if page, err := e.svc.ListBranches(t.Context(), id, "", 0, ""); err != nil || len(page.Branches) != 0 {
		t.Errorf("ListBranches() = %+v, %v; want none", page, err)
	}
	_, err = e.svc.GetBlob(t.Context(), id, "", "README.md")
	wantErr(t, "GetBlob", err, ErrRefNotFound)
}

func TestDefaultBranchWithoutCommits(t *testing.T) {
	e := newEnv(t)
	id := e.newRepo(t)
	w := e.newWork(t, id)
	c := w.commit(t, 1000, "first", map[string]string{"a.txt": "a\n"})
	w.push(t, "HEAD:refs/heads/dev")

	// Not empty, but the default branch main does not exist.
	_, err := e.svc.GetTree(t.Context(), id, "", "")
	wantErr(t, "GetTree", err, ErrRefNotFound)
	_, err = e.svc.ListCommits(t.Context(), id, "", "", 0, "")
	wantErr(t, "ListCommits", err, ErrRefNotFound)

	// The branch that exists can be found and read.
	page, err := e.svc.ListBranches(t.Context(), id, "", 0, "")
	if err != nil || branchNames(page.Branches) != "dev" || page.Branches[0].Default {
		t.Errorf("ListBranches() = %+v, %v; want dev, not default", page.Branches, err)
	}
	if tree, err := e.svc.GetTree(t.Context(), id, "dev", ""); err != nil || tree.CommitSHA != c {
		t.Errorf("GetTree(dev) = %+v, %v; want commit %s", tree, err, c)
	}
}
