package browse

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// ErrNoMergeBase means that two refs share no history, so there is nothing
// to compare them from.
var ErrNoMergeBase = errors.New("no common history")

// compareCommitLimit is the most commits CompareRefs returns (diff.proto). A
// variable only so that tests can lower it.
var compareCommitLimit = 250

// Comparison is what head has that base does not: the answer of CompareRefs.
type Comparison struct {
	BaseSHA      string
	HeadSHA      string
	MergeBaseSHA string
	AheadBy      int // commits head has that base does not
	BehindBy     int // commits base has that head does not

	Commits          []Commit // base..head, newest first, at most compareCommitLimit
	CommitsTruncated bool

	Stats          DiffStats  // from the merge base to head, over all files
	Files          []FileDiff // at most maxDiffFiles
	FilesTruncated bool
}

// CompareRefs compares head with base the way a merge request does: the
// commits that head has and base does not, and the changes from their merge
// base to head (`git diff base...head`). base and head are branches, tags,
// full refs or full SHAs; unlike the other reads, neither may be empty.
//
// When base already contains head, the comparison is empty, not an error.
// Refs without common history return ErrNoMergeBase.
func (s *Service) CompareRefs(ctx context.Context, id uuid.UUID, base, head string) (Comparison, error) {
	if err := checkCompared("base", base); err != nil {
		return Comparison{}, err
	}
	if err := checkCompared("head", head); err != nil {
		return Comparison{}, err
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return Comparison{}, err
	}
	baseSHA, err := s.resolveCompared(ctx, r, base)
	if err != nil {
		return Comparison{}, err
	}
	headSHA, err := s.resolveCompared(ctx, r, head)
	if err != nil {
		return Comparison{}, err
	}

	mergeBase, err := s.mergeBase(ctx, r.dir, baseSHA, headSHA)
	if err != nil {
		return Comparison{}, err
	}
	behind, ahead, err := s.countApart(ctx, r.dir, baseSHA, headSHA)
	if err != nil {
		return Comparison{}, err
	}
	cmp := Comparison{
		BaseSHA:          baseSHA,
		HeadSHA:          headSHA,
		MergeBaseSHA:     mergeBase,
		AheadBy:          ahead,
		BehindBy:         behind,
		CommitsTruncated: ahead > compareCommitLimit,
	}
	if ahead > 0 {
		cmp.Commits, err = s.logCommits(ctx, r.dir, "--max-count="+strconv.Itoa(compareCommitLimit), baseSHA+".."+headSHA)
		if err != nil {
			return Comparison{}, err
		}
	}
	if mergeBase != headSHA {
		cmp.Stats, cmp.Files, cmp.FilesTruncated, err = s.diff(ctx, r.dir, []string{mergeBase, headSHA})
		if err != nil {
			return Comparison{}, err
		}
	}
	return cmp, nil
}

// checkCompared validates base or head before anything is read: the forms
// of commit.proto's ref, without the empty default.
func checkCompared(field, ref string) error {
	if ref == "" {
		return invalid(field, "is required")
	}
	if isFullSHA(ref) {
		return nil
	}
	name := ref
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			name = rest
			break
		}
	}
	return repo.ValidateRefName(field, name)
}

// resolveCompared turns base or head into a commit. A repository without
// commits has no ref to compare, so that is ErrRefNotFound too.
func (s *Service) resolveCompared(ctx context.Context, r repository, ref string) (string, error) {
	res, err := s.resolve(ctx, r, ref)
	if errors.Is(err, errEmpty) {
		return "", ErrRefNotFound
	}
	if err != nil {
		return "", err
	}
	return res.commit, nil
}

// mergeBase returns the best common ancestor of two commits, or
// ErrNoMergeBase when they have none.
func (s *Service) mergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := s.git.Run(ctx, dir, "merge-base", a, b)
	var gitErr *gitcmd.Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return "", ErrNoMergeBase // git found no common ancestor
	}
	if err != nil {
		return "", fmt.Errorf("find merge base: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if !isFullSHA(sha) {
		return "", fmt.Errorf("unexpected git merge-base output: %q", out)
	}
	return sha, nil
}

// countApart returns how many commits only base has (behind) and how many
// only head has (ahead).
func (s *Service) countApart(ctx context.Context, dir, base, head string) (behind, ahead int, err error) {
	out, err := s.git.Run(ctx, dir, "rev-list", "--left-right", "--count", base+"..."+head)
	if err != nil {
		return 0, 0, fmt.Errorf("count commits: %w", err)
	}
	left, right, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
	behind, errL := strconv.Atoi(left)
	ahead, errR := strconv.Atoi(right)
	if !ok || errL != nil || errR != nil {
		return 0, 0, fmt.Errorf("unexpected git rev-list output: %q", out)
	}
	return behind, ahead, nil
}
