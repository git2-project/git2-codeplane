package merge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// These tests merge real repositories, built with real git and pushed over
// file://. The repository rows and the events go to a fake, so no database
// is needed; package server tests the RPCs.

// mergeTime is git-svc's clock in these tests.
const mergeTime = 9000

type env struct {
	svc    *Service
	repos  *fakeRepos
	layout *storage.Layout
	store  *storage.Store
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
	repos := &fakeRepos{live: map[uuid.UUID]bool{}}
	svc := NewService(repos, layout, git, slog.New(slog.DiscardHandler))
	svc.now = func() time.Time { return time.Unix(mergeTime, 0) }
	return &env{svc: svc, repos: repos, layout: layout, store: storage.NewStore(layout, git), client: client}
}

// fakeRepos knows which repositories are live and keeps the events.
type fakeRepos struct {
	mu        sync.Mutex
	live      map[uuid.UUID]bool
	pushes    []repo.Push
	recordErr error
}

func (f *fakeRepos) Get(_ context.Context, id uuid.UUID) (repo.Repository, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[id] {
		return repo.Repository{}, repo.ErrNotFound
	}
	return repo.Repository{ID: id, DefaultBranch: "main"}, nil
}

func (f *fakeRepos) RecordPush(_ context.Context, p repo.Push) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.pushes = append(f.pushes, p)
	return nil
}

func (f *fakeRepos) events() []repo.Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushes)
}

// scenario is a repository with branches for every kind of merge into main:
//
//	c1 (t=1000)          f.txt "one/two", k.txt         clean, conflict, same, old start here
//	clean (t=2000)       adds n.txt
//	clean2 (t=2050)      adds n2.txt
//	conflict (t=2100)    f.txt "one/FEAT"
//	same (t=2200)        f.txt "one/MAIN", adds m.txt: main's change, made again
//	m1 (t=3000, main)    f.txt "one/MAIN", adds m.txt
//	ahead (t=4000)       on m1, adds a.txt: main can fast-forward to it
//	other (t=5000)       a root commit with no common history
//	old                  points to c1, which main already contains
type scenario struct {
	id                                                  uuid.UUID
	w                                                   *work
	c1, clean, clean2, conflict, same, m1, ahead, other string
}

func newScenario(t *testing.T, e *env) scenario {
	t.Helper()
	s := scenario{id: uuid.New()}
	if err := e.store.Init(t.Context(), s.id, "main"); err != nil {
		t.Fatal(err)
	}
	e.repos.mu.Lock()
	e.repos.live[s.id] = true
	e.repos.mu.Unlock()

	w := &work{e: e, dir: filepath.Join(t.TempDir(), "w"), url: fileURL(e.layout.RepoPath(s.id))}
	w.git(t, 0, "", "init", "--quiet", "--initial-branch=main", w.dir)
	s.w = w
	s.c1 = w.commit(t, 1000, "base", map[string]string{"f.txt": "one\ntwo\n", "k.txt": "keep\n"})
	s.clean = w.branch(t, "clean", s.c1, 2000, map[string]string{"n.txt": "new\n"})
	s.clean2 = w.branch(t, "clean2", s.c1, 2050, map[string]string{"n2.txt": "new 2\n"})
	s.conflict = w.branch(t, "conflict", s.c1, 2100, map[string]string{"f.txt": "one\nFEAT\n"})
	s.same = w.branch(t, "same", s.c1, 2200, map[string]string{"f.txt": "one\nMAIN\n", "m.txt": "main\n"})
	w.git(t, 0, "", "switch", "--quiet", "main")
	s.m1 = w.commit(t, 3000, "main change", map[string]string{"f.txt": "one\nMAIN\n", "m.txt": "main\n"})
	s.ahead = w.branch(t, "ahead", s.m1, 4000, map[string]string{"a.txt": "ahead\n"})
	w.git(t, 0, "", "switch", "--quiet", "main")
	tree := w.mktree(t, "other.txt", "other\n")
	s.other = w.git(t, 5000, "", "commit-tree", tree, "-m", "unrelated root")
	w.push(t, "main:refs/heads/main", "clean:refs/heads/clean", "clean2:refs/heads/clean2",
		"conflict:refs/heads/conflict", "same:refs/heads/same", "ahead:refs/heads/ahead",
		s.other+":refs/heads/other", s.c1+":refs/heads/old")
	return s
}

