package browse

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

func TestGetTreeRoot(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)

	tree, err := e.svc.GetTree(t.Context(), s.id, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if tree.Ref != "main" || tree.CommitSHA != s.c3 || tree.Empty || tree.Truncated {
		t.Errorf("GetTree() = ref %q, commit %s, empty %v, truncated %v; want main at %s", tree.Ref, tree.CommitSHA, tree.Empty, tree.Truncated, s.c3)
	}
	var got []string
	for _, en := range tree.Entries {
		got = append(got, en.Path)
	}
	// Directories first, then by name in byte order.
	want := []string{"docs", "src", "vendor", "README.md", "a1.txt", "a[1].txt", "link"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if tree.LastCommit == nil || tree.LastCommit.SHA != s.c3 {
		t.Errorf("last commit of the root = %+v, want %s", tree.LastCommit, s.c3)
	}

	byName := map[string]TreeEntry{}
	for _, en := range tree.Entries {
		byName[en.Name] = en
	}
	if r := byName["README.md"]; r.Type != EntryFile || r.Mode != "100644" || r.Size != int64(len("hello\nworld\n")) || len(r.SHA) != 40 {
		t.Errorf("README.md = %+v", r)
	}
	if l := byName["link"]; l.Type != EntrySymlink || l.Size != int64(len("README.md")) {
		t.Errorf("link = %+v, want a symlink of 9 bytes", l)
	}
	if d := byName["docs"]; d.Type != EntryDirectory || d.Size != -1 {
		t.Errorf("docs = %+v, want a directory without size", d)
	}
}

func TestGetTreeSubdirectory(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)

	tree, err := e.svc.GetTree(t.Context(), s.id, "main", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Entries) != 1 || tree.Entries[0].Path != "docs/manual.md" || tree.Entries[0].Name != "manual.md" {
		t.Errorf("entries of docs = %+v, want docs/manual.md", tree.Entries)
	}
	// docs last changed in c2, not in c3.
	if tree.LastCommit == nil || tree.LastCommit.SHA != s.c2 {
		t.Errorf("last commit of docs = %+v, want %s", tree.LastCommit, s.c2)
	}
	// vendor holds only the submodule.
	vendor, err := e.svc.GetTree(t.Context(), s.id, "", "vendor")
	if err != nil || len(vendor.Entries) != 1 || vendor.Entries[0].Type != EntrySubmodule || vendor.Entries[0].SHA != s.c1 {
		t.Errorf("GetTree(vendor) = %+v, %v; want the submodule at %s", vendor.Entries, err, s.c1)
	}
}

func TestGetTreeRefs(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)
	s.w.git(t, 0, "", "tag", "v1", s.c1)
	// A tag named like a branch: the branch wins.
	s.w.git(t, 0, "", "tag", "dev", s.c1)
	s.w.git(t, 0, "", "branch", "dev", s.c2)
	s.w.push(t, "refs/tags/v1", "refs/tags/dev", "refs/heads/dev")

	for _, tt := range []struct {
		ref, want string
	}{
		{"v1", s.c1},
		{"refs/tags/v1", s.c1},
		{s.c2, s.c2},
		{"dev", s.c2},
		{"refs/heads/dev", s.c2},
		{"refs/tags/dev", s.c1},
		{"refs/heads/main", s.c3},
	} {
		tree, err := e.svc.GetTree(t.Context(), s.id, tt.ref, "")
		if err != nil || tree.CommitSHA != tt.want || tree.Ref != tt.ref {
			t.Errorf("GetTree(%q) = commit %s, ref %q, %v; want %s", tt.ref, tree.CommitSHA, tree.Ref, err, tt.want)
		}
	}
	// In c1, docs still holds guide.md.
	tree, err := e.svc.GetTree(t.Context(), s.id, "v1", "docs")
	if err != nil || len(tree.Entries) != 1 || tree.Entries[0].Name != "guide.md" {
		t.Errorf("GetTree(v1, docs) = %+v, %v; want guide.md", tree.Entries, err)
	}

	for _, ref := range []string{"nope", "refs/heads/v1", strings.Repeat("0", 40)} {
		_, err := e.svc.GetTree(t.Context(), s.id, ref, "")
		wantErr(t, "GetTree("+ref+")", err, ErrRefNotFound)
	}
}

func TestGetTreeErrors(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)

	_, err := e.svc.GetTree(t.Context(), s.id, "", "nope")
	wantErr(t, "GetTree(nope)", err, ErrPathNotFound)
	_, err = e.svc.GetTree(t.Context(), s.id, "", "README.md/x")
	wantErr(t, "GetTree(README.md/x)", err, ErrPathNotFound)

	for _, tt := range []struct{ ref, path string }{
		{"", "README.md"},  // a file
		{"", "vendor/lib"}, // a submodule
		{"", "/docs"},
		{"", "docs/"},
		{"", "docs//x"},
		{"", "../docs"},
		{"", "docs/./x"},
		{"-x", ""},
		{"a..b", ""},
		{"refs/heads/-x", ""},
		{"main:README.md", ""},
		{"HEAD~1", ""},
	} {
		_, err := e.svc.GetTree(t.Context(), s.id, tt.ref, tt.path)
		var invalidErr *repo.ValidationError
		if !errors.As(err, &invalidErr) {
			t.Errorf("GetTree(%q, %q) error = %v, want a validation error", tt.ref, tt.path, err)
		}
	}
}

