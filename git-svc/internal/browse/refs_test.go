package browse

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestListBranches(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)
	for _, b := range []string{"zeta", "feature", "Fix-UI"} {
		s.w.git(t, 0, "", "branch", b, s.c1)
	}
	s.w.push(t, "refs/heads/zeta", "refs/heads/feature", "refs/heads/Fix-UI")

	page, err := e.svc.ListBranches(t.Context(), s.id, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// The default branch first, then by name in byte order.
	if got := branchNames(page.Branches); got != "main Fix-UI feature zeta" || page.NextPageToken != "" {
		t.Errorf("branches = %q (next %q), want main Fix-UI feature zeta", got, page.NextPageToken)
	}
	main := page.Branches[0]
	if !main.Default || main.Commit.SHA != s.c3 || main.Commit.Title != "third" || page.Branches[1].Default {
		t.Errorf("main = %+v, want the default branch at %s", main, s.c3)
	}
	c := page.Branches[3].Commit // zeta, at c1
	if c.SHA != s.c1 || c.Title != "first" || c.Message != "first\n\nwith a body" || len(c.ParentSHAs) != 0 ||
		c.Author != (Signature{Name: "Ada", Email: "ada@example.com", Time: time.Unix(1000, 0).UTC()}) ||
		c.Committer.Name != "Cy" || c.Committer.Email != "cy@example.com" {
		t.Errorf("zeta's commit = %+v", c)
	}

	if page, err := e.svc.ListBranches(t.Context(), s.id, "FI", 0, ""); err != nil || branchNames(page.Branches) != "Fix-UI" {
		t.Errorf("ListBranches(FI) = %q, %v; want Fix-UI", branchNames(page.Branches), err)
	}

	// One branch per page.
	var all []string
	token := ""
	for range 5 {
		page, err := e.svc.ListBranches(t.Context(), s.id, "", 1, token)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, branchNames(page.Branches))
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if got := join(all); got != "main Fix-UI feature zeta" {
		t.Errorf("paged branches = %q", got)
	}

	_, err = e.svc.ListBranches(t.Context(), uuid.New(), "", 1, token)
	if err == nil {
		t.Error("a token of another repository was accepted")
	}
	for _, bad := range []string{"garbage", encodeToken(refCursor{Repo: e.newRepo(t)})} {
		_, err := e.svc.ListBranches(t.Context(), s.id, "", 1, bad)
		assertInvalid(t, "ListBranches(token "+bad+")", err)
	}
	_, err = e.svc.ListBranches(t.Context(), s.id, "", -1, "")
	assertInvalid(t, "ListBranches(page_size -1)", err)
	_, err = e.svc.ListBranches(t.Context(), s.id, string(make([]byte, 256)), 0, "")
	assertInvalid(t, "ListBranches(long query)", err)
}

func TestListTags(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)
	s.w.git(t, 0, "", "tag", "light-old", s.c1)                               // date of c1: 1000
	s.w.git(t, 5000, "", "tag", "-a", "-m", "Release 1\n\nNotes", "v1", s.c2) // tagged at 5000
	s.w.git(t, 0, "", "tag", "light-new", s.c3)                               // date of c3: 3000
	s.w.git(t, 0, "", "tag", "same-date", s.c3)                               // also 3000: by name
	s.w.push(t, "--tags")

	page, err := e.svc.ListTags(t.Context(), s.id, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := tagNames(page.Tags); got != "v1 light-new same-date light-old" {
		t.Errorf("tags = %q, want newest first", got)
	}
	v1 := page.Tags[0]
	if !v1.Annotated || v1.Message != "Release 1\n\nNotes" || v1.Commit == nil || v1.Commit.SHA != s.c2 {
		t.Errorf("v1 = %+v, want an annotated tag of %s", v1, s.c2)
	}
	if light := page.Tags[3]; light.Annotated || light.Message != "" || light.Commit == nil || light.Commit.SHA != s.c1 {
		t.Errorf("light-old = %+v, want a lightweight tag of %s", light, s.c1)
	}

	var all []string
	token := ""
	for range 5 {
		page, err := e.svc.ListTags(t.Context(), s.id, 3, token)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, tagNames(page.Tags))
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if got := join(all); got != "v1 light-new same-date light-old" {
		t.Errorf("paged tags = %q", got)
	}
}

func TestListTagsThroughOtherTags(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)
	s.w.git(t, 1000, "", "tag", "-a", "-m", "inner", "inner", s.c2)
	s.w.git(t, 2000, "", "tag", "-a", "-m", "outer", "outer", "inner")
	s.w.git(t, 3000, "", "tag", "-a", "-m", "outermost", "outermost", "outer")
	readme := s.c3 + ":README.md"
	s.w.git(t, 0, "", "tag", "file-light", readme)
	s.w.git(t, 4000, "", "tag", "-a", "-m", "file", "file", readme)
	s.w.git(t, 5000, "", "tag", "-a", "-m", "of file", "of-file", "file")
	s.w.push(t, "--tags")

	page, err := e.svc.ListTags(t.Context(), s.id, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tags) != 6 {
		t.Fatalf("tags = %q, want 6", tagNames(page.Tags))
	}
	for _, tag := range page.Tags {
		switch tag.Name {
		case "inner", "outer", "outermost":
			if tag.Commit == nil || tag.Commit.SHA != s.c2 || tag.Message != tag.Name {
				t.Errorf("%s = %+v, want its own message and commit %s", tag.Name, tag, s.c2)
			}
		default:
			if tag.Commit != nil {
				t.Errorf("%s has commit %s, want none: it leads to a file", tag.Name, tag.Commit.SHA)
			}
		}
	}
}

func branchNames(bs []Branch) string {
	var names []string
	for _, b := range bs {
		names = append(names, b.Name)
	}
	return join(names)
}

func tagNames(ts []Tag) string {
	var names []string
	for _, tag := range ts {
		names = append(names, tag.Name)
	}
	return join(names)
}

func join(parts []string) string {
	out := ""
	for _, p := range parts {
		if out != "" {
			out += " "
		}
		out += p
	}
	return out
}
