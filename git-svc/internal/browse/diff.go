package browse

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

// DiffStats are the totals of a diff.
type DiffStats struct {
	FilesChanged int
	Additions    int
	Deletions    int
}

// FileStatus is how a file changed.
type FileStatus int

// File statuses. Copies are not detected yet, so FileCopied is unused.
const (
	FileAdded FileStatus = iota + 1
	FileModified
	FileDeleted
	FileRenamed
	FileCopied
)

// FileDiff is one changed file.
type FileDiff struct {
	Status    FileStatus
	OldPath   string // empty for an added file
	NewPath   string // empty for a deleted file
	Additions int
	Deletions int
	Binary    bool
	Truncated bool   // the patch was too large and is left out
	Patch     []byte // hunks from the first "@@" line
}

// change is one file of a diff before its patch is read.
type change struct {
	FileDiff
	header   string // the "diff --git" line git prints for it
	sections int    // patch sections git prints for it: 2 for a type change
}

// diffArgs are the options shared by the file list and the patches:
// rename detection, and nothing from configuration that could change the
// output.
var diffArgs = []string{"-r", "-M", "--no-commit-id", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/"}

// diffFiles lists the files that differ between trees (two tree-ish, or
// "--root" and a commit), with their line counts, in git's order.
func (s *Service) diffFiles(ctx context.Context, dir string, trees []string) ([]change, DiffStats, error) {
	args := append(append([]string{"diff-tree", "-z", "--raw", "--numstat"}, diffArgs...), trees...)
	out, err := s.git.Run(ctx, dir, args...)
	if err != nil {
		return nil, DiffStats{}, fmt.Errorf("list changed files: %w", err)
	}
	changes, err := parseDiffFiles(out)
	if err != nil {
		return nil, DiffStats{}, err
	}
	stats := DiffStats{FilesChanged: len(changes)}
	for _, c := range changes {
		stats.Additions += c.Additions
		stats.Deletions += c.Deletions
	}
	return changes, stats, nil
}

// parseDiffFiles parses `git diff-tree -z --raw --numstat`. Each file has a
// raw record (":<mode> <mode> <sha> <sha> <status>", then one path, or two
// for a rename) and a numstat record ("<added>\t<deleted>\t<path>", or
// "<added>\t<deleted>\t" and two paths for a rename), in the same order.
func parseDiffFiles(out []byte) ([]change, error) {
	tokens := strings.Split(string(out), "\x00")
	tokens = tokens[:len(tokens)-1] // after the last NUL
	var raws []change
	var counts [][2]int // -1 for binary
	for i := 0; i < len(tokens); {
		tok := tokens[i]
		if meta, ok := strings.CutPrefix(tok, ":"); ok {
			f := strings.Fields(meta)
			if len(f) != 5 || f[4] == "" {
				return nil, fmt.Errorf("unexpected git diff-tree record: %q", tok)
			}
			paths := 1
			if f[4][0] == 'R' || f[4][0] == 'C' {
				paths = 2
			}
			if i+paths >= len(tokens) {
				return nil, fmt.Errorf("truncated git diff-tree record: %q", tok)
			}
			raws = append(raws, newChange(f[4][0], tokens[i+1:i+1+paths]))
			i += 1 + paths
			continue
		}
		added, rest, ok1 := strings.Cut(tok, "\t")
		deleted, p, ok2 := strings.Cut(rest, "\t")
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("unexpected git diff-tree record: %q", tok)
		}
		i++
		if p == "" { // a rename: the two paths follow
			if i += 2; i > len(tokens) {
				return nil, fmt.Errorf("truncated git diff-tree record: %q", tok)
			}
		}
		a, errA := strconv.Atoi(added)
		d, errD := strconv.Atoi(deleted)
		switch {
		case added == "-" && deleted == "-":
			counts = append(counts, [2]int{-1, -1})
		case errA != nil || errD != nil:
			return nil, fmt.Errorf("unexpected git diff-tree counts: %q", tok)
		default:
			counts = append(counts, [2]int{a, d})
		}
	}
	if len(counts) != len(raws) {
		return nil, fmt.Errorf("git diff-tree listed %d files but counted %d", len(raws), len(counts))
	}
	for i := range raws {
		if counts[i][0] < 0 {
			raws[i].Binary = true
		} else {
			raws[i].Additions, raws[i].Deletions = counts[i][0], counts[i][1]
		}
	}
	return raws, nil
}

