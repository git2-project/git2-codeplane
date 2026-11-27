package browse

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// branched is a repository where main and feature went separate ways:
//
//	c1 (t=1000)            base.txt, shared.txt         main, then feature
//	f1 (t=2000, feature)   adds feature.txt
//	f2 (t=3000, feature)   adds a line to shared.txt
//	m2 (t=4000, main)      adds main.txt
type branched struct {
	id             uuid.UUID
	w              *work
	c1, f1, f2, m2 string
}

func newBranched(t *testing.T, e *env) branched {
	t.Helper()
	b := branched{id: e.newRepo(t)}
	b.w = e.newWork(t, b.id)
	b.c1 = b.w.commit(t, 1000, "base", map[string]string{"base.txt": "base\n", "shared.txt": "one\n"})
	b.w.git(t, 0, "", "switch", "--quiet", "-c", "feature")
	b.f1 = b.w.commit(t, 2000, "add feature", map[string]string{"feature.txt": "feature\n"})
	b.f2 = b.w.commit(t, 3000, "change shared", map[string]string{"shared.txt": "one\ntwo\n"})
	b.w.git(t, 0, "", "switch", "--quiet", "main")
	b.m2 = b.w.commit(t, 4000, "add main", map[string]string{"main.txt": "main\n"})
	b.w.push(t, "main:refs/heads/main", "feature:refs/heads/feature")
	return b
}

