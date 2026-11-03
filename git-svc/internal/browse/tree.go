package browse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

// EntryType is what a directory entry is.
type EntryType int

// Entry types, from the entry's Git mode.
const (
	EntryFile EntryType = iota + 1
	EntryDirectory
	EntrySymlink
	EntrySubmodule
)

// TreeEntry is one entry of a directory.
type TreeEntry struct {
	Name string
	Path string
	Type EntryType
	Mode string // octal, as git prints it
	SHA  string
	Size int64 // files and symlinks only; -1 otherwise
}

// Tree is the listing of one directory.
type Tree struct {
	Ref        string
	CommitSHA  string
	Path       string
	Empty      bool // the repository has no commits
	Entries    []TreeEntry
	Truncated  bool
	LastCommit *Commit
}

// Blob is one file, for display.
type Blob struct {
	Ref       string
	CommitSHA string
	Path      string
	Name      string
	SHA       string
	Mode      string
	Type      EntryType // EntryFile or EntrySymlink
	Size      int64
	Binary    bool
	LFS       bool
	Truncated bool
	Content   []byte // nil for binary files
}

// RawFile is a file found by OpenRaw, ready to be written by WriteRaw.
type RawFile struct {
	Name string
	Size int64
	dir  string
	sha  string
}

// GetTree lists the directory at path (the root if empty) at ref.
func (s *Service) GetTree(ctx context.Context, id uuid.UUID, ref, dirPath string) (Tree, error) {
	if err := checkPath(dirPath); err != nil {
		return Tree{}, err
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return Tree{}, err
	}
	res, err := s.resolve(ctx, r, ref)
	if errors.Is(err, errEmpty) {
		return Tree{Ref: res.ref, Path: dirPath, Empty: true}, nil
	}
	if err != nil {
		return Tree{}, err
	}

	treeish := res.commit
	if dirPath != "" {
		e, err := s.lookup(ctx, r.dir, res.commit, dirPath)
		if err != nil {
			return Tree{}, err
		}
		switch e.Type {
		case EntryDirectory:
			treeish = e.SHA
		case EntrySubmodule:
			return Tree{}, invalid("path", "is a submodule, which git-svc cannot read")
		default:
			return Tree{}, invalid("path", "is a file, not a directory")
		}
	}
	out, err := s.git.Run(ctx, r.dir, "ls-tree", "-z", "--long", treeish)
	if err != nil {
		return Tree{}, fmt.Errorf("list directory: %w", err)
	}
	entries, err := parseLsTree(out)
	if err != nil {
		return Tree{}, err
	}
	for i := range entries {
		entries[i].Path = clean(path.Join(dirPath, entries[i].Name))
		entries[i].Name = clean(entries[i].Name)
	}
	// Directories first, then by name in byte order.
	slices.SortFunc(entries, func(a, b TreeEntry) int {
		if (a.Type == EntryDirectory) != (b.Type == EntryDirectory) {
			if a.Type == EntryDirectory {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	tree := Tree{Ref: res.ref, CommitSHA: res.commit, Path: dirPath, Entries: entries}
	if len(entries) > maxTreeEntries {
		tree.Entries, tree.Truncated = entries[:maxTreeEntries], true
	}

	args := []string{"-1", res.commit}
	if dirPath != "" {
		args = append(args, "--", dirPath)
	}
	last, err := s.logCommits(ctx, r.dir, args...)
	if err != nil {
		return Tree{}, err
	}
	if len(last) == 1 {
		tree.LastCommit = &last[0]
	}
	return tree, nil
}

// GetBlob returns the file at path at ref, cut at maxBlobBytes.
func (s *Service) GetBlob(ctx context.Context, id uuid.UUID, ref, filePath string) (Blob, error) {
	r, res, e, err := s.file(ctx, id, ref, filePath)
	if err != nil {
		return Blob{}, err
	}
	content, err := s.readBlob(ctx, r.dir, e.SHA, min(e.Size, maxBlobBytes))
	if err != nil {
		return Blob{}, err
	}
	b := Blob{
		Ref:       res.ref,
		CommitSHA: res.commit,
		Path:      filePath,
		Name:      path.Base(filePath),
		SHA:       e.SHA,
		Mode:      e.Mode,
		Type:      e.Type,
		Size:      e.Size,
		Truncated: e.Size > maxBlobBytes,
		Binary:    bytes.IndexByte(content[:min(len(content), binaryProbe)], 0) >= 0,
	}
	if !b.Binary {
		b.Content = content
		b.LFS = e.Size < maxLFSPointer && isLFSPointer(content)
	}
	return b, nil
}

// OpenRaw finds the file at path at ref, for WriteRaw.
func (s *Service) OpenRaw(ctx context.Context, id uuid.UUID, ref, filePath string) (RawFile, error) {
	r, _, e, err := s.file(ctx, id, ref, filePath)
	if err != nil {
		return RawFile{}, err
	}
	return RawFile{Name: path.Base(filePath), Size: e.Size, dir: r.dir, sha: e.SHA}, nil
}

// WriteRaw writes the bytes of f to w. If w fails, git stops and the error
// is returned.
func (s *Service) WriteRaw(ctx context.Context, f RawFile, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	failed := &failingWriter{w: w}
	err := s.git.Stream(ctx, gitcmd.StreamOptions{Dir: f.dir, Stdout: failed}, "cat-file", "blob", f.sha)
	if failed.err != nil {
		return failed.err
	}
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	return nil
}

// file finds the file at path at ref and checks that it is one.
func (s *Service) file(ctx context.Context, id uuid.UUID, ref, filePath string) (repository, resolved, TreeEntry, error) {
	if filePath == "" {
		return repository{}, resolved{}, TreeEntry{}, invalid("path", "is required")
	}
	if err := checkPath(filePath); err != nil {
		return repository{}, resolved{}, TreeEntry{}, err
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return repository{}, resolved{}, TreeEntry{}, err
	}
	res, err := s.resolve(ctx, r, ref)
	if errors.Is(err, errEmpty) {
		err = ErrRefNotFound
	}
	if err != nil {
		return repository{}, resolved{}, TreeEntry{}, err
	}
	e, err := s.lookup(ctx, r.dir, res.commit, filePath)
	if err != nil {
		return repository{}, resolved{}, TreeEntry{}, err
	}
	switch e.Type {
	case EntryDirectory:
		return repository{}, resolved{}, TreeEntry{}, invalid("path", "is a directory, not a file")
	case EntrySubmodule:
		return repository{}, resolved{}, TreeEntry{}, invalid("path", "is a submodule, which git-svc cannot read")
	}
	return r, res, e, nil
}

// lookup returns the entry at path in commit, or ErrPathNotFound.
func (s *Service) lookup(ctx context.Context, dir, commit, p string) (TreeEntry, error) {
	out, err := s.git.Run(ctx, dir, "--literal-pathspecs", "ls-tree", "-z", "--long", "--full-tree", commit, "--", p)
	if err != nil {
		return TreeEntry{}, fmt.Errorf("find path: %w", err)
	}
	entries, err := parseLsTree(out)
	if err != nil {
		return TreeEntry{}, err
	}
	for _, e := range entries {
		if e.Name == p { // with a path, ls-tree prints it whole
			e.Path = p
			return e, nil
		}
	}
	return TreeEntry{}, ErrPathNotFound
}

// parseLsTree parses `git ls-tree -z --long`: one
// "<mode> <type> <sha> <size>\t<name>" record per entry, each followed by
// a NUL. Names are returned as git printed them.
func parseLsTree(out []byte) ([]TreeEntry, error) {
	records := strings.Split(string(out), "\x00")
	records = records[:len(records)-1] // after the last NUL
	entries := make([]TreeEntry, 0, len(records))
	for _, rec := range records {
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 4 {
			return nil, fmt.Errorf("unexpected git ls-tree output: %q", rec)
		}
		e := TreeEntry{Name: name, Mode: f[0], SHA: f[2], Type: entryType(f[0]), Size: -1}
		if e.Type == EntryFile || e.Type == EntrySymlink {
			size, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("unexpected size in git ls-tree output: %q", rec)
			}
			e.Size = size
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func entryType(mode string) EntryType {
	switch mode {
	case "040000":
		return EntryDirectory
	case "120000":
		return EntrySymlink
	case "160000":
		return EntrySubmodule
	}
	return EntryFile
}

// isLFSPointer reports whether a small file is a Git LFS pointer.
func isLFSPointer(content []byte) bool {
	return bytes.HasPrefix(content, []byte("version https://git-lfs.github.com/spec/v1\n")) ||
		bytes.HasPrefix(content, []byte("version https://hawser.github.com/spec/v1\n")) // pre-1.0 LFS
}

// readBlob returns the first n bytes of a blob. It stops git once it has
// them, so a large file is never read whole.
func (s *Service) readBlob(ctx context.Context, dir, sha string, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()
	stop, stopGit := context.WithCancel(ctx)
	defer stopGit()
	w := &prefixWriter{buf: make([]byte, 0, n), full: stopGit}
	err := s.git.Stream(stop, gitcmd.StreamOptions{Dir: dir, Stdout: w}, "cat-file", "blob", sha)
	if int64(len(w.buf)) == n {
		return w.buf, nil // git may have been stopped by us; that is no error
	}
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return w.buf, nil
}

// prefixWriter keeps the first cap(buf) bytes written to it, then calls
// full and drops the rest.
type prefixWriter struct {
	buf  []byte
	full context.CancelFunc
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if room := cap(w.buf) - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
		if len(w.buf) == cap(w.buf) {
			w.full()
		}
	}
	return len(p), nil
}

// failingWriter remembers the first error of w, for example a client that
// went away, so that it is reported instead of git's reaction to it.
type failingWriter struct {
	w   io.Writer
	err error
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n, err := f.w.Write(p)
	f.err = err
	return n, err
}