// newChange builds a change from a raw record's status letter and paths.
func newChange(status byte, paths []string) change {
	c := change{sections: 1}
	from, to := paths[0], paths[len(paths)-1]
	switch status {
	case 'A':
		c.Status, c.NewPath = FileAdded, clean(to)
	case 'D':
		c.Status, c.OldPath = FileDeleted, clean(from)
	case 'R':
		c.Status, c.OldPath, c.NewPath = FileRenamed, clean(from), clean(to)
	case 'C':
		c.Status, c.OldPath, c.NewPath = FileCopied, clean(from), clean(to)
	case 'T':
		// git prints a type change as a deletion and an addition.
		c.Status, c.OldPath, c.NewPath, c.sections = FileModified, clean(from), clean(to), 2
	default:
		c.Status, c.OldPath, c.NewPath = FileModified, clean(from), clean(to)
	}
	c.header = "diff --git " + quotePath("a/"+from) + " " + quotePath("b/"+to)
	return c
}

// quotePath returns a path the way git prints it in a "diff --git" line with
// core.quotePath=false: control characters, double quotes and backslashes are
// escaped as in C, and the whole path then goes in double quotes. Bytes from
// 0x80 up stay as they are. Git decides this for each path on its own, so one
// path of a rename can be quoted and the other not.
func quotePath(p string) string {
	var b strings.Builder
	for i := range len(p) {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= '\a' && c <= '\r':
			b.WriteByte('\\')
			b.WriteByte("abtnvfr"[c-'\a'])
		case c < ' ' || c == 0x7f:
			fmt.Fprintf(&b, `\%03o`, c)
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() == len(p) {
		return p // nothing was escaped
	}
	return `"` + b.String() + `"`
}

// diffPatches reads the patches of changes, which are the first files of
// the diff between trees, and returns the files with their patches.
func (s *Service) diffPatches(ctx context.Context, dir string, trees []string, changes []change) ([]FileDiff, error) {
	want := 0
	for _, c := range changes {
		want += c.sections
	}
	files := make([]FileDiff, len(changes))
	if want == 0 {
		return files, nil
	}

	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	stop, stopGit := context.WithCancel(ctx)
	defer stopGit()
	split := &patchSplitter{want: want, stop: stopGit, atStart: true}
	// core.quotePath=false keeps non-ASCII paths unquoted in the headers; see
	// quotePath for the rest.
	args := append(append([]string{"-c", "core.quotePath=false", "diff-tree", "-p", "--no-color"}, diffArgs...), trees...)
	err := s.git.Stream(stop, gitcmd.StreamOptions{Dir: dir, Stdout: split}, args...)
	split.flush()
	if err != nil && !split.stopped {
		return nil, fmt.Errorf("read patches: %w", err)
	}

	next := 0
	for i, c := range changes {
		f := c.FileDiff
		var patch []byte
		for range c.sections {
			if next >= len(split.sections) {
				if !split.stopped {
					return nil, fmt.Errorf("git printed %d patches for %d files", len(split.sections), len(changes))
				}
				f.Truncated = true // never reached: the total limit stopped git
				next++
				continue
			}
			sec := split.sections[next]
			next++
			if h := string(bytes.TrimSuffix(sec.header, []byte("\n"))); len(sec.header) < maxHeader && h != c.header {
				return nil, fmt.Errorf("patch %d is for %q, want %q", next-1, h, c.header)
			}
			f.Truncated = f.Truncated || sec.truncated
			patch = append(patch, sec.hunks...)
		}
		if !f.Binary && !f.Truncated && len(patch) > 0 {
			f.Patch = patch
		}
		files[i] = f
	}
	return files, nil
}

// headProbe is how much of a line patchSplitter needs to classify it.
const headProbe = len("diff --git ")

// maxHeader bounds the "diff --git" line kept per section; a longer one is
// not checked against the file list.
const maxHeader = 16 << 10

// section is the patch of one "diff --git" section.
type section struct {
	header    []byte // the "diff --git" line
	hunks     []byte // from the first "@@" line
	inHunks   bool
	truncated bool
}

// lineMode is what patchSplitter does with the current line.
type lineMode int

const (
	lineSkip   lineMode = iota // a header line such as "index ..." or "+++ b/x"
	lineHeader                 // the "diff --git" line
	lineHunk                   // a line of the hunks
)

// patchSplitter receives the output of `git diff-tree -p` and splits it
// into sections at the lines that start with "diff --git ", which a patch
// line never does (those start with " ", "+", "-", "\" or "@@"). It keeps
// at most maxFilePatch bytes of hunks per section and maxTotalPatch in
// all, and stops git as soon as it has want sections or reaches the total
// limit. Memory stays bounded however long the lines are.
type patchSplitter struct {
	want     int
	stop     context.CancelFunc
	sections []*section
	total    int  // bytes of hunks kept
	stopped  bool // stop was called, so git did not finish

	atStart bool   // the next byte starts a line
	head    []byte // the start of the current line, until it is classified
	mode    lineMode
}

func (p *patchSplitter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 && !p.stopped {
		if !p.atStart {
			seg := b
			if i := bytes.IndexByte(b, '\n'); i >= 0 {
				seg, p.atStart = b[:i+1], true
			}
			p.emit(seg)
			b = b[len(seg):]
			continue
		}
		take := min(headProbe-len(p.head), len(b))
		if i := bytes.IndexByte(b[:take], '\n'); i >= 0 {
			take = i + 1
		}
		p.head = append(p.head, b[:take]...)
		b = b[take:]
		if ended := p.head[len(p.head)-1] == '\n'; len(p.head) == headProbe || ended {
			p.startLine()
			p.atStart = ended
		}
	}
	return n, nil
}

// flush handles a last line without a newline.
func (p *patchSplitter) flush() {
	if len(p.head) > 0 && !p.stopped {
		p.startLine()
	}
}

// startLine classifies the line whose start is in head, then emits head.
func (p *patchSplitter) startLine() {
	switch {
	case bytes.HasPrefix(p.head, []byte("diff --git ")):
		if len(p.sections) == p.want {
			p.halt() // every wanted section is complete
			return
		}
		p.sections = append(p.sections, &section{})
		p.mode = lineHeader
	case len(p.sections) == 0:
		p.mode = lineSkip
	default:
		cur := p.sections[len(p.sections)-1]
		if !cur.inHunks && bytes.HasPrefix(p.head, []byte("@@")) {
			cur.inHunks = true
		}
		p.mode = lineSkip
		if cur.inHunks {
			p.mode = lineHunk
		}
	}
	p.emit(p.head)
	p.head = p.head[:0]
}

// emit handles bytes of the current line according to its mode.
func (p *patchSplitter) emit(seg []byte) {
	if len(p.sections) == 0 {
		return
	}
	cur := p.sections[len(p.sections)-1]
	switch p.mode {
	case lineHeader:
		if room := maxHeader - len(cur.header); room > 0 {
			cur.header = append(cur.header, seg[:min(room, len(seg))]...)
		}
	case lineHunk:
		if cur.truncated {
			return
		}
		perFile, overall := len(cur.hunks)+len(seg) > maxFilePatch, p.total+len(seg) > maxTotalPatch
		if perFile || overall {
			// The file's patch is left out, and its bytes free up room.
			p.total -= len(cur.hunks)
			cur.hunks, cur.truncated = nil, true
			if overall && !perFile {
				p.halt() // the commit's patches are full: later files are left out too
			}
			return
		}
		cur.hunks = append(cur.hunks, seg...)
		p.total += len(seg)
	}
}

func (p *patchSplitter) halt() {
	p.stopped = true
	p.stop()
}