func TestCompareRefs(t *testing.T) {
	e := newEnv(t)
	b := newBranched(t, e)

	c, err := e.svc.CompareRefs(t.Context(), b.id, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseSHA != b.m2 || c.HeadSHA != b.f2 || c.MergeBaseSHA != b.c1 {
		t.Errorf("SHAs = %s %s %s, want main %s, feature %s, merge base %s", c.BaseSHA, c.HeadSHA, c.MergeBaseSHA, b.m2, b.f2, b.c1)
	}
	if c.AheadBy != 2 || c.BehindBy != 1 {
		t.Errorf("ahead %d, behind %d; want 2 and 1", c.AheadBy, c.BehindBy)
	}
	if got := commitSHAs(c.Commits); !slices.Equal(got, []string{b.f2, b.f1}) || c.CommitsTruncated {
		t.Errorf("commits = %v (truncated %v), want [f2 f1]", got, c.CommitsTruncated)
	}
	// Only the feature's changes: main.txt came to main after the fork.
	if got := filePaths(c.Files); !slices.Equal(got, []string{"feature.txt", "shared.txt"}) || c.FilesTruncated {
		t.Errorf("files = %v (truncated %v), want [feature.txt shared.txt]", got, c.FilesTruncated)
	}
	if want := (DiffStats{FilesChanged: 2, Additions: 2}); c.Stats != want {
		t.Errorf("stats = %+v, want %+v", c.Stats, want)
	}
	if got, want := string(c.Files[1].Patch), "@@ -1 +1,2 @@\n one\n+two\n"; got != want {
		t.Errorf("patch of shared.txt = %q, want %q", got, want)
	}

	// The other way round, main is what is new.
	c, err = e.svc.CompareRefs(t.Context(), b.id, "feature", "main")
	if err != nil {
		t.Fatal(err)
	}
	if c.AheadBy != 1 || c.BehindBy != 2 || !slices.Equal(commitSHAs(c.Commits), []string{b.m2}) ||
		!slices.Equal(filePaths(c.Files), []string{"main.txt"}) {
		t.Errorf("feature...main: ahead %d, behind %d, commits %v, files %v", c.AheadBy, c.BehindBy, commitSHAs(c.Commits), filePaths(c.Files))
	}
}

func TestCompareRefsForms(t *testing.T) {
	e := newEnv(t)
	b := newBranched(t, e)

	byName, err := e.svc.CompareRefs(t.Context(), b.id, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{"refs/heads/main", "refs/heads/feature"},
		{b.m2, b.f2},
	} {
		c, err := e.svc.CompareRefs(t.Context(), b.id, pair[0], pair[1])
		if err != nil {
			t.Fatalf("CompareRefs(%q, %q): %v", pair[0], pair[1], err)
		}
		if c.BaseSHA != byName.BaseSHA || c.HeadSHA != byName.HeadSHA || c.AheadBy != byName.AheadBy ||
			!slices.Equal(filePaths(c.Files), filePaths(byName.Files)) {
			t.Errorf("CompareRefs(%q, %q) differs from the comparison by branch name", pair[0], pair[1])
		}
	}
}

func TestCompareRefsNothingToMerge(t *testing.T) {
	e := newEnv(t)
	b := newBranched(t, e)

	for _, head := range []string{b.c1, "main"} {
		c, err := e.svc.CompareRefs(t.Context(), b.id, "main", head)
		if err != nil {
			t.Fatalf("CompareRefs(main, %s): %v", head, err)
		}
		if c.AheadBy != 0 || len(c.Commits) != 0 || len(c.Files) != 0 || c.Stats != (DiffStats{}) {
			t.Errorf("CompareRefs(main, %s) = ahead %d, %d commits, %d files; want nothing", head, c.AheadBy, len(c.Commits), len(c.Files))
		}
		if c.MergeBaseSHA != c.HeadSHA {
			t.Errorf("CompareRefs(main, %s): merge base %s, want head %s", head, c.MergeBaseSHA, c.HeadSHA)
		}
	}
}

func TestCompareRefsUnrelated(t *testing.T) {
	e := newEnv(t)
	b := newBranched(t, e)
	tree := b.w.mktree(t, map[string]string{"other.txt": "other\n"})
	root := b.w.git(t, 5000, "", "commit-tree", tree, "-m", "unrelated root")
	b.w.push(t, root+":refs/heads/other")

	_, err := e.svc.CompareRefs(t.Context(), b.id, "main", "other")
	wantErr(t, "CompareRefs(main, other)", err, ErrNoMergeBase)
}

func TestCompareRefsCommitLimit(t *testing.T) {
	e := newEnv(t)
	b := newBranched(t, e)
	old := compareCommitLimit
	compareCommitLimit = 1
	t.Cleanup(func() { compareCommitLimit = old })

	c, err := e.svc.CompareRefs(t.Context(), b.id, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(commitSHAs(c.Commits), []string{b.f2}) || !c.CommitsTruncated || c.AheadBy != 2 {
		t.Errorf("commits %v, truncated %v, ahead %d; want the newest one, truncated, ahead 2", commitSHAs(c.Commits), c.CommitsTruncated, c.AheadBy)
	}
}

func TestCompareRefsErrors(t *testing.T) {
	e := newEnv(t)
	b := newBranched(t, e)
	empty := e.newRepo(t)

	for _, tc := range []struct {
		name       string
		id         uuid.UUID
		base, head string
		field      string // a *repo.ValidationError for this field
		want       error  // otherwise this error
	}{
		{name: "no base", id: b.id, head: "feature", field: "base"},
		{name: "no head", id: b.id, base: "main", field: "head"},
		{name: "bad head", id: b.id, base: "main", head: "a..b", field: "head"},
		{name: "bad base before the repository is read", id: uuid.New(), base: "-x", head: "main", field: "base"},
		{name: "unknown ref", id: b.id, base: "main", head: "nope", want: ErrRefNotFound},
		{name: "unknown repository", id: uuid.New(), base: "main", head: "feature", want: repo.ErrNotFound},
		{name: "repository without commits", id: empty, base: "main", head: "main", want: ErrRefNotFound},
	} {
		_, err := e.svc.CompareRefs(t.Context(), tc.id, tc.base, tc.head)
		if tc.field != "" {
			var v *repo.ValidationError
			if !errors.As(err, &v) || v.Field != tc.field {
				t.Errorf("%s: error = %v, want a validation error for %s", tc.name, err, tc.field)
			}
			continue
		}
		wantErr(t, tc.name, err, tc.want)
	}
}

func commitSHAs(commits []Commit) []string {
	var shas []string
	for _, c := range commits {
		shas = append(shas, c.SHA)
	}
	return shas
}

func filePaths(files []FileDiff) []string {
	var paths []string
	for _, f := range files {
		p := f.NewPath
		if p == "" {
			p = f.OldPath
		}
		paths = append(paths, p)
	}
	return paths
}
