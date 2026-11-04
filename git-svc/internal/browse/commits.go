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

// CommitPage is one page of ListCommits.
type CommitPage struct {
	Commits       []Commit
	NextPageToken string
}

// CommitDetail is a commit with its changes against its first parent.
type CommitDetail struct {
	Commit
	Stats     DiffStats
	Files     []FileDiff // at most maxDiffFiles
	Truncated bool       // more files changed than Files holds
}

// commitCursor is where the next page of ListCommits starts: Skip commits
// into the history of Commit, which never changes, so pages stay
// consistent when the branch moves.
type commitCursor struct {
	Repo   uuid.UUID `json:"r"`
	Commit string    `json:"c"`
	Path   string    `json:"p,omitempty"`
	Skip   int       `json:"s"`
}

// maxSkip bounds a page token's offset, so a forged token cannot make git
// walk an absurd distance.
const maxSkip = 1_000_000

// ListCommits returns one page of the commits reachable from ref that
// changed path (all commits if path is empty), newest first.
func (s *Service) ListCommits(ctx context.Context, id uuid.UUID, ref, filePath string, pageSize int32, pageToken string) (CommitPage, error) {
	if err := checkPath(filePath); err != nil {
		return CommitPage{}, err
	}
	size, err := repo.PageSize(pageSize)
	if err != nil {
		return CommitPage{}, err
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return CommitPage{}, err
	}

	cur := commitCursor{Repo: id, Path: filePath}
	if pageToken != "" {
		var c commitCursor
		if err := decodeToken(pageToken, &c); err != nil || c.Repo != id || c.Path != filePath ||
			!isFullSHA(c.Commit) || c.Skip < 0 || c.Skip > maxSkip {
			return CommitPage{}, invalid("page_token", "is invalid or was issued for another repository or path")
		}
		if _, ok, err := s.revParse(ctx, r.dir, c.Commit+"^{commit}"); err != nil || !ok {
			if err != nil {
				return CommitPage{}, err
			}
			return CommitPage{}, invalid("page_token", "is invalid or was issued for another repository or path")
		}
		cur = c
	} else {
		res, err := s.resolve(ctx, r, ref)
		if errors.Is(err, errEmpty) {
			return CommitPage{}, nil
		}
		if err != nil {
			return CommitPage{}, err
		}
		cur.Commit = res.commit
	}

	args := []string{"--skip=" + strconv.Itoa(cur.Skip), "--max-count=" + strconv.Itoa(size+1), cur.Commit}
	if filePath != "" {
		args = append(args, "--", filePath)
	}
	commits, err := s.logCommits(ctx, r.dir, args...)
	if err != nil {
		return CommitPage{}, err
	}
	page := CommitPage{Commits: commits}
	if len(commits) > size {
		page.Commits = commits[:size]
		cur.Skip += size
		page.NextPageToken = encodeToken(cur)
	}
	return page, nil
}

// GetCommit returns the commit named by sha (full, or a prefix of at least
// 7 characters) with its changes against its first parent.
func (s *Service) GetCommit(ctx context.Context, id uuid.UUID, sha string) (CommitDetail, error) {
	if len(sha) < 7 || len(sha) > 40 || !isLowerHex(sha) {
		return CommitDetail{}, invalid("sha", "must be 7 to 40 lowercase hex characters")
	}
	r, err := s.open(ctx, id)
	if err != nil {
		return CommitDetail{}, err
	}
	full, err := s.resolveCommit(ctx, r.dir, sha)
	if err != nil {
		return CommitDetail{}, err
	}
	c, err := s.readCommit(ctx, r.dir, full)
	if err != nil {
		return CommitDetail{}, err
	}

	// The two trees to compare: the first parent's and the commit's, or
	// the empty tree's for a root commit.
	trees := []string{"--root", full}
	if len(c.ParentSHAs) > 0 {
		trees = []string{c.ParentSHAs[0], full}
	}
	changes, stats, err := s.diffFiles(ctx, r.dir, trees)
	if err != nil {
		return CommitDetail{}, err
	}
	detail := CommitDetail{Commit: c, Stats: stats}
	if len(changes) > maxDiffFiles {
		changes, detail.Truncated = changes[:maxDiffFiles], true
	}
	if detail.Files, err = s.diffPatches(ctx, r.dir, trees, changes); err != nil {
		return CommitDetail{}, err
	}
	return detail, nil
}

// readCommit returns the commit with this full SHA.
func (s *Service) readCommit(ctx context.Context, dir, sha string) (Commit, error) {
	commits, err := s.logCommits(ctx, dir, "-1", sha)
	if err != nil {
		return Commit{}, err
	}
	if len(commits) != 1 {
		return Commit{}, fmt.Errorf("git log printed %d commits for %s", len(commits), sha)
	}
	return commits[0], nil
}

// resolveCommit turns a full or short SHA into the full SHA of a commit.
func (s *Service) resolveCommit(ctx context.Context, dir, sha string) (string, error) {
	// Without --quiet, so that git says when a short SHA is ambiguous.
	out, err := s.git.Run(ctx, dir, "rev-parse", "--verify", sha+"^{commit}")
	var gitErr *gitcmd.Error
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)), nil
	case errors.As(err, &gitErr) && strings.Contains(gitErr.Stderr, "is ambiguous"):
		return "", invalid("sha", "matches more than one commit; give more characters")
	case errors.As(err, &gitErr) && strings.Contains(gitErr.Stderr, "Needed a single revision"):
		return "", ErrCommitNotFound
	}
	return "", fmt.Errorf("resolve commit: %w", err)
}
