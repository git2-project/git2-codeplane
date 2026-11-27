// Package merge checks and does the merges of merge requests: CheckMerge
// and MergeBranches of OperationService (ADR 0017).
//
// It works on the bare repository with git plumbing only, never with a
// working tree:
//   - merge-base finds the common ancestor and tells fast-forwards apart;
//   - merge-tree --write-tree computes the merged tree and the conflicts
//     (git 2.38 or newer, git-svc's minimum);
//   - commit-tree writes the merge or squash commit;
//   - update-ref moves the target branch only from the commit it merged
//     with, so a concurrent push or merge is never overwritten.
//
// User input reaches git only as a validated branch name behind
// "refs/heads/", as hex object IDs, or in the environment and on standard
// input of commit-tree (names, emails and the message). It is never read as
// an option.
package merge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
	"github.com/git2-project/git2-codeplane/git-svc/internal/storage"
)

// ErrBranchNotFound means the source or the target branch does not exist.
var ErrBranchNotFound = errors.New("branch not found")

// Limits from the API contract (operation.proto).
const (
	maxConflicts = 100
)

// gitTimeout bounds one git call. merge-tree reads every object that
// differs between the branches, which takes longer than a single read in a
// large repository.
const gitTimeout = 2 * time.Minute

// Repositories finds live repositories and records the event of a merge.
type Repositories interface {
	// Get returns repo.ErrNotFound for an unknown or deleted repository.
	Get(ctx context.Context, id uuid.UUID) (repo.Repository, error)
	// RecordPush writes the RepositoryPushed event of a ref update.
	RecordPush(ctx context.Context, p repo.Push) error
}

// Service checks and does merges.
type Service struct {
	repos  Repositories
	layout *storage.Layout
	git    *gitcmd.Runner
	log    *slog.Logger
	now    func() time.Time
}

// NewService returns a Service that finds repositories through repos,
// changes them under layout with git, and logs to log.
func NewService(repos Repositories, layout *storage.Layout, git *gitcmd.Runner, log *slog.Logger) *Service {
	return &Service{repos: repos, layout: layout, git: git, log: log, now: time.Now}
}

// Status is whether a merge is possible.
type Status int

// Statuses of a Check.
const (
	StatusClean         Status = iota + 1 // the branches merge without conflicts
	StatusConflicts                       // Conflicts lists the files git cannot merge
	StatusAlreadyMerged                   // the target has every change of the source
	StatusUnrelated                       // the branches have no common history
)

// Check is whether, and how, a source branch merges into a target branch.
type Check struct {
	SourceSHA          string
	TargetSHA          string
	MergeBaseSHA       string // empty for StatusUnrelated
	Status             Status
	CanFastForward     bool     // only with StatusClean
	Conflicts          []string // paths, at most maxConflicts; only with StatusConflicts
	ConflictsTruncated bool

	tree string // the merged tree, with StatusClean
}

// CheckMerge tells whether source can be merged into target now. It changes
// no branch; merge-tree does write the merged tree's objects, which nothing
// references unless a merge commit is made from them.
func (s *Service) CheckMerge(ctx context.Context, id uuid.UUID, source, target string) (Check, error) {
	if err := checkBranches(source, target); err != nil {
		return Check{}, err
	}
	dir, err := s.open(ctx, id)
	if err != nil {
		return Check{}, err
	}
	src, err := s.branchTip(ctx, dir, source)
	if err != nil {
		return Check{}, err
	}
	tgt, err := s.branchTip(ctx, dir, target)
	if err != nil {
		return Check{}, err
	}
	return s.analyze(ctx, dir, src, tgt)
}

// checkBranches validates the two branch names of a request.
func checkBranches(source, target string) error {
	if err := repo.ValidateRefName("source_branch", source); err != nil {
		return err
	}
	if err := repo.ValidateRefName("target_branch", target); err != nil {
		return err
	}
	if source == target {
		return &repo.ValidationError{Field: "target_branch", Reason: "must differ from source_branch"}
	}
	return nil
}

// open checks that the repository is live and returns its directory.
func (s *Service) open(ctx context.Context, id uuid.UUID) (string, error) {
	if _, err := s.repos.Get(ctx, id); err != nil {
		return "", err
	}
	return s.layout.RepoPath(id), nil
}

