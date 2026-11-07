// Package browse reads the content of repositories: branches, tags,
// directories, files and commits. Dash's code browser and ci-svc use it
// through RefService and CommitService (ADR 0013). Nothing here changes a
// repository.
//
// Every read runs git plumbing (for-each-ref, rev-parse, ls-tree,
// cat-file, log, diff-tree) through package gitcmd. User input reaches git
// only as a validated ref name behind "refs/heads/" or "refs/tags/", as hex
// object IDs, or as a path after "--" with --literal-pathspecs. It is never
// read as an option or as a pattern.
package browse

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// Errors returned by Service, matched with errors.Is. Unknown and deleted
// repositories return repo.ErrNotFound, and bad input a
// *repo.ValidationError.
var (
	ErrRefNotFound    = errors.New("ref not found")
	ErrPathNotFound   = errors.New("path not found")
	ErrCommitNotFound = errors.New("commit not found")
)

// errEmpty means the repository has no refs, so no commits to read.
var errEmpty = errors.New("repository has no commits")

// Limits from the API contract (commit.proto, ref.proto; ADR 0013).
const (
	maxQueryLen    = 255
	maxPathLen     = 4096
	maxTreeEntries = 10000
	maxBlobBytes   = 1 << 20
	binaryProbe    = 8000 // bytes git looks at to call a file binary
	maxDiffFiles   = 300
	maxFilePatch   = 64 << 10
	maxTotalPatch  = 1 << 20
	maxLFSPointer  = 1024
)

// streamTimeout bounds the git calls that stream (blobs, patches); calls
// through gitcmd.Run have gitcmd's own timeout.
const streamTimeout = 10 * time.Minute

// defaultLastCommitTimeout bounds finding the last commit of a directory,
// which GetTree leaves out when it takes longer.
const defaultLastCommitTimeout = 10 * time.Second

// Repositories finds live repositories.
type Repositories interface {
	// Get returns repo.ErrNotFound for an unknown or deleted repository.
	Get(ctx context.Context, id uuid.UUID) (repo.Repository, error)
}

// Service reads repositories.
type Service struct {
	repos             Repositories
	layout            *storage.Layout
	git               *gitcmd.Runner
	lastCommitTimeout time.Duration
}

// NewService returns a Service that looks repositories up in repos and
// reads them from layout with git.
func NewService(repos Repositories, layout *storage.Layout, git *gitcmd.Runner) *Service {
	return &Service{repos: repos, layout: layout, git: git, lastCommitTimeout: defaultLastCommitTimeout}
}

// repository is a live repository opened for reading.
type repository struct {
	id            uuid.UUID
	dir           string
	defaultBranch string
}

// open checks that the repository is live and returns where it is.
func (s *Service) open(ctx context.Context, id uuid.UUID) (repository, error) {
	r, err := s.repos.Get(ctx, id)
	if err != nil {
		return repository{}, err
	}
	return repository{id: id, dir: s.layout.RepoPath(id), defaultBranch: r.DefaultBranch}, nil
}

// resolved is a ref turned into a commit.
type resolved struct {
	ref    string // the request's ref, or the default branch
	commit string // full SHA
}

// resolve turns ref into a commit: a full SHA, a branch, a tag, or the
// default branch when ref is empty. It returns errEmpty (with res.ref set)
// when the repository has no refs at all, and ErrRefNotFound otherwise.
func (s *Service) resolve(ctx context.Context, r repository, ref string) (resolved, error) {
	if ref == "" {
		ref = r.defaultBranch
	}
	candidates, err := refCandidates(ref)
	if err != nil {
		return resolved{}, err
	}
	for _, c := range candidates {
		sha, ok, err := s.revParse(ctx, r.dir, c+"^{commit}")
		if err != nil {
			return resolved{}, err
		}
		if ok {
			return resolved{ref: ref, commit: sha}, nil
		}
	}
	empty, err := s.isEmpty(ctx, r.dir)
	if err != nil {
		return resolved{}, err
	}
	if empty {
		return resolved{ref: ref}, errEmpty
	}
	return resolved{}, ErrRefNotFound
}

// refCandidates returns what git should try for ref, in order: the SHA
// itself, or the branch and then the tag of that name.
func refCandidates(ref string) ([]string, error) {
	if isFullSHA(ref) {
		return []string{ref}, nil
	}
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if name, ok := strings.CutPrefix(ref, prefix); ok {
			if err := repo.ValidateRefName("ref", name); err != nil {
				return nil, err
			}
			return []string{ref}, nil
		}
	}
	if err := repo.ValidateRefName("ref", ref); err != nil {
		return nil, err
	}
	return []string{"refs/heads/" + ref, "refs/tags/" + ref}, nil
}

