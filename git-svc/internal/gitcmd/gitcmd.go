// Package gitcmd runs the git binary safely.
//
// Every call goes through exec.CommandContext, never through a shell. The
// arguments are passed explicitly, the environment is rebuilt from scratch
// and each call is bounded: Run by a timeout, Stream by its context.
//
// Callers must never put user input in option position. Validate it first,
// and attach it to its flag ("--initial-branch="+b) or pass it after "--".
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// DefaultTimeout bounds every git call unless the Runner is configured
// otherwise.
const DefaultTimeout = 30 * time.Second

// maxStderr is how much of git's stderr is kept for error messages.
const maxStderr = 4 << 10 // 4 KiB

// waitDelay is how long a call waits for git's pipes to close after the
// process was stopped, or after it exited. It covers grandchildren that
// inherit the pipes, and an input that is still being read.
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
		return nil, newError(ctx, args, stderr, err)
	}
	return stdout.Bytes(), nil
}

// StreamOptions are the input and output of a Stream call.
type StreamOptions struct {
	// Dir is git's working directory; empty means the process's.
	Dir string
	// Env is added to the Runner's environment, for example
	// "GIT_PROTOCOL=version=2".
	Env []string
	// Stdin is copied to git's standard input; nil means empty input.
	Stdin io.Reader
	// Stdout receives git's standard output as git writes it.
	Stdout io.Writer
}

// Stream runs git with args and streams its input and output, for commands
// such as upload-pack whose output can be too large to buffer.
//
// It has no timeout of its own: ctx bounds the call. When ctx ends, git
// gets SIGTERM, so that it can remove its temporary files, and is killed
// if it is still running after a short delay. On failure the error is an
// *Error, and it wraps ctx.Err() if the call was canceled.
//
// Stream returns soon after git exits even if a read from Stdin is still
// blocked, such as a request body from a stalled client; that read then
// ends in the background when its owner closes it.
func (r *Runner) Stream(ctx context.Context, opts StreamOptions, args ...string) error {
	stderr := &limitedBuffer{max: maxStderr}

	cmd := exec.CommandContext(ctx, r.bin, args...) //nolint:gosec // G204: fixed binary, explicit args, no shell.
	cmd.Dir = opts.Dir
	cmd.Env = append(slices.Clone(r.env), opts.Env...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return interrupt(cmd.Process) }
	cmd.WaitDelay = waitDelay

	// With cmd.Stdin set, Wait would also wait for the copy from Stdin, for
	// as long as a read blocks; this copy is waited for only briefly.
	var stdin io.WriteCloser
	if opts.Stdin != nil {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			return newError(ctx, args, stderr, err)
		}
	}
	if err := cmd.Start(); err != nil {
		return newError(ctx, args, stderr, err)
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		if stdin != nil {
			// A failed write means git stopped reading; its exit status
			// says whether that mattered.
			_, _ = io.Copy(stdin, opts.Stdin)
			_ = stdin.Close()
		}
	}()

	err := cmd.Wait() // closes stdin once git has exited
	select {
	case <-copied:
	case <-time.After(waitDelay):
	}
	if err != nil {
		return newError(ctx, args, stderr, err)
	}
	return nil
}

// interrupt asks git to stop. Git removes its temporary files (lock files,
// a push's quarantined objects) on SIGTERM, but not when killed. Windows
// has no SIGTERM, so there the process is killed at once.
func interrupt(p *os.Process) error {
	if runtime.GOOS == "windows" {
		return p.Kill()
	}
	return p.Signal(syscall.SIGTERM)
}

// newError builds the *Error for a failed call.
func newError(ctx context.Context, args []string, stderr *limitedBuffer, err error) *Error {
	gitErr := &Error{Args: args, ExitCode: -1, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	if ctxErr := ctx.Err(); ctxErr != nil {
		gitErr.Err = ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		gitErr.ExitCode = exitErr.ExitCode()
	}
	return gitErr
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