// analyze checks the merge of commit src into commit tgt.
func (s *Service) analyze(ctx context.Context, dir, src, tgt string) (Check, error) {
	c := Check{SourceSHA: src, TargetSHA: tgt}
	base, ok, err := s.mergeBase(ctx, dir, tgt, src)
	if err != nil {
		return Check{}, err
	}
	if !ok {
		c.Status = StatusUnrelated
		return c, nil
	}
	c.MergeBaseSHA = base
	if base == src {
		c.Status = StatusAlreadyMerged // the target already contains the source
		return c, nil
	}

	tree, conflicts, err := s.mergeTree(ctx, dir, tgt, src)
	if err != nil {
		return Check{}, err
	}
	if len(conflicts) > 0 {
		c.Status = StatusConflicts
		if len(conflicts) > maxConflicts {
			conflicts, c.ConflictsTruncated = conflicts[:maxConflicts], true
		}
		c.Conflicts = conflicts
		return c, nil
	}
	tgtTree, err := s.revParse(ctx, dir, tgt+"^{tree}")
	if err != nil {
		return Check{}, err
	}
	if tree == tgtTree {
		// The source's changes are already on the target, for example
		// after a squash: a merge would change no file. This compares
		// files, not commits, so it holds for a fast-forward too: a source
		// whose commits add up to no change (a commit and its revert) is
		// already merged, although its commits are not on the target.
		c.Status = StatusAlreadyMerged
		return c, nil
	}
	c.Status, c.tree, c.CanFastForward = StatusClean, tree, base == tgt
	return c, nil
}

// branchTip returns the commit a branch points to, or ErrBranchNotFound.
func (s *Service) branchTip(ctx context.Context, dir, branch string) (string, error) {
	out, err := s.git.Run(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	var gitErr *gitcmd.Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return "", ErrBranchNotFound // --quiet: no such branch, no message
	}
	if err != nil {
		return "", fmt.Errorf("resolve branch: %w", err)
	}
	return parseSHA("rev-parse", out)
}

// revParse resolves rev, which must name an object.
func (s *Service) revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := s.git.Run(ctx, dir, "rev-parse", "--verify", rev)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", rev, err)
	}
	return parseSHA("rev-parse", out)
}

// mergeBase returns the best common ancestor of two commits; ok is false
// when they have none.
func (s *Service) mergeBase(ctx context.Context, dir, a, b string) (base string, ok bool, err error) {
	out, err := s.git.Run(ctx, dir, "merge-base", a, b)
	var gitErr *gitcmd.Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return "", false, nil // no common ancestor
	}
	if err != nil {
		return "", false, fmt.Errorf("find merge base: %w", err)
	}
	base, err = parseSHA("merge-base", out)
	return base, err == nil, err
}

// mergeTree merges commit theirs into commit ours without a working tree.
// It returns the merged tree when the merge is clean, and the conflicting
// paths otherwise, in git's order.
//
// With -z, --name-only and --no-messages, git prints the tree's ID and a
// NUL, then each conflicting path and a NUL, and exits with 1 when there
// are conflicts.
func (s *Service) mergeTree(ctx context.Context, dir, ours, theirs string) (tree string, conflicts []string, err error) {
	out, err := s.run(ctx, dir, nil, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", ours, theirs)
	conflicted := false
	var gitErr *gitcmd.Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 && ctx.Err() == nil {
		conflicted = true
	} else if err != nil {
		return "", nil, fmt.Errorf("merge trees: %w", err)
	}

	fields := strings.Split(string(out), "\x00")
	if len(fields) < 2 || fields[len(fields)-1] != "" {
		return "", nil, fmt.Errorf("unexpected git merge-tree output: %q", out)
	}
	if tree, err = parseSHA("merge-tree", []byte(fields[0])); err != nil {
		return "", nil, err
	}
	seen := map[string]bool{}
	for _, p := range fields[1 : len(fields)-1] {
		if p != "" && !seen[p] {
			seen[p] = true
			conflicts = append(conflicts, strings.ToValidUTF8(p, "�"))
		}
	}
	if conflicted != (len(conflicts) > 0) {
		return "", nil, fmt.Errorf("git merge-tree exit status and conflicts disagree: %q", out)
	}
	return tree, conflicts, nil
}

// run runs git with extra environment and standard input, and returns its
// standard output also when git fails: merge-tree reports conflicts with
// exit status 1 and its normal output.
func (s *Service) run(ctx context.Context, dir string, env []string, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	var out bytes.Buffer
	opts := gitcmd.StreamOptions{Dir: dir, Env: env, Stdout: &out}
	if stdin != nil {
		opts.Stdin = bytes.NewReader(stdin)
	}
	err := s.git.Stream(ctx, opts, args...)
	return out.Bytes(), err
}

// parseSHA reads a full SHA-1 object ID printed by git.
func parseSHA(cmd string, out []byte) (string, error) {
	sha := strings.TrimSpace(string(out))
	if !isFullSHA(sha) {
		return "", fmt.Errorf("unexpected git %s output: %q", cmd, out)
	}
	return sha, nil
}

// isFullSHA reports whether s is a full SHA-1 object ID in lowercase hex.
// git-svc creates every repository with SHA-1 (ADR 0013).
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
