// Package gitcmd runs the git binary safely.
//
// Every call goes through exec.CommandContext, never through a shell. The
// arguments are passed explicitly, the environment is rebuilt from scratch
// and each call has a timeout.
//
// Callers must never put user input in option position. Validate it first,
// and attach it to its flag ("--initial-branch="+b) or pass it after "--".
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// DefaultTimeout bounds every git call unless the Runner is configured
// otherwise.
const DefaultTimeout = 30 * time.Second

// maxStderr is how much of git's stderr is kept for error messages.
const maxStderr = 4 << 10 // 4 KiB

// waitDelay is how long Run waits for git's output pipes to close after the
// process is killed. It covers grandchildren that inherit the pipes.
const waitDelay = 2 * time.Second

// Runner runs git commands with a sanitized environment.
type Runner struct {
	bin     string
	env     []string
	timeout time.Duration
}

// New finds git on PATH and returns a Runner that uses home as git's HOME.
// The directory must be owned by git-svc, so that no user-level git config
// changes git's behavior.
func New(home string) (*Runner, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("find git binary: %w", err)
	}
	return &Runner{bin: bin, env: baseEnv(home), timeout: DefaultTimeout}, nil
}

// Path returns the absolute path of the git binary.
func (r *Runner) Path() string {
	return r.bin
}

// Run runs git with args in dir (the process working directory if dir is
// empty) and returns its stdout. On failure the error is an *Error, and it
// wraps ctx.Err() if the call was canceled or timed out.
func (r *Runner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout bytes.Buffer
	stderr := &limitedBuffer{max: maxStderr}

	cmd := exec.CommandContext(ctx, r.bin, args...) //nolint:gosec // G204: fixed binary, explicit args, no shell.
	cmd.Dir = dir
	cmd.Env = r.env
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = waitDelay

	if err := cmd.Run(); err != nil {
		gitErr := &Error{Args: args, ExitCode: -1, Stderr: strings.TrimSpace(stderr.String()), Err: err}
		if ctxErr := ctx.Err(); ctxErr != nil {
			gitErr.Err = ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			gitErr.ExitCode = exitErr.ExitCode()
		}
		return nil, gitErr
	}
	return stdout.Bytes(), nil
}

// Error describes a failed git call. Stderr and Args can contain filesystem
// paths, so an Error must never be shown to API clients.
type Error struct {
	Args     []string
	ExitCode int // -1 if git did not exit normally
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func (e *Error) Unwrap() error {
	return e.Err
}

// baseEnv is the complete environment for git. Nothing else is inherited
// from the parent, so stray GIT_DIR, GIT_CONFIG_* or locale settings cannot
// change behavior.
func baseEnv(home string) []string {
	env := []string{
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	keep := []string{"PATH"}
	if runtime.GOOS == "windows" {
		// Git for Windows needs these to start child processes and use temp files.
		keep = append(keep, "SYSTEMROOT", "TEMP", "TMP")
	}
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// limitedBuffer keeps the first max bytes written to it and drops the rest.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "...(truncated)"
	}
	return b.buf.String()
}
