package browse

import (
	"fmt"
	"strings"
	"testing"
)

func TestListCommits(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)

	page, err := e.svc.ListCommits(t.Context(), s.id, "", "", 0, "")
	if err != nil || shas(page.Commits) != join([]string{s.c3, s.c2, s.c1}) || page.NextPageToken != "" {
		t.Errorf("ListCommits() = %q (next %q), %v; want c3 c2 c1", shas(page.Commits), page.NextPageToken, err)
	}

	// Page by page, while the branch moves: the pages keep following c3.
	first, err := e.svc.ListCommits(t.Context(), s.id, "", "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	s.w.commit(t, 4000, "fourth", nil)
	s.w.push(t, "HEAD:refs/heads/main")
	got := shas(first.Commits)
	for token := first.NextPageToken; token != ""; {
		page, err := e.svc.ListCommits(t.Context(), s.id, "", "", 1, token)
		if err != nil {
			t.Fatal(err)
		}
		got += " " + shas(page.Commits)
		token = page.NextPageToken
	}
	if want := join([]string{s.c3, s.c2, s.c1}); got != want {
		t.Errorf("paged commits = %q, want %q", got, want)
	}

	// Only the commits that changed a path; brackets are literal.
	for _, tt := range []struct{ path, want string }{
		{"docs", join([]string{s.c2, s.c1})},
		{"a[1].txt", s.c1},
		{"a1.txt", join([]string{s.c3, s.c1})},
		{"nope", ""},
	} {
		page, err := e.svc.ListCommits(t.Context(), s.id, s.c3, tt.path, 0, "")
		if err != nil || shas(page.Commits) != tt.want {
			t.Errorf("ListCommits(path %q) = %q, %v; want %q", tt.path, shas(page.Commits), err, tt.want)
		}
	}

	// A token is valid only for its repository and path, and must name a
	// commit of the repository.
	_, err = e.svc.ListCommits(t.Context(), s.id, "", "docs", 1, first.NextPageToken)
	assertInvalid(t, "ListCommits(token of another path)", err)
	forged := encodeToken(commitCursor{Repo: s.id, Commit: strings.Repeat("a", 40), Skip: 1})
	_, err = e.svc.ListCommits(t.Context(), s.id, "", "", 1, forged)
	assertInvalid(t, "ListCommits(forged token)", err)
	forged = encodeToken(commitCursor{Repo: s.id, Commit: "--output=x", Skip: 1})
	_, err = e.svc.ListCommits(t.Context(), s.id, "", "", 1, forged)
	assertInvalid(t, "ListCommits(token with an option)", err)
	_, err = e.svc.ListCommits(t.Context(), s.id, "nope", "", 0, "")
	wantErr(t, "ListCommits(unknown ref)", err, ErrRefNotFound)
}

func TestGetCommit(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)

	root, err := e.svc.GetCommit(t.Context(), s.id, s.c1)
	if err != nil {
		t.Fatal(err)
	}
	if root.SHA != s.c1 || root.Stats != (DiffStats{FilesChanged: 6, Additions: 24, Deletions: 0}) || root.Truncated {
		t.Errorf("root commit = %s, stats %+v", root.SHA, root.Stats)
	}
	for _, f := range root.Files {
		if f.Status != FileAdded || f.OldPath != "" || f.NewPath == "" {
			t.Errorf("root commit file %+v, want added", f)
		}
	}

	c2, err := e.svc.GetCommit(t.Context(), s.id, s.c2[:7])
	if err != nil {
		t.Fatal(err)
	}
	if c2.SHA != s.c2 || len(c2.ParentSHAs) != 1 || c2.ParentSHAs[0] != s.c1 {
		t.Errorf("GetCommit(short) = %s with parents %v", c2.SHA, c2.ParentSHAs)
	}
	files := map[string]FileDiff{}
	for _, f := range c2.Files {
		files[f.OldPath+">"+f.NewPath] = f
	}
	readme := files["README.md>README.md"]
	if readme.Status != FileModified || readme.Additions != 1 || readme.Deletions != 0 ||
		string(readme.Patch) != "@@ -1 +1,2 @@\n hello\n+world\n" {
		t.Errorf("README.md = %+v, patch %q", readme, readme.Patch)
	}
	manual := files["docs/guide.md>docs/manual.md"]
	if manual.Status != FileRenamed || manual.Additions != 1 || !strings.HasPrefix(string(manual.Patch), "@@ ") {
		t.Errorf("docs/manual.md = %+v, patch %q; want a rename with one added line", manual, manual.Patch)
	}
	bin := files["bin.dat>"]
	if bin.Status != FileDeleted || !bin.Binary || bin.Patch != nil || bin.Truncated {
		t.Errorf("bin.dat = %+v, want a deleted binary file without patch", bin)
	}
	if c2.Stats.FilesChanged != 3 || len(c2.Files) != 3 {
		t.Errorf("stats %+v with %d files, want 3", c2.Stats, len(c2.Files))
	}

	// A submodule and a symlink were added in c3.
	c3, err := e.svc.GetCommit(t.Context(), s.id, s.c3)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c3.Files); got != 4 {
		t.Errorf("c3 changed %d files, want 4: %+v", got, c3.Files)
	}

	_, err = e.svc.GetCommit(t.Context(), s.id, strings.Repeat("0", 40))
	wantErr(t, "GetCommit(unknown)", err, ErrCommitNotFound)
	for _, bad := range []string{"", "abc", strings.Repeat("A", 40), strings.Repeat("a", 41), "--all", "main"} {
		_, err := e.svc.GetCommit(t.Context(), s.id, bad)
		assertInvalid(t, fmt.Sprintf("GetCommit(%q)", bad), err)
	}
	// A blob's SHA is not a commit.
	blob := s.w.git(t, 0, "", "rev-parse", s.c1+":README.md")
	_, err = e.svc.GetCommit(t.Context(), s.id, blob)
	wantErr(t, "GetCommit(blob)", err, ErrCommitNotFound)
}

