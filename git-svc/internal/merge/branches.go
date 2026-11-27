package merge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// ErrTargetBusy means the target branch kept moving while MergeBranches tried
// to update it. Nothing changed; the caller may try again.
var ErrTargetBusy = errors.New("the target branch kept moving")

// Limits from the API contract (operation.proto).
const (
	maxMessage      = 64 << 10
	maxIdentityPart = 255
)

// maxAttempts is how often MergeBranches merges again when the target moved
// before it could be updated.
const maxAttempts = 3

// recordTimeout bounds writing the event of a merge, as for a push.
const recordTimeout = 30 * time.Second

// Strategy is how MergeBranches combines the branches.
type Strategy int

// Strategies of a Request.
const (
	StrategyMergeCommit Strategy = iota + 1 // a commit with the target and the source as parents
	StrategySquash                          // a commit on the target with all of the source's changes
	StrategyFastForward                     // the target moves to the source; no commit
)

// Identity is the name and email recorded in a new commit.
type Identity struct {
	Name  string
	Email string
}

// Request is a merge of SourceBranch into TargetBranch.
type Request struct {
	RepositoryID      uuid.UUID
	SourceBranch      string
	TargetBranch      string
	Strategy          Strategy
	ExpectedSourceSHA string    // the commit the reviewers saw
	Message           string    // for StrategyMergeCommit and StrategySquash
	Author            *Identity // for StrategyMergeCommit and StrategySquash
	Committer         *Identity // optional: the author when nil
	MergerID          uuid.UUID // becomes the pusher of the event
}

// Result is where the target branch was and is.
type Result struct {
	OldTargetSHA string
	NewTargetSHA string
}

// Reason is why MergeBranches did not merge.
type Reason int

// Reasons of a Refusal.
const (
	ReasonConflicts Reason = iota + 1
	ReasonFastForwardImpossible
	ReasonSourceMoved
	ReasonAlreadyMerged
	ReasonUnrelated
)

func (r Reason) String() string {
	switch r {
	case ReasonConflicts:
		return "conflicts"
	case ReasonFastForwardImpossible:
		return "fast-forward impossible"
	case ReasonSourceMoved:
		return "source moved"
	case ReasonAlreadyMerged:
		return "already merged"
	case ReasonUnrelated:
		return "no common history"
	default:
		return "unknown"
	}
}

// Refusal is a merge that is not possible now. Nothing changed.
type Refusal struct {
	Reason             Reason
	SourceSHA          string // where the source branch points now
	TargetSHA          string // where the target branch points now
	Conflicts          []string
	ConflictsTruncated bool
}

func (r *Refusal) Error() string {
	return "merge not possible: " + r.Reason.String()
}

// MergeBranches merges req.SourceBranch into req.TargetBranch with
// req.Strategy, moves the target branch to the result and records the move
// as a push. A merge that is not possible returns a *Refusal; a target that
// kept moving returns ErrTargetBusy. Either way nothing changed.
func (s *Service) MergeBranches(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	dir, err := s.open(ctx, req.RepositoryID)
	if err != nil {
		return Result{}, err
	}
	src, err := s.branchTip(ctx, dir, req.SourceBranch)
	if err != nil {
		return Result{}, err
	}
	if src != req.ExpectedSourceSHA {
		tgt, err := s.branchTip(ctx, dir, req.TargetBranch)
		if err != nil {
			return Result{}, err
		}
		return Result{}, &Refusal{Reason: ReasonSourceMoved, SourceSHA: src, TargetSHA: tgt}
	}

	// The source is read once: every attempt merges src, the commit the
	// reviewers saw, even if the source branch moves meanwhile.
	for attempt := 1; ; attempt++ {
		tgt, err := s.branchTip(ctx, dir, req.TargetBranch)
		if err != nil {
			return Result{}, err
		}
		next, err := s.prepare(ctx, dir, req, src, tgt)
		if err != nil {
			return Result{}, err
		}

		moved, err := s.updateRef(ctx, dir, req.TargetBranch, next, tgt)
		if err != nil {
			return Result{}, err
		}
		if moved {
			s.record(ctx, req, tgt, next)
			return Result{OldTargetSHA: tgt, NewTargetSHA: next}, nil
		}
		// The target moved after it was read: merge again with its new tip
		// (UC-18 E3), a few times at most.
		if attempt == maxAttempts {
			return Result{}, ErrTargetBusy
		}
		if err := pause(ctx, attempt); err != nil {
			return Result{}, err
		}
	}
}