func TestGetBlob(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)

	b, err := e.svc.GetBlob(t.Context(), s.id, "", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(b.Content) != "hello\nworld\n" || b.Name != "README.md" || b.CommitSHA != s.c3 || b.Ref != "main" ||
		b.Binary || b.LFS || b.Truncated || b.Size != 12 || b.Type != EntryFile {
		t.Errorf("GetBlob(README.md) = %+v", b)
	}
	// The pattern characters are part of the name.
	if b, err := e.svc.GetBlob(t.Context(), s.id, "", "a[1].txt"); err != nil || string(b.Content) != "brackets\n" {
		t.Errorf("GetBlob(a[1].txt) = %q, %v; want the file with brackets", b.Content, err)
	}
	if b, err := e.svc.GetBlob(t.Context(), s.id, s.c1, "bin.dat"); err != nil || !b.Binary || b.Content != nil || b.Size != 8 {
		t.Errorf("GetBlob(bin.dat) = %+v, %v; want binary without content", b, err)
	}
	if b, err := e.svc.GetBlob(t.Context(), s.id, "", "link"); err != nil || b.Type != EntrySymlink || string(b.Content) != "README.md" {
		t.Errorf("GetBlob(link) = %+v, %v; want the symlink's target", b, err)
	}

	_, err = e.svc.GetBlob(t.Context(), s.id, "", "docs")
	assertInvalid(t, "GetBlob(docs)", err)
	_, err = e.svc.GetBlob(t.Context(), s.id, "", "vendor/lib")
	assertInvalid(t, "GetBlob(vendor/lib)", err)
	_, err = e.svc.GetBlob(t.Context(), s.id, "", "")
	assertInvalid(t, "GetBlob(\"\")", err)
	_, err = e.svc.GetBlob(t.Context(), s.id, "", "bin.dat")
	wantErr(t, "GetBlob(deleted file)", err, ErrPathNotFound)
}

func TestGetBlobLargeAndLFS(t *testing.T) {
	e := newEnv(t)
	id := e.newRepo(t)
	w := e.newWork(t, id)
	big := strings.Repeat("0123456789abcde\n", (maxBlobBytes+maxBlobBytes/2)/16) // 1.5 MiB
	lfs := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 12345\n"
	lateNUL := strings.Repeat("x", binaryProbe) + "\x00" // a NUL past the probe: text, as for git
	w.commit(t, 1000, "files", map[string]string{"big.txt": big, "model.bin": lfs, "late.txt": lateNUL})
	w.push(t, "HEAD:refs/heads/main")

	b, err := e.svc.GetBlob(t.Context(), id, "", "big.txt")
	if err != nil || !b.Truncated || len(b.Content) != maxBlobBytes || b.Size != int64(len(big)) || b.Binary {
		t.Errorf("GetBlob(big.txt) = truncated %v, %d of %d bytes, %v; want the first %d bytes", b.Truncated, len(b.Content), b.Size, err, maxBlobBytes)
	}
	if !bytes.Equal(b.Content, []byte(big[:maxBlobBytes])) {
		t.Error("GetBlob(big.txt) content is not the file's start")
	}
	if b, err := e.svc.GetBlob(t.Context(), id, "", "model.bin"); err != nil || !b.LFS || string(b.Content) != lfs {
		t.Errorf("GetBlob(model.bin) = %+v, %v; want an LFS pointer", b, err)
	}
	if b, err := e.svc.GetBlob(t.Context(), id, "", "late.txt"); err != nil || b.Binary {
		t.Errorf("GetBlob(late.txt) binary = %v, %v; want text", b.Binary, err)
	}

	f, err := e.svc.OpenRaw(t.Context(), id, "", "big.txt")
	if err != nil || f.Size != int64(len(big)) || f.Name != "big.txt" {
		t.Fatalf("OpenRaw() = %+v, %v", f, err)
	}
	var raw bytes.Buffer
	if err := e.svc.WriteRaw(t.Context(), f, &raw); err != nil || raw.String() != big {
		t.Errorf("WriteRaw() wrote %d bytes, %v; want the whole file of %d", raw.Len(), err, len(big))
	}
	// A writer that fails (a client that went away) stops git.
	errGone := errors.New("client gone")
	if err := e.svc.WriteRaw(t.Context(), f, failAfter{n: 1000, err: errGone}); !errors.Is(err, errGone) {
		t.Errorf("WriteRaw() to a failing writer = %v, want %v", err, errGone)
	}
}

// failAfter accepts writes until it has seen n bytes, then fails.
type failAfter struct {
	n   int
	err error
}

func (f failAfter) Write(p []byte) (int, error) {
	if len(p) >= f.n {
		return 0, f.err
	}
	return len(p), nil
}

func assertInvalid(t *testing.T, call string, err error) {
	t.Helper()
	var invalidErr *repo.ValidationError
	if !errors.As(err, &invalidErr) {
		t.Errorf("%s: error = %v, want a validation error", call, err)
	}
}