func TestGetCommitMergeAndTypeChange(t *testing.T) {
	e := newEnv(t)
	id := e.newRepo(t)
	w := e.newWork(t, id)
	base := w.commit(t, 1000, "base", map[string]string{"a.txt": "a\n", "kind": "a file\n"})
	w.git(t, 0, "", "switch", "--quiet", "-c", "feature")
	w.commit(t, 2000, "on feature", map[string]string{"f.txt": "f\n"})
	w.git(t, 0, "", "switch", "--quiet", "main")
	w.commit(t, 3000, "on main", map[string]string{"m.txt": "m\n"})
	w.git(t, 4000, "", "merge", "--quiet", "--no-ff", "-m", "merge feature", "feature")
	merge := w.git(t, 0, "", "rev-parse", "HEAD")
	// "kind" turns from a file into a symlink: git prints two patches.
	w.git(t, 0, "", "rm", "--quiet", "--cached", "kind")
	w.addSpecial(t, "120000", "kind", "a.txt")
	typeChange := w.commit(t, 5000, "file to symlink", map[string]string{"a.txt": "a\nb\n"})
	w.push(t, "HEAD:refs/heads/main")

	m, err := e.svc.GetCommit(t.Context(), id, merge)
	if err != nil {
		t.Fatal(err)
	}
	// Against the first parent (main), the merge brings f.txt.
	if len(m.ParentSHAs) != 2 || len(m.Files) != 1 || m.Files[0].NewPath != "f.txt" || m.Files[0].Status != FileAdded {
		t.Errorf("merge = parents %v, files %+v; want f.txt added", m.ParentSHAs, m.Files)
	}
	if m.ParentSHAs[0] == base {
		t.Error("the merge's first parent should be main's commit, not base")
	}

	tc, err := e.svc.GetCommit(t.Context(), id, typeChange)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Files) != 2 {
		t.Fatalf("type change commit files = %+v, want a.txt and kind", tc.Files)
	}
	a, kind := tc.Files[0], tc.Files[1]
	if a.NewPath != "a.txt" || string(a.Patch) != "@@ -1 +1,2 @@\n a\n+b\n" {
		t.Errorf("a.txt = %+v, patch %q", a, a.Patch)
	}
	if kind.NewPath != "kind" || kind.Status != FileModified ||
		!strings.Contains(string(kind.Patch), "-a file\n") || !strings.Contains(string(kind.Patch), "+a.txt") {
		t.Errorf("kind = %+v, patch %q; want both halves of the type change", kind, kind.Patch)
	}
}

func TestGetCommitLimits(t *testing.T) {
	e := newEnv(t)
	id := e.newRepo(t)
	w := e.newWork(t, id)

	// More files than the limit.
	many := map[string]string{}
	for i := range maxDiffFiles + 1 {
		many[fmt.Sprintf("many/f%03d.txt", i)] = "x\n"
	}
	wide := w.commit(t, 1000, "many files", many)

	// One patch over the per-file limit, then enough files to pass the
	// total limit.
	line := strings.Repeat("y", 99) + "\n"
	files := map[string]string{"huge.txt": strings.Repeat(line, maxFilePatch/len(line)+10)}
	perFile := (maxFilePatch - 1024) / len(line) // lines that fit in one patch
	for i := range maxTotalPatch/(perFile*len(line)) + 3 {
		files[fmt.Sprintf("part/p%02d.txt", i)] = strings.Repeat(line, perFile)
	}
	big := w.commit(t, 2000, "big patches", files)
	w.push(t, "HEAD:refs/heads/main")

	d, err := e.svc.GetCommit(t.Context(), id, wide)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || len(d.Files) != maxDiffFiles || d.Stats.FilesChanged != maxDiffFiles+1 || d.Stats.Additions != maxDiffFiles+1 {
		t.Errorf("wide commit: truncated %v, %d files, stats %+v", d.Truncated, len(d.Files), d.Stats)
	}

	d, err = e.svc.GetCommit(t.Context(), id, big)
	if err != nil {
		t.Fatal(err)
	}
	total, cut, kept := 0, 0, 0
	for _, f := range d.Files {
		total += len(f.Patch)
		switch {
		case f.Truncated:
			cut++
			if f.Patch != nil {
				t.Errorf("%s is truncated but has a patch", f.NewPath)
			}
		default:
			kept++
		}
		if f.NewPath == "huge.txt" && !f.Truncated {
			t.Error("huge.txt is over the per-file limit but not truncated")
		}
	}
	if total > maxTotalPatch || cut < 2 || kept == 0 || d.Truncated {
		t.Errorf("big commit: %d patch bytes, %d cut, %d kept, truncated %v", total, cut, kept, d.Truncated)
	}
}

func shas(cs []Commit) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.SHA)
	}
	return join(out)
}
