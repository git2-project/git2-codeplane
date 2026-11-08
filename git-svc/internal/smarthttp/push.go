package smarthttp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
	"github.com/git2-project/git2-codeplane/git-svc/internal/repo"
)

// envPushedRefs names the file that the post-receive hook writes the
// updated refs to. git-svc sets it for each push.
const envPushedRefs = "GIT2_PUSHED_REFS"

// postReceiveHook is git-svc's post-receive hook. Git runs it after a push
// updated refs, with one "<old> <new> <ref>" line per updated ref on
// stdin; rejected refs are not in the list.
const postReceiveHook = `#!/bin/sh
# Installed by git-svc and rewritten at every start: do not edit.
# Copies the refs a push updated to the file git-svc named in
# GIT2_PUSHED_REFS; git-svc then writes the RepositoryPushed event.
[ -n "$GIT2_PUSHED_REFS" ] || exit 0
exec cat > "$GIT2_PUSHED_REFS"
`

// hookPerm makes the hook executable for git-svc's user.
const hookPerm = 0o750

// recordTimeout bounds writing the event of a push.
const recordTimeout = 30 * time.Second

// hookCheckTimeout bounds the startup run of the hook.
const hookCheckTimeout = 10 * time.Second

// installHooks writes git-svc's hooks into dir, replacing older versions.
// The file is written under a name of its own and renamed over the hook, so a
// push never runs a half-written hook, even if two git-svc processes start on
// one storage root at the same moment. Git ignores the temporary name if a
// crash leaves the file behind.
func installHooks(dir string) error {
	if err := writeHook(dir, "post-receive", postReceiveHook); err != nil {
		return fmt.Errorf("install post-receive hook: %w", err)
	}
	return nil
}

func writeHook(dir, name, content string) (err error) {
	f, err := os.CreateTemp(dir, name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	_, err = f.WriteString(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	//nolint:gosec // G302: a hook must be executable; CreateTemp makes files 0600.
	if err := os.Chmod(tmp, hookPerm); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// checkHooks runs the installed hook once, without GIT2_PUSHED_REFS, so it
// exits at once. Git skips a hook it cannot execute and still accepts the
// push: on a storage root mounted noexec, every push would go unrecorded
// and nothing would say why. Failing here makes that a startup error.
//
// On Windows there is no noexec, and git runs hooks through its own sh,
// so the check is skipped.
func checkHooks(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookCheckTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, filepath.Join(dir, "post-receive")) //nolint:gosec // G204: git-svc's own hook.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("run the post-receive hook in %s: %w; the storage root must allow executing files (not mounted noexec)", dir, err)
	}
	return nil
}

// newRefsFile creates the empty file that the hook fills during one push.
func (s *Server) newRefsFile() (string, error) {
	f, err := os.CreateTemp(s.opts.Layout.TmpDir(), "push-*.refs")
	if err != nil {
		return "", fmt.Errorf("create pushed-refs file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("create pushed-refs file: %w", err)
	}
	return f.Name(), nil
}

func (s *Server) removeRefsFile(path string) {
	if err := os.Remove(path); err != nil {
		s.opts.Log.Warn("remove pushed-refs file", slog.String("err", err.Error()))
	}
}

// recordPush writes the RepositoryPushed event for the refs a push
// updated, if any. git has finished, so the push has happened: the work
// runs on contexts of its own, even if the client is gone.
func (s *Server) recordPush(r *http.Request, id, pusher uuid.UUID, refsFile string) {
	ctx := context.WithoutCancel(r.Context())
	log := s.opts.Log.With(slog.String("repository_id", id.String()))

	data, err := os.ReadFile(refsFile) //nolint:gosec // G304: a file git-svc created under its storage root.
	if err != nil {
		log.ErrorContext(ctx, "push not recorded: read pushed refs", slog.String("err", err.Error()))
		return
	}
	updates, err := parsePushedRefs(string(data))
	if err != nil {
		log.ErrorContext(ctx, "push not recorded", slog.String("err", err.Error()))
		return
	}
	if len(updates) == 0 {
		return // every ref was rejected, or the push changed nothing
	}
	s.checkForced(ctx, log, s.opts.Layout.RepoPath(id), updates)

	// A full budget of its own: a slow check must not cost the event.
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	err = s.opts.Pushes.RecordPush(ctx, repo.Push{RepositoryID: id, PusherID: pusher, RefUpdates: updates})
	switch {
	case errors.Is(err, repo.ErrNotFound):
		log.InfoContext(ctx, "repository deleted during the push; no RepositoryPushed event")
	case err != nil:
		// The refs are updated, but consumers such as ci-svc will not hear
		// about this push.
		log.ErrorContext(ctx, "push not recorded: RepositoryPushed is lost",
			slog.Int("ref_updates", len(updates)), slog.String("err", err.Error()))
	}
}

// parsePushedRefs parses the hook's output: one "<old> <new> <ref>" line
// per updated ref.
func parsePushedRefs(data string) ([]repo.RefUpdate, error) {
	var updates []repo.RefUpdate
	for line := range strings.Lines(data) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		oldSHA, rest, ok1 := strings.Cut(line, " ")
		newSHA, ref, ok2 := strings.Cut(rest, " ")
		if !ok1 || !ok2 || !isObjectID(oldSHA) || !isObjectID(newSHA) || !strings.HasPrefix(ref, "refs/") {
			return nil, fmt.Errorf("unexpected line from the post-receive hook: %q", line)
		}
		updates = append(updates, repo.RefUpdate{Ref: ref, OldSHA: oldSHA, NewSHA: newSHA})
	}
	return updates, nil
}

// checkForced sets Forced on each update, within a time budget of its own.
// Every update of an existing ref needs a git call, and a push with --mirror
// can have thousands. When the budget runs out, the remaining updates are
// repo.ForcedUnknown and the event is still written.
func (s *Server) checkForced(ctx context.Context, log *slog.Logger, dir string, updates []repo.RefUpdate) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.ForcedCheckTimeout)
	defer cancel()

	for i := range updates {
		updates[i].Forced = s.forced(ctx, log, dir, updates[i])
	}
	if ctx.Err() != nil {
		log.WarnContext(ctx, "the check for forced updates ran out of time; the event leaves the rest unknown",
			slog.Int("ref_updates", len(updates)), slog.Duration("budget", s.opts.ForcedCheckTimeout))
	}
}

// forced reports whether an update was not a fast-forward, that is, the old
// object is not an ancestor of the new one. Creating or deleting a ref is
// never forced.
func (s *Server) forced(ctx context.Context, log *slog.Logger, dir string, u repo.RefUpdate) repo.Forced {
	if isZeroID(u.OldSHA) || isZeroID(u.NewSHA) {
		return repo.ForcedNo
	}
	_, err := s.opts.Git.Run(ctx, dir, "merge-base", "--is-ancestor", u.OldSHA, u.NewSHA)
	if err == nil {
		return repo.ForcedNo
	}
	var gitErr *gitcmd.Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return repo.ForcedYes
	}
	// The objects are not commits (a tag moved from one tree to another, say),
	// git failed, or the budget ran out, which checkForced logs once.
	if ctx.Err() == nil {
		log.WarnContext(ctx, "check for a forced update", slog.String("ref", u.Ref), slog.String("err", err.Error()))
	}
	return repo.ForcedUnknown
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object ID in
// lowercase hex.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isZeroID reports whether an object ID means "no object".
func isZeroID(s string) bool {
	return strings.Trim(s, "0") == ""
}
