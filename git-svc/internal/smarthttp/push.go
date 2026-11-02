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

// recordTimeout bounds the work after git finished a push: finding forced
// updates and writing the event.
const recordTimeout = 30 * time.Second

// hookCheckTimeout bounds the startup run of the hook.
const hookCheckTimeout = 10 * time.Second

// installHooks writes git-svc's hooks into dir, replacing older versions.
// The file is written next to the hook and renamed over it, so a push
// never runs a half-written hook.
func installHooks(dir string) error {
	path := filepath.Join(dir, "post-receive")
	tmp := path + ".tmp"
	//nolint:gosec // G306: a hook must be executable.
	if err := os.WriteFile(tmp, []byte(postReceiveHook), hookPerm); err != nil {
		return fmt.Errorf("install post-receive hook: %w", err)
	}
	//nolint:gosec // G302: WriteFile keeps the mode of an existing file.
	if err := os.Chmod(tmp, hookPerm); err != nil {
		return fmt.Errorf("install post-receive hook: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install post-receive hook: %w", err)
	}
	return nil
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
// runs on a context of its own, even if the client is gone.
func (s *Server) recordPush(r *http.Request, id, pusher uuid.UUID, refsFile string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), recordTimeout)
	defer cancel()
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
	dir := s.opts.Layout.RepoPath(id)
	for i := range updates {
		updates[i].Forced = s.forced(ctx, dir, updates[i])
	}

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

// forced reports whether an update was not a fast-forward, that is, the
// old object is not an ancestor of the new one. Creating or deleting a ref
// is never forced.
func (s *Server) forced(ctx context.Context, dir string, u repo.RefUpdate) bool {
	if isZeroID(u.OldSHA) || isZeroID(u.NewSHA) {
		return false
	}
	_, err := s.opts.Git.Run(ctx, dir, "merge-base", "--is-ancestor", u.OldSHA, u.NewSHA)
	if err == nil {
		return false
	}
	var gitErr *gitcmd.Error
	if !errors.As(err, &gitErr) || gitErr.ExitCode != 1 {
		// Not commits (a tag moved from one tree to another, say), so there
		// is no ancestry to keep; or git failed. Either way it was no
		// fast-forward that git could prove.
		s.opts.Log.WarnContext(ctx, "check for a forced update", slog.String("ref", u.Ref), slog.String("err", err.Error()))
	}
	return true
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