// revParse resolves rev to a full object ID. ok is false if rev names no
// object.
func (s *Service) revParse(ctx context.Context, dir, rev string) (sha string, ok bool, err error) {
	out, err := s.git.Run(ctx, dir, "rev-parse", "--verify", "--quiet", rev)
	var gitErr *gitcmd.Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return "", false, nil // --quiet: not an object, no message
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve revision: %w", err)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// isEmpty reports whether the repository has no refs.
func (s *Service) isEmpty(ctx context.Context, dir string) (bool, error) {
	out, err := s.git.Run(ctx, dir, "for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil {
		return false, fmt.Errorf("list refs: %w", err)
	}
	return len(out) == 0, nil
}

// checkPath validates a path from a request; see the rules in
// commit.proto. Empty is allowed and means the root.
func checkPath(p string) error {
	switch {
	case p == "":
		return nil
	case len(p) > maxPathLen:
		return invalid("path", fmt.Sprintf("must be at most %d bytes", maxPathLen))
	case !utf8.ValidString(p) || strings.IndexByte(p, 0) >= 0:
		return invalid("path", "must be valid UTF-8 without NUL")
	}
	for part := range strings.SplitSeq(p, "/") {
		switch part {
		case "":
			return invalid("path", `must not start or end with "/" or contain "//"`)
		case ".", "..":
			return invalid("path", `must not contain "." or ".." parts`)
		}
	}
	return nil
}

func invalid(field, reason string) error {
	return &repo.ValidationError{Field: field, Reason: reason}
}

// isFullSHA reports whether s is a full SHA-1 object ID in lowercase hex.
func isFullSHA(s string) bool {
	return len(s) == 40 && isLowerHex(s)
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}

// clean makes a name, path or message from git valid UTF-8 for protobuf:
// bytes that are not UTF-8 become U+FFFD.
func clean(s string) string {
	return strings.ToValidUTF8(s, "�")
}

// Signature is who wrote or committed a commit, and when.
type Signature struct {
	Name  string
	Email string
	Time  time.Time
}

// Commit is one commit, without its changes.
type Commit struct {
	SHA        string
	ParentSHAs []string
	Author     Signature
	Committer  Signature
	Title      string // first line of Message
	Message    string // without trailing newlines
}

// commitFields is the number of NUL-separated fields that describe a
// commit, in the order of logFormat and commitAtoms: SHA, parents, author
// name, email and time, committer name, email and time, message.
const commitFields = 9

// logFormat prints a commit as commitFields fields for git log -z.
const logFormat = "--pretty=tformat:%H%x00%P%x00%an%x00%ae%x00%at%x00%cn%x00%ce%x00%ct%x00%B"

// commitAtoms are the for-each-ref atoms of a commit, in the same order.
var commitAtoms = []string{
	"objectname", "parent",
	"authorname", "authoremail", "authordate:unix",
	"committername", "committeremail", "committerdate:unix",
	"contents",
}

// parseCommit builds a Commit from commitFields fields.
func parseCommit(f []string) (Commit, error) {
	if len(f) != commitFields || !isFullSHA(f[0]) {
		return Commit{}, fmt.Errorf("unexpected commit fields from git: %q", f)
	}
	author, err := parseSignature(f[2], f[3], f[4])
	if err != nil {
		return Commit{}, err
	}
	committer, err := parseSignature(f[5], f[6], f[7])
	if err != nil {
		return Commit{}, err
	}
	message := clean(strings.TrimRight(f[8], "\n"))
	title, _, _ := strings.Cut(message, "\n")
	return Commit{
		SHA:        f[0],
		ParentSHAs: strings.Fields(f[1]),
		Author:     author,
		Committer:  committer,
		Title:      title,
		Message:    message,
	}, nil
}

func parseSignature(name, email, unix string) (Signature, error) {
	sec, err := strconv.ParseInt(unix, 10, 64)
	if err != nil {
		return Signature{}, fmt.Errorf("unexpected commit time from git: %q", unix)
	}
	// for-each-ref keeps the angle brackets; git log's %ae drops them.
	email = strings.TrimSuffix(strings.TrimPrefix(email, "<"), ">")
	return Signature{Name: clean(name), Email: clean(email), Time: time.Unix(sec, 0).UTC()}, nil
}

// logCommits runs git log with args (revisions, options, paths) and
// parses the commits it prints.
func (s *Service) logCommits(ctx context.Context, dir string, args ...string) ([]Commit, error) {
	out, err := s.git.Run(ctx, dir, append([]string{"--literal-pathspecs", "log", "-z", logFormat}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	// Each commit is commitFields fields, each followed by a NUL.
	fields := strings.Split(string(out), "\x00")
	fields = fields[:len(fields)-1] // after the last NUL
	if len(fields)%commitFields != 0 {
		return nil, fmt.Errorf("unexpected git log output: %d fields", len(fields))
	}
	commits := make([]Commit, 0, len(fields)/commitFields)
	for i := 0; i < len(fields); i += commitFields {
		c, err := parseCommit(fields[i : i+commitFields])
		if err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// forEachRef lists the refs under prefix with the given atoms, one record
// of len(atoms) fields per ref.
func (s *Service) forEachRef(ctx context.Context, dir, prefix string, atoms []string, extra ...string) ([][]string, error) {
	format := ""
	for _, a := range atoms {
		format += "%(" + a + ")%00"
	}
	args := append([]string{"for-each-ref", "--format=" + format}, extra...)
	out, err := s.git.Run(ctx, dir, append(args, prefix)...)
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	// Every field ends with a NUL, and every record with a newline, which
	// therefore starts the next record's first field.
	fields := strings.Split(string(out), "\x00")
	fields = fields[:len(fields)-1] // the final newline
	if len(fields)%len(atoms) != 0 {
		return nil, fmt.Errorf("unexpected git for-each-ref output: %d fields", len(fields))
	}
	var records [][]string
	for i := 0; i < len(fields); i += len(atoms) {
		rec := fields[i : i+len(atoms)]
		rec[0] = strings.TrimPrefix(rec[0], "\n")
		records = append(records, rec)
	}
	return records, nil
}