// work is a working copy that pushes to one repository.
type work struct {
	e   *env
	dir string
	url string
}

// git runs the client's git in the working copy (or in dir when dir is not
// ""), dated at (Unix seconds) when at is not 0, and returns its trimmed
// output.
func (w *work) git(t *testing.T, at int64, dir string, args ...string) string {
	t.Helper()
	if dir == "" && args[0] != "init" {
		dir = w.dir
	}
	env := []string{"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com"}
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

// commit writes files (path to content), stages everything and commits at
// time at. It returns the commit's SHA.
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
	}
	w.git(t, 0, "", "add", "--all")
	w.git(t, at, "", "commit", "--quiet", "--allow-empty", "-m", message)
	return w.git(t, 0, "", "rev-parse", "HEAD")
}

// branch creates branch name at start and commits files on it.
func (w *work) branch(t *testing.T, name, start string, at int64, files map[string]string) string {
	t.Helper()
	w.git(t, 0, "", "switch", "--quiet", "-c", name, start)
	return w.commit(t, at, name, files)
}

// mktree writes a tree with one file and returns its ID.
func (w *work) mktree(t *testing.T, name, content string) string {
	t.Helper()
	var out bytes.Buffer
	run := func(input string, args ...string) string {
		out.Reset()
		opts := gitcmd.StreamOptions{Dir: w.dir, Stdin: strings.NewReader(input), Stdout: &out}
		if err := w.e.client.Stream(t.Context(), opts, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(out.String())
	}
	blob := run(content, "hash-object", "-w", "--stdin")
	return run(fmt.Sprintf("100644 blob %s\t%s\n", blob, name), "mktree")
}

func (w *work) push(t *testing.T, refspecs ...string) {
	t.Helper()
	w.git(t, 0, "", append([]string{"push", "--quiet", "--force", w.url}, refspecs...)...)
}

// bare runs the client's git in the repository itself.
func (e *env) bare(t *testing.T, id uuid.UUID, args ...string) string {
	t.Helper()
	out, err := e.client.Run(t.Context(), e.layout.RepoPath(id), args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) tip(t *testing.T, id uuid.UUID, branch string) string {
	t.Helper()
	return e.bare(t, id, "rev-parse", "refs/heads/"+branch)
}

func (e *env) parents(t *testing.T, id uuid.UUID, sha string) []string {
	t.Helper()
	return strings.Fields(e.bare(t, id, "rev-list", "--parents", "-n", "1", sha))[1:]
}

func (e *env) file(t *testing.T, id uuid.UUID, sha, path string) string {
	t.Helper()
	return e.bare(t, id, "show", sha+":"+path)
}

// fileURL turns an absolute path into a file:// URL, also on Windows.
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

var (
	merger = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	deniz  = &Identity{Name: "Deniz Yılmaz", Email: "deniz@example.com"}
	selin  = &Identity{Name: "Selin", Email: "selin@example.com"}
)

// request merges source into main as the reviewers saw it at sha.
func request(s scenario, source, sha string, strategy Strategy) Request {
	return Request{
		RepositoryID:      s.id,
		SourceBranch:      source,
		TargetBranch:      "main",
		Strategy:          strategy,
		ExpectedSourceSHA: sha,
		Message:           "Merge branch '" + source + "' into 'main'",
		Author:            deniz,
		MergerID:          merger,
	}
}

func TestCheckMerge(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	for _, tc := range []struct {
		source      string
		status      Status
		mergeBase   string
		fastForward bool
		conflicts   []string
		sourceSHA   string
	}{
		{source: "clean", status: StatusClean, mergeBase: s.c1, sourceSHA: s.clean},
		{source: "ahead", status: StatusClean, mergeBase: s.m1, fastForward: true, sourceSHA: s.ahead},
		{source: "conflict", status: StatusConflicts, mergeBase: s.c1, conflicts: []string{"f.txt"}, sourceSHA: s.conflict},
		{source: "old", status: StatusAlreadyMerged, mergeBase: s.c1, sourceSHA: s.c1},
		{source: "same", status: StatusAlreadyMerged, mergeBase: s.c1, sourceSHA: s.same},
		{source: "other", status: StatusUnrelated, sourceSHA: s.other},
	} {
		c, err := e.svc.CheckMerge(t.Context(), s.id, tc.source, "main")
		if err != nil {
			t.Fatalf("CheckMerge(%s): %v", tc.source, err)
		}
		if c.Status != tc.status || c.CanFastForward != tc.fastForward || !slices.Equal(c.Conflicts, tc.conflicts) {
			t.Errorf("CheckMerge(%s) = status %d, fast-forward %v, conflicts %v; want %d, %v, %v",
				tc.source, c.Status, c.CanFastForward, c.Conflicts, tc.status, tc.fastForward, tc.conflicts)
		}
		if c.SourceSHA != tc.sourceSHA || c.TargetSHA != s.m1 || c.MergeBaseSHA != tc.mergeBase {
			t.Errorf("CheckMerge(%s) SHAs = %s %s %s, want %s %s %s",
				tc.source, c.SourceSHA, c.TargetSHA, c.MergeBaseSHA, tc.sourceSHA, s.m1, tc.mergeBase)
		}
	}
	if e.tip(t, s.id, "main") != s.m1 || len(e.repos.events()) != 0 {
		t.Error("CheckMerge changed main or wrote an event")
	}
}

func TestCheckMergeManyConflicts(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)
	files := func(content string) map[string]string {
		m := map[string]string{}
		for i := range maxConflicts + 1 {
			m[fmt.Sprintf("many/f%03d.txt", i)] = content
		}
		return m
	}
	base := s.w.branch(t, "many-base", s.m1, 6000, files("base\n"))
	s.w.branch(t, "many-a", base, 6100, files("a\n"))
	s.w.branch(t, "many-b", base, 6200, files("b\n"))
	s.w.push(t, "many-a:refs/heads/many-a", "many-b:refs/heads/many-b")

	c, err := e.svc.CheckMerge(t.Context(), s.id, "many-a", "many-b")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusConflicts || len(c.Conflicts) != maxConflicts || !c.ConflictsTruncated || c.Conflicts[0] != "many/f000.txt" {
		t.Errorf("status %d, %d conflicts (truncated %v, first %q); want %d, truncated", c.Status, len(c.Conflicts), c.ConflictsTruncated, c.Conflicts[0], maxConflicts)
	}
}

func TestCheckMergeErrors(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	for _, tc := range []struct {
		name           string
		id             uuid.UUID
		source, target string
		field          string
		want           error
	}{
		{name: "unknown source", id: s.id, source: "nope", target: "main", want: ErrBranchNotFound},
		{name: "unknown target", id: s.id, source: "clean", target: "nope", want: ErrBranchNotFound},
		{name: "same branch", id: s.id, source: "main", target: "main", field: "target_branch"},
		{name: "bad source", id: s.id, source: "-x", target: "main", field: "source_branch"},
		{name: "bad target", id: s.id, source: "clean", target: "a..b", field: "target_branch"},
		{name: "unknown repository", id: uuid.New(), source: "clean", target: "main", want: repo.ErrNotFound},
	} {
		_, err := e.svc.CheckMerge(t.Context(), tc.id, tc.source, tc.target)
		checkErr(t, tc.name, err, tc.field, tc.want)
	}
}

func TestMergeCommit(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	res, err := e.svc.MergeBranches(t.Context(), request(s, "clean", s.clean, StrategyMergeCommit))
	if err != nil {
		t.Fatal(err)
	}
	if res.OldTargetSHA != s.m1 || res.NewTargetSHA != e.tip(t, s.id, "main") {
		t.Fatalf("result %+v, main at %s; want old %s and main at the new commit", res, e.tip(t, s.id, "main"), s.m1)
	}
	if got := e.parents(t, s.id, res.NewTargetSHA); !slices.Equal(got, []string{s.m1, s.clean}) {
		t.Errorf("parents = %v, want [main's tip, clean's tip]", got)
	}
	if e.file(t, s.id, res.NewTargetSHA, "n.txt") != "new" || e.file(t, s.id, res.NewTargetSHA, "m.txt") != "main" {
		t.Error("the merge commit lacks a change of one side")
	}
	header := e.bare(t, s.id, "log", "-1", "--format=%an|%ae|%cn|%ce|%at|%ct|%B", res.NewTargetSHA)
	if want := "Deniz Yılmaz|deniz@example.com|Deniz Yılmaz|deniz@example.com|9000|9000|Merge branch 'clean' into 'main'"; header != want {
		t.Errorf("commit header = %q, want %q", header, want)
	}
	wantEvents(t, e, s, []repo.RefUpdate{{Ref: "refs/heads/main", OldSHA: s.m1, NewSHA: res.NewTargetSHA, Forced: repo.ForcedNo}})
}

func TestMergeCommitEvenWhenFastForwardIsPossible(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	res, err := e.svc.MergeBranches(t.Context(), request(s, "ahead", s.ahead, StrategyMergeCommit))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.parents(t, s.id, res.NewTargetSHA); !slices.Equal(got, []string{s.m1, s.ahead}) {
		t.Errorf("parents = %v, want a merge commit of [main, ahead]", got)
	}
}

func TestSquash(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)
	req := request(s, "clean", s.clean, StrategySquash)
	req.Committer = selin

	res, err := e.svc.MergeBranches(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.parents(t, s.id, res.NewTargetSHA); !slices.Equal(got, []string{s.m1}) {
		t.Errorf("parents = %v, want only main's tip", got)
	}
	if e.file(t, s.id, res.NewTargetSHA, "n.txt") != "new" {
		t.Error("the squash commit lacks the source's change")
	}
	header := e.bare(t, s.id, "log", "-1", "--format=%an|%cn|%ce", res.NewTargetSHA)
	if want := "Deniz Yılmaz|Selin|selin@example.com"; header != want {
		t.Errorf("author and committer = %q, want %q", header, want)
	}
	wantEvents(t, e, s, []repo.RefUpdate{{Ref: "refs/heads/main", OldSHA: s.m1, NewSHA: res.NewTargetSHA, Forced: repo.ForcedNo}})
}

func TestFastForward(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)
	req := request(s, "ahead", s.ahead, StrategyFastForward)
	req.Message, req.Author = "", nil // ignored for a fast-forward

	res, err := e.svc.MergeBranches(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.OldTargetSHA != s.m1 || res.NewTargetSHA != s.ahead || e.tip(t, s.id, "main") != s.ahead {
		t.Errorf("result %+v; want main moved from %s to %s", res, s.m1, s.ahead)
	}
	wantEvents(t, e, s, []repo.RefUpdate{{Ref: "refs/heads/main", OldSHA: s.m1, NewSHA: s.ahead, Forced: repo.ForcedNo}})
}

func TestRefusals(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	for _, tc := range []struct {
		name      string
		req       Request
		reason    Reason
		sourceSHA string
		conflicts []string
	}{
		{"fast-forward impossible", request(s, "clean", s.clean, StrategyFastForward), ReasonFastForwardImpossible, s.clean, nil},
		{"conflicts", request(s, "conflict", s.conflict, StrategyMergeCommit), ReasonConflicts, s.conflict, []string{"f.txt"}},
		{"unrelated", request(s, "other", s.other, StrategyMergeCommit), ReasonUnrelated, s.other, nil},
		{"already merged by ancestry", request(s, "old", s.c1, StrategyMergeCommit), ReasonAlreadyMerged, s.c1, nil},
		{"already merged by content", request(s, "same", s.same, StrategySquash), ReasonAlreadyMerged, s.same, nil},
		{"source moved", request(s, "clean", s.c1, StrategyMergeCommit), ReasonSourceMoved, s.clean, nil},
	} {
		_, err := e.svc.MergeBranches(t.Context(), tc.req)
		var r *Refusal
		if !errors.As(err, &r) {
			t.Errorf("%s: error = %v, want a refusal", tc.name, err)
			continue
		}
		if r.Reason != tc.reason || r.SourceSHA != tc.sourceSHA || r.TargetSHA != s.m1 || !slices.Equal(r.Conflicts, tc.conflicts) {
			t.Errorf("%s: refusal %+v, want reason %v, source %s, target %s, conflicts %v", tc.name, r, tc.reason, tc.sourceSHA, s.m1, tc.conflicts)
		}
	}
	if e.tip(t, s.id, "main") != s.m1 || len(e.repos.events()) != 0 {
		t.Error("a refused merge changed main or wrote an event")
	}
}

// A retry of a merge that succeeded, because the caller never got the
// answer, must not merge again.
func TestRetryAfterSuccess(t *testing.T) {
	for _, strategy := range []Strategy{StrategyMergeCommit, StrategySquash, StrategyFastForward} {
		e := newEnv(t)
		s := newScenario(t, e)
		source, sha := "clean", s.clean
		if strategy == StrategyFastForward {
			source, sha = "ahead", s.ahead
		}
		req := request(s, source, sha, strategy)
		if _, err := e.svc.MergeBranches(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		tip := e.tip(t, s.id, "main")

		_, err := e.svc.MergeBranches(t.Context(), req)
		var r *Refusal
		if !errors.As(err, &r) || r.Reason != ReasonAlreadyMerged {
			t.Errorf("strategy %d: retry error = %v, want ReasonAlreadyMerged", strategy, err)
		}
		if e.tip(t, s.id, "main") != tip || len(e.repos.events()) != 1 {
			t.Errorf("strategy %d: the retry moved main or wrote a second event", strategy)
		}
	}
}

// UC-18 E3: the target moves between CheckMerge and MergeBranches. The merge
// is computed again against the new tip.
func TestTargetMovedSinceCheck(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)
	check, err := e.svc.CheckMerge(t.Context(), s.id, "clean", "main")
	if err != nil {
		t.Fatal(err)
	}
	s.w.git(t, 0, "", "switch", "--quiet", "main")
	moved := s.w.commit(t, 7000, "pushed meanwhile", map[string]string{"x.txt": "x\n"})
	s.w.push(t, "main:refs/heads/main")

	res, err := e.svc.MergeBranches(t.Context(), request(s, "clean", check.SourceSHA, StrategyMergeCommit))
	if err != nil {
		t.Fatal(err)
	}
	if res.OldTargetSHA != moved || !slices.Equal(e.parents(t, s.id, res.NewTargetSHA), []string{moved, s.clean}) {
		t.Errorf("result %+v; want a merge onto the new tip %s, not the checked %s", res, moved, check.TargetSHA)
	}
	if e.file(t, s.id, res.NewTargetSHA, "x.txt") != "x" {
		t.Error("the commit pushed meanwhile was lost")
	}
}

// Two merges into one branch at the same time: the second one to update it
// finds the branch moved, merges again and lands on top. Nothing is lost.
func TestConcurrentMerges(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, src := range [][2]string{{"clean", s.clean}, {"clean2", s.clean2}} {
		wg.Go(func() {
			_, errs[i] = e.svc.MergeBranches(context.Background(), request(s, src[0], src[1], StrategyMergeCommit))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("merge %d: %v", i, err)
		}
	}
	tip := e.tip(t, s.id, "main")
	for _, sha := range []string{s.clean, s.clean2, s.m1} {
		if _, err := e.client.Run(t.Context(), e.layout.RepoPath(s.id), "merge-base", "--is-ancestor", sha, tip); err != nil {
			t.Errorf("main lost %s", sha)
		}
	}
	events := e.repos.events()
	if len(events) != 2 {
		t.Fatalf("%d events, want 2", len(events))
	}
	first, second := events[0].RefUpdates[0], events[1].RefUpdates[0]
	if first.OldSHA != s.m1 || second.OldSHA != first.NewSHA || second.NewSHA != tip {
		t.Errorf("events %+v then %+v do not follow each other from %s to %s", first, second, s.m1, tip)
	}
}

func TestUpdateRefOnlyFromOld(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)

	moved, err := e.svc.updateRef(t.Context(), e.layout.RepoPath(s.id), "main", s.clean, s.c1)
	if err != nil || moved {
		t.Errorf("updateRef from a stale tip = %v, %v; want not moved, no error", moved, err)
	}
	if e.tip(t, s.id, "main") != s.m1 {
		t.Error("main moved")
	}
}

// The merge happened, so a lost event is logged, not returned.
func TestMergeWhenTheEventIsLost(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)
	e.repos.recordErr = errors.New("database down")

	res, err := e.svc.MergeBranches(t.Context(), request(s, "clean", s.clean, StrategyMergeCommit))
	if err != nil {
		t.Fatalf("MergeBranches: %v", err)
	}
	if e.tip(t, s.id, "main") != res.NewTargetSHA {
		t.Error("main did not move")
	}
}

