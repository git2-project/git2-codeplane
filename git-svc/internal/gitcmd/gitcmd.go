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
	// StopStdin makes a blocked read from Stdin return. Stream calls it if a
	// read is still blocked a moment after git exited. Stdin needs it unless
	// its reads cannot block for long: Stream does not return during a read.
	StopStdin func()
	// Stdout receives git's standard output as git writes it. Its writes
	// must not block for long: Stream does not return during a write.
	Stdout io.Writer
}

// Stream runs git with args and streams its input and output, for commands
// such as upload-pack whose output can be too large to buffer.
//
// It has no timeout of its own: ctx bounds the call. When ctx ends, git
// gets SIGTERM, so that it can remove its temporary files, and is killed
// if it is still running after a short delay.
//
// On failure the error is an *Error. It wraps ctx.Err() if the call was
// canceled, and the read error if git's input was cut short, even if git
// then exited successfully. Git not reading all of its input is no failure.
//
// When Stream returns it no longer uses Stdin or Stdout, so the caller may
// inspect them or release what they read from.
func (r *Runner) Stream(ctx context.Context, opts StreamOptions, args ...string) error {
	stderr := &limitedBuffer{max: maxStderr}

	cmd := exec.CommandContext(ctx, r.bin, args...) //nolint:gosec // G204: fixed binary, explicit args, no shell.
	cmd.Dir = opts.Dir
	cmd.Env = append(slices.Clone(r.env), opts.Env...)
	cmd.Stdout = opts.Stdout
	cmd.Stderr = stderr
	cmd.Cancel = func() error { return interrupt(cmd.Process) }
	cmd.WaitDelay = waitDelay

	// With cmd.Stdin set, Wait would also wait for the copy from Stdin, and
	// would have no way to end a read that blocks; Stream does that itself.
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
	copied := make(chan error, 1)
	if stdin != nil {
		go func() { copied <- copyStdin(stdin, opts.Stdin) }()
	} else {
		copied <- nil
	}

	waitErr := cmd.Wait() // closes stdin once git has exited

	var readErr error
	select {
	case readErr = <-copied:
	case <-time.After(waitDelay):
		// Git did not need the rest of its input. The error of the read
		// stopped here says nothing about git, so it is dropped.
		if opts.StopStdin != nil {
			opts.StopStdin()
		}
		<-copied //nolint:errcheck // Stream stopped this read itself.
	}

	if waitErr == nil && readErr == nil {
		return nil
	}
	gitErr := newError(ctx, args, stderr, waitErr)
	switch {
	case readErr == nil:
	case gitErr.Err == nil: // git exited successfully on a shortened input
		gitErr.ExitCode = 0
		gitErr.Err = fmt.Errorf("read input: %w", readErr)
	default:
		gitErr.Err = fmt.Errorf("read input: %w (%w)", readErr, gitErr.Err)
	}
	return gitErr
}

// copyStdin copies src to git's standard input and closes it. It returns
// the error of a failed read. A failed write means git stopped reading; its
// exit status says whether that mattered.
func copyStdin(stdin io.WriteCloser, src io.Reader) error {
	in := &readErrorKeeper{r: src}
	_, _ = io.Copy(stdin, in)
	_ = stdin.Close()
	return in.err
}

// readErrorKeeper remembers why a read failed, which io.Copy cannot tell
// apart from a failed write.
type readErrorKeeper struct {
	r   io.Reader
	err error
}

func (k *readErrorKeeper) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		k.err = err
	}
	return n, err
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
	// When ctx ended, git was stopped and its exit status means nothing: on
	// Windows a killed process even reports exit code 1.
	if ctxErr := ctx.Err(); ctxErr != nil {
		gitErr.Err = ctxErr
		return gitErr
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
	ExitCode int // -1 if git did not exit normally; 0 if only its input failed
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