// prepare computes the merge of commit src into commit tgt and returns the
// commit the target should move to: a new merge or squash commit, or src
// for a fast-forward. It does not move any branch.
func (s *Service) prepare(ctx context.Context, dir string, req Request, src, tgt string) (string, error) {
	c, err := s.analyze(ctx, dir, src, tgt)
	if err != nil {
		return "", err
	}
	refuse := func(r Reason) error {
		return &Refusal{Reason: r, SourceSHA: src, TargetSHA: tgt, Conflicts: c.Conflicts, ConflictsTruncated: c.ConflictsTruncated}
	}
	switch c.Status {
	case StatusUnrelated:
		return "", refuse(ReasonUnrelated)
	case StatusAlreadyMerged:
		return "", refuse(ReasonAlreadyMerged)
	case StatusConflicts:
		return "", refuse(ReasonConflicts)
	}

	switch req.Strategy {
	case StrategyFastForward:
		if !c.CanFastForward {
			return "", refuse(ReasonFastForwardImpossible)
		}
		return src, nil
	case StrategyMergeCommit:
		return s.commitTree(ctx, dir, c.tree, []string{tgt, src}, req)
	default: // StrategySquash; validate rejected anything else
		return s.commitTree(ctx, dir, c.tree, []string{tgt}, req)
	}
}

// commitTree writes a commit of tree with parents, req's message and
// identities, and git-svc's clock as its time. It returns the commit's ID.
func (s *Service) commitTree(ctx context.Context, dir, tree string, parents []string, req Request) (string, error) {
	committer := req.Author
	if req.Committer != nil {
		committer = req.Committer
	}
	date := fmt.Sprintf("@%d +0000", s.now().Unix())
	env := []string{
		"GIT_AUTHOR_NAME=" + req.Author.Name,
		"GIT_AUTHOR_EMAIL=" + req.Author.Email,
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + committer.Name,
		"GIT_COMMITTER_EMAIL=" + committer.Email,
		"GIT_COMMITTER_DATE=" + date,
	}
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	message := req.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	// The message goes on standard input, so it is never an argument.
	out, err := s.run(ctx, dir, env, []byte(message), args...)
	if err != nil {
		return "", fmt.Errorf("write merge commit: %w", err)
	}
	return parseSHA("commit-tree", out)
}

// updateRef moves branch from old to next. moved is false, with no error,
// when the branch no longer points to old: a push or another merge got
// there first, and nothing changed.
func (s *Service) updateRef(ctx context.Context, dir, branch, next, old string) (moved bool, err error) {
	_, err = s.git.Run(ctx, dir, "update-ref", "refs/heads/"+branch, next, old)
	if err == nil {
		return true, nil
	}
	now, tipErr := s.branchTip(ctx, dir, branch)
	switch {
	case errors.Is(tipErr, ErrBranchNotFound):
		return false, ErrBranchNotFound // deleted meanwhile
	case tipErr != nil:
		return false, fmt.Errorf("update branch: %w", err)
	case now != old:
		return false, nil
	}
	// The branch is still at old, so git failed for another reason, such
	// as a lock left by an update that is still running.
	return false, fmt.Errorf("update branch: %w", err)
}

