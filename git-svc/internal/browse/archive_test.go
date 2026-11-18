package browse

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

func TestArchive(t *testing.T) {
	e := newEnv(t)
	id := e.newRepo(t)
	w := e.newWork(t, id)
	// Attributes that git archive applies by default, and that a CI
	// checkout must not see applied.
	c1 := w.commit(t, 1000, "files", map[string]string{
		".gitattributes": "/tests export-ignore\nversion.txt export-subst\n*.bat text eol=crlf\n",
		"tests/unit.txt": "test\n",
		"version.txt":    "$Format:%H$\n",
		"run.bat":        "echo a\necho b\n",
		"bin/tool.sh":    "#!/bin/sh\n",
		"a[1].txt":       "brackets\n",
	})
	w.git(t, 0, "", "update-index", "--chmod=+x", "bin/tool.sh")
	w.addSpecial(t, "120000", "link", "../outside")
	w.addSpecial(t, "160000", "vendor/lib", c1)
	c2 := w.commit(t, 2000, "modes, a symlink and a submodule", nil)
	w.push(t, "HEAD:refs/heads/main")

	a, err := e.svc.OpenArchive(t.Context(), id, "")
	if err != nil || a.CommitSHA != c2 || a.Ref != "main" {
		t.Fatalf("OpenArchive() = %+v, %v; want commit %s on main", a, err, c2)
	}
	var buf bytes.Buffer
	if err := e.svc.WriteArchive(t.Context(), a, &buf); err != nil {
		t.Fatal(err)
	}
	entries := readTar(t, &buf)

	// git archive names the commit in a pax global header first.
	if len(entries) == 0 || entries[0].hdr.Typeflag != tar.TypeXGlobalHeader || entries[0].hdr.PAXRecords["comment"] != c2 {
		t.Fatalf("first entry = %+v, want a global header naming %s", entries[0].hdr, c2)
	}
	entries = entries[1:]

	var names []string
	byName := map[string]tarEntry{}
	for _, en := range entries {
		names = append(names, en.hdr.Name)
		byName[en.hdr.Name] = en
		if !en.hdr.ModTime.Equal(time.Unix(2000, 0)) {
			t.Errorf("%s: time %v, want the commit's", en.hdr.Name, en.hdr.ModTime)
		}
	}
	want := []string{
		".gitattributes", "a[1].txt", "bin/", "bin/tool.sh", "link", "run.bat",
		"tests/", "tests/unit.txt", "vendor/", "vendor/lib/", "version.txt",
	}
	if !slices.Equal(names, want) {
		t.Errorf("entries = %q, want %q (no prefix, export-ignore not applied)", names, want)
	}

	// Content exactly as committed.
	for name, content := range map[string]string{
		"version.txt":    "$Format:%H$\n",    // no export-subst
		"run.bat":        "echo a\necho b\n", // no eol=crlf
		"tests/unit.txt": "test\n",
	} {
		if got := byName[name].content; got != content {
			t.Errorf("%s = %q, want %q as committed", name, got, content)
		}
	}

	for _, tt := range []struct {
		name string
		typ  byte
		mode int64
	}{
		{"run.bat", tar.TypeReg, 0o644},
		{"bin/tool.sh", tar.TypeReg, 0o755},
		{"bin/", tar.TypeDir, 0o755},
		{"vendor/lib/", tar.TypeDir, 0o755}, // a submodule: an empty directory
	} {
		if h := byName[tt.name].hdr; h == nil || h.Typeflag != tt.typ || h.Mode != tt.mode {
			t.Errorf("%s = %+v, want type %q and mode %o", tt.name, h, tt.typ, tt.mode)
		}
	}
	if h := byName["link"].hdr; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "../outside" {
		t.Errorf("link = %+v, want a symlink to ../outside", h)
	}

	// A writer that fails (a client that went away) stops git.
	errGone := errors.New("client gone")
	if err := e.svc.WriteArchive(t.Context(), a, failAfter{n: 1, err: errGone}); !errors.Is(err, errGone) {
		t.Errorf("WriteArchive() to a failing writer = %v, want %v", err, errGone)
	}
}

func TestOpenArchiveRefs(t *testing.T) {
	e := newEnv(t)
	s := newSample(t, e)
	s.w.push(t, s.c1+":refs/heads/old", s.c2+":refs/tags/v2")

	for _, tt := range []struct{ ref, wantRef, want string }{
		{"", "main", s.c3},
		{"main", "main", s.c3},
		{"refs/heads/main", "refs/heads/main", s.c3},
		{"old", "old", s.c1},
		{"v2", "v2", s.c2},
		{"refs/tags/v2", "refs/tags/v2", s.c2},
		{s.c1, s.c1, s.c1},
	} {
		a, err := e.svc.OpenArchive(t.Context(), s.id, tt.ref)
		if err != nil || a.CommitSHA != tt.want || a.Ref != tt.wantRef {
			t.Errorf("OpenArchive(%q) = %+v, %v; want commit %s, ref %q", tt.ref, a, err, tt.want, tt.wantRef)
		}
	}

	// The archive is of the resolved commit, not of the default branch.
	a, err := e.svc.OpenArchive(t.Context(), s.id, "old")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := e.svc.WriteArchive(t.Context(), a, &buf); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, en := range readTar(t, &buf) {
		files[en.hdr.Name] = en.content
	}
	_, renamed := files["docs/manual.md"] // c2 renamed docs/guide.md
	if files["README.md"] != "hello\n" || files["bin.dat"] != "\x00\x01binary" || files["docs/guide.md"] == "" || renamed {
		t.Errorf("archive of old has %d entries, want c1's files", len(files))
	}
}

func TestOpenArchiveErrors(t *testing.T) {
	e := newEnv(t)

	_, err := e.svc.OpenArchive(t.Context(), uuid.New(), "")
	wantErr(t, "OpenArchive(unknown repository)", err, repo.ErrNotFound)

	// A repository without commits has nothing to archive, whatever the ref.
	empty := e.newRepo(t)
	_, err = e.svc.OpenArchive(t.Context(), empty, "")
	wantErr(t, "OpenArchive(empty repository)", err, ErrRefNotFound)

	s := newSample(t, e)
	_, err = e.svc.OpenArchive(t.Context(), s.id, "nope")
	wantErr(t, "OpenArchive(nope)", err, ErrRefNotFound)
	_, err = e.svc.OpenArchive(t.Context(), s.id, strings.Repeat("0", 40))
	wantErr(t, "OpenArchive(unknown sha)", err, ErrRefNotFound)

	for _, ref := range []string{"-x", "a..b", "HEAD~1", "main:README.md", "refs/heads/-x"} {
		_, err := e.svc.OpenArchive(t.Context(), s.id, ref)
		assertInvalid(t, "OpenArchive("+ref+")", err)
	}
}

// tarEntry is one entry of a tar archive, with its content.
type tarEntry struct {
	hdr     *tar.Header
	content string
}

// readTar reads a whole tar archive.
func readTar(t *testing.T, r io.Reader) []tarEntry {
	t.Helper()
	tr := tar.NewReader(r)
	var entries []tarEntry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		entries = append(entries, tarEntry{hdr: hdr, content: string(content)})
	}
}
