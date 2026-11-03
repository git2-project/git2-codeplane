package browse

import (
	"fmt"
	"strings"
	"testing"
)

const samplePatch = "diff --git a/a.txt b/a.txt\n" +
	"index 1111111..2222222 100644\n" +
	"--- a/a.txt\n" +
	"+++ b/a.txt\n" +
	"@@ -1 +1,2 @@\n" +
	" a\n" +
	"+b\n" +
	"diff --git a/bin b/bin\n" +
	"deleted file mode 100644\n" +
	"Binary files a/bin and /dev/null differ\n" +
	"diff --git a/c b/c\n" +
	"new file mode 100644\n" +
	"--- /dev/null\n" +
	"+++ b/c\n" +
	"@@ -0,0 +1 @@\n" +
	"+diff --git is only text here\n" +
	"\\ No newline at end of file\n"

func TestPatchSplitter(t *testing.T) {
	// The same output, whole and one byte at a time: line starts may fall
	// anywhere in a write.
	for _, chunk := range []int{len(samplePatch), 1, 3, 7} {
		stopped := false
		p := &patchSplitter{want: 3, stop: func() { stopped = true }, atStart: true}
		for i := 0; i < len(samplePatch); i += chunk {
			_, _ = p.Write([]byte(samplePatch[i:min(i+chunk, len(samplePatch))]))
		}
		p.flush()
		if stopped || len(p.sections) != 3 {
			t.Fatalf("chunk %d: %d sections, stopped %v; want 3, not stopped", chunk, len(p.sections), stopped)
		}
		want := []struct{ header, hunks string }{
			{"diff --git a/a.txt b/a.txt\n", "@@ -1 +1,2 @@\n a\n+b\n"},
			{"diff --git a/bin b/bin\n", ""},
			{"diff --git a/c b/c\n", "@@ -0,0 +1 @@\n+diff --git is only text here\n\\ No newline at end of file\n"},
		}
		for i, w := range want {
			if got := p.sections[i]; string(got.header) != w.header || string(got.hunks) != w.hunks {
				t.Errorf("chunk %d, section %d = header %q, hunks %q; want %q, %q", chunk, i, got.header, got.hunks, w.header, w.hunks)
			}
		}
	}
}

func TestPatchSplitterStopsAtTotal(t *testing.T) {
	// Sections that each fit the per-file limit, until the total is full:
	// the one that overflows is cut, and git is stopped.
	stopped := false
	p := &patchSplitter{want: 100, stop: func() { stopped = true }, atStart: true}
	hunk := "@@ -0,0 +1 @@\n+" + strings.Repeat("z", maxFilePatch-100) + "\n"
	fit := maxTotalPatch / len(hunk)
	for i := 0; i < 100 && !stopped; i++ {
		_, _ = fmt.Fprintf(p, "diff --git a/f%d b/f%d\n%s", i, i, hunk)
	}
	if !stopped || len(p.sections) != fit+1 || !p.sections[fit].truncated || p.sections[fit-1].truncated || p.total > maxTotalPatch {
		t.Errorf("stopped %v after %d sections (want %d, the last cut), total %d", stopped, len(p.sections), fit+1, p.total)
	}
}

func TestPatchSplitterStopsAtWant(t *testing.T) {
	stopped := false
	p := &patchSplitter{want: 1, stop: func() { stopped = true }, atStart: true}
	_, _ = p.Write([]byte(samplePatch))
	if !stopped || len(p.sections) != 1 || string(p.sections[0].hunks) != "@@ -1 +1,2 @@\n a\n+b\n" {
		t.Errorf("stopped %v with %d sections, want stopped after the first", stopped, len(p.sections))
	}
}

func TestPatchSplitterLongLine(t *testing.T) {
	// A single line longer than the per-file limit, written in pieces: the
	// file is cut, memory stays bounded, and the next file still splits.
	p := &patchSplitter{want: 2, stop: func() {}, atStart: true}
	_, _ = p.Write([]byte("diff --git a/min.js b/min.js\n@@ -0,0 +1 @@\n+"))
	piece := []byte(strings.Repeat("x", 4096))
	for range maxFilePatch/len(piece) + 2 {
		_, _ = p.Write(piece)
	}
	_, _ = p.Write([]byte("\ndiff --git a/b b/b\n@@ -1 +1 @@\n-x\n+y\n"))
	if len(p.sections) != 2 || !p.sections[0].truncated || p.sections[0].hunks != nil || string(p.sections[1].hunks) != "@@ -1 +1 @@\n-x\n+y\n" {
		t.Errorf("sections = %d; first truncated %v; second hunks %q", len(p.sections), p.sections[0].truncated, p.sections[1].hunks)
	}
	if p.total != len("@@ -1 +1 @@\n-x\n+y\n") {
		t.Errorf("total = %d, want only the second file's bytes", p.total)
	}
}

func TestParseDiffFiles(t *testing.T) {
	out := ":100644 100644 aaa bbb M\x00x.txt\x00" +
		":100644 100644 ccc ddd R086\x00old.txt\x00new.txt\x00" +
		":100644 000000 eee 000 D\x00bin\x00" +
		"2\t1\tx.txt\x00" +
		"3\t0\t\x00old.txt\x00new.txt\x00" +
		"-\t-\tbin\x00"
	changes, err := parseDiffFiles([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []FileDiff{
		{Status: FileModified, OldPath: "x.txt", NewPath: "x.txt", Additions: 2, Deletions: 1},
		{Status: FileRenamed, OldPath: "old.txt", NewPath: "new.txt", Additions: 3},
		{Status: FileDeleted, OldPath: "bin", Binary: true},
	}
	if len(changes) != len(want) {
		t.Fatalf("%d changes, want %d", len(changes), len(want))
	}
	for i, w := range want {
		got := changes[i].FileDiff
		got.Patch = nil
		if got.Status != w.Status || got.OldPath != w.OldPath || got.NewPath != w.NewPath ||
			got.Additions != w.Additions || got.Deletions != w.Deletions || got.Binary != w.Binary {
			t.Errorf("change %d = %+v, want %+v", i, got, w)
		}
	}
	if changes[1].header != "diff --git a/old.txt b/new.txt" || changes[2].header != "diff --git a/bin b/bin" {
		t.Errorf("headers = %q, %q", changes[1].header, changes[2].header)
	}

	for _, bad := range []string{":100644 M\x00x\x00", "x\ty\x00", ":100644 100644 a b R100\x00only-one\x00"} {
		if _, err := parseDiffFiles([]byte(bad)); err == nil {
			t.Errorf("parseDiffFiles(%q) succeeded, want an error", bad)
		}
	}
}

func TestCheckPath(t *testing.T) {
	for _, ok := range []string{"", "a", "a/b.txt", "a b/c", "a[1]/*.go", ".github/workflows", "..x/y.."} {
		if err := checkPath(ok); err != nil {
			t.Errorf("checkPath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"/a", "a/", "a//b", ".", "..", "a/../b", "a/./b", "a\x00b", "\xff", strings.Repeat("a", maxPathLen+1)} {
		if err := checkPath(bad); err == nil {
			t.Errorf("checkPath(%q) = nil, want an error", bad)
		}
	}
}