// record writes the RepositoryPushed event of a merge that moved the
// target. The merge has happened, so the event is written even if the
// caller is gone; if it cannot be written, the loss is logged, as for a
// push (ADR 0012).
func (s *Service) record(ctx context.Context, req Request, old, next string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err := s.repos.RecordPush(ctx, repo.Push{
		RepositoryID: req.RepositoryID,
		PusherID:     req.MergerID,
		RefUpdates: []repo.RefUpdate{{
			Ref: "refs/heads/" + req.TargetBranch, OldSHA: old, NewSHA: next, Forced: repo.ForcedNo,
		}},
	})
	log := s.log.With(slog.String("repository_id", req.RepositoryID.String()))
	switch {
	case errors.Is(err, repo.ErrNotFound):
		log.InfoContext(ctx, "repository deleted during the merge; no RepositoryPushed event")
	case err != nil:
		log.ErrorContext(ctx, "merge not recorded: RepositoryPushed is lost",
			slog.String("branch", req.TargetBranch), slog.String("new_sha", next), slog.String("err", err.Error()))
	}
}

// pause waits a little before the next attempt, longer each time.
func pause(ctx context.Context, attempt int) error {
	t := time.NewTimer(time.Duration(attempt) * 50 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// validate checks a request before anything is read.
func (r Request) validate() error {
	if err := checkBranches(r.SourceBranch, r.TargetBranch); err != nil {
		return err
	}
	if r.Strategy < StrategyMergeCommit || r.Strategy > StrategyFastForward {
		return invalid("strategy", "must be set")
	}
	if !isFullSHA(r.ExpectedSourceSHA) {
		return invalid("expected_source_sha", "must be a full 40-character lowercase SHA")
	}
	if r.Strategy == StrategyFastForward {
		return nil // no commit, so no message or identity
	}
	switch {
	case strings.TrimSpace(r.Message) == "":
		return invalid("message", "is required for a merge commit or a squash")
	case len(r.Message) > maxMessage:
		return invalid("message", fmt.Sprintf("must be at most %d bytes", maxMessage))
	case !utf8.ValidString(r.Message) || strings.IndexByte(r.Message, 0) >= 0:
		return invalid("message", "must be valid UTF-8 without NUL")
	}
	if r.Author == nil {
		return invalid("author", "is required for a merge commit or a squash")
	}
	if err := r.Author.validate("author"); err != nil {
		return err
	}
	if r.Committer != nil {
		return r.Committer.validate("committer")
	}
	return nil
}

// validate checks a name and email that git will record in a commit header.
// Every value it accepts is one that commit-tree accepts too.
func (id Identity) validate(field string) error {
	for _, part := range []struct{ name, value string }{{"name", id.Name}, {"email", id.Email}} {
		f := field + "." + part.name
		switch v := part.value; {
		case strings.TrimSpace(v) == "":
			return invalid(f, "is required")
		case len(v) > maxIdentityPart:
			return invalid(f, fmt.Sprintf("must be at most %d bytes", maxIdentityPart))
		case !utf8.ValidString(v) || strings.IndexByte(v, 0) >= 0:
			return invalid(f, "must be valid UTF-8 without NUL")
		case strings.ContainsAny(v, "<>\n\r"):
			return invalid(f, "must not contain <, > or a line break")
		case strings.TrimFunc(v, isCrud) == "":
			// git would refuse such a name, and record such an email as empty.
			return invalid(f, `must contain a character other than spaces, control characters and . , : ; " ' \`)
		}
	}
	return nil
}

// isCrud reports whether git drops r at the start and the end of a name or
// email (the "crud" of git's ident.c). Recent versions of git keep "."; it
// counts here anyway, so a value made only of dots is refused whatever the
// git version.
func isCrud(r rune) bool {
	return r <= ' ' || strings.ContainsRune(`.,:;<>"'\`, r)
}

func invalid(field, reason string) error {
	return &repo.ValidationError{Field: field, Reason: reason}
}