func TestMergeBranchesErrors(t *testing.T) {
	e := newEnv(t)
	s := newScenario(t, e)
	with := func(change func(*Request)) Request {
		r := request(s, "clean", s.clean, StrategyMergeCommit)
		change(&r)
		return r
	}

	for _, tc := range []struct {
		name  string
		req   Request
		field string
		want  error
	}{
		{"no strategy", with(func(r *Request) { r.Strategy = 0 }), "strategy", nil},
		{"short SHA", with(func(r *Request) { r.ExpectedSourceSHA = s.clean[:7] }), "expected_source_sha", nil},
		{"no message", with(func(r *Request) { r.Message = " \n" }), "message", nil},
		{"message with NUL", with(func(r *Request) { r.Message = "a\x00b" }), "message", nil},
		{"message too long", with(func(r *Request) { r.Message = strings.Repeat("x", maxMessage+1) }), "message", nil},
		{"no author", with(func(r *Request) { r.Author = nil }), "author", nil},
		{"author without email", with(func(r *Request) { r.Author = &Identity{Name: "Deniz"} }), "author.email", nil},
		{"author with brackets", with(func(r *Request) { r.Author = &Identity{Name: "D <x>", Email: "d@example.com"} }), "author.name", nil},
		{"committer with a line break", with(func(r *Request) { r.Committer = &Identity{Name: "S", Email: "s@example.com\nx"} }), "committer.email", nil},
		{"same branches", with(func(r *Request) { r.SourceBranch = "main" }), "target_branch", nil},
		{"unknown source", with(func(r *Request) { r.SourceBranch = "nope" }), "", ErrBranchNotFound},
		{"unknown target", with(func(r *Request) { r.TargetBranch = "nope" }), "", ErrBranchNotFound},
		{"unknown repository", with(func(r *Request) { r.RepositoryID = uuid.New() }), "", repo.ErrNotFound},
	} {
		_, err := e.svc.MergeBranches(t.Context(), tc.req)
		checkErr(t, tc.name, err, tc.field, tc.want)
	}
	if e.tip(t, s.id, "main") != s.m1 || len(e.repos.events()) != 0 {
		t.Error("a failed request changed main or wrote an event")
	}
}

// checkErr wants a *repo.ValidationError for field, or else the error want.
func checkErr(t *testing.T, name string, err error, field string, want error) {
	t.Helper()
	if field != "" {
		var v *repo.ValidationError
		if !errors.As(err, &v) || v.Field != field {
			t.Errorf("%s: error = %v, want a validation error for %s", name, err, field)
		}
		return
	}
	if !errors.Is(err, want) {
		t.Errorf("%s: error = %v, want %v", name, err, want)
	}
}

// wantEvents checks that exactly one event was written, by merger, with
// updates.
func wantEvents(t *testing.T, e *env, s scenario, updates []repo.RefUpdate) {
	t.Helper()
	events := e.repos.events()
	if len(events) != 1 {
		t.Fatalf("%d events, want 1", len(events))
	}
	if events[0].RepositoryID != s.id || events[0].PusherID != merger || !slices.Equal(events[0].RefUpdates, updates) {
		t.Errorf("event = %+v, want repository %s, pusher %s, updates %+v", events[0], s.id, merger, updates)
	}
}
