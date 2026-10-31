package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func newRunner(t *testing.T) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	r, err := New(home)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r, home
}

func TestBaseEnvDropsParentGitVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/elsewhere")
	t.Setenv("GIT_CONFIG_GLOBAL", "/elsewhere/config")
	t.Setenv("LANG", "de_DE.UTF-8")

	env := baseEnv("/svc/home")

	for _, want := range []string{"HOME=/svc/home", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"} {
		if !slices.Contains(env, want) {
			t.Errorf("baseEnv() is missing %q: %q", want, env)
		}
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GIT_DIR" || k == "GIT_CONFIG_GLOBAL" || k == "LANG" {
			t.Errorf("baseEnv() inherited %q", kv)
		}
	}
}

func TestRunUsesServiceHome(t *testing.T) {
	r, home := newRunner(t)

	// A config pointed to by the parent env must be ignored...
	leaked := filepath.Join(t.TempDir(), "leaked.gitconfig")
	writeFile(t, leaked, "[user]\n\tname = leaked\n")
	t.Setenv("GIT_CONFIG_GLOBAL", leaked)
	// ...and the service-owned HOME must be used.
	writeFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = git-svc\n")

	out, err := r.Run(t.Context(), "", "config", "--global", "--get", "user.name")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "git-svc" {
		t.Errorf("user.name = %q, want %q", got, "git-svc")
	}
}

func TestRunErrorIncludesStderr(t *testing.T) {
	r, _ := newRunner(t)

	_, err := r.Run(t.Context(), t.TempDir(), "rev-parse", "--git-dir")

	var gitErr *Error
	if !errors.As(err, &gitErr) {
		t.Fatalf("Run() error = %v, want *Error", err)
	}
	if gitErr.ExitCode != 128 {
		t.Errorf("ExitCode = %d, want 128", gitErr.ExitCode)
	}
	if !strings.Contains(gitErr.Stderr, "not a git repository") {
		t.Errorf("Stderr = %q, want it to mention 'not a git repository'", gitErr.Stderr)
	}
	if !strings.Contains(err.Error(), gitErr.Stderr) {
		t.Errorf("Error() = %q, want it to include stderr", err)
	}
}

func TestRunCanceled(t *testing.T) {
	r, _ := newRunner(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := r.Run(ctx, "", "version")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v, want context.Canceled", err)
	}
}

func TestStreamPipesInputAndOutput(t *testing.T) {
	r, _ := newRunner(t)
	var out bytes.Buffer

	// hash-object reads the content from stdin and prints its object ID.
	err := r.Stream(t.Context(), StreamOptions{Stdin: strings.NewReader("hello\n"), Stdout: &out},
		"hash-object", "--stdin")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	// The well-known ID of a blob containing "hello\n".
	if got, want := strings.TrimSpace(out.String()), "ce013625030ba8dba906f756967f9e9ca394464a"; got != want {
		t.Errorf("hash-object = %q, want %q", got, want)
	}
}

func TestStreamAddsEnv(t *testing.T) {
	r, _ := newRunner(t)
	var out bytes.Buffer

	// GIT_CONFIG_COUNT and friends set config through the environment.
	env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=git2.probe", "GIT_CONFIG_VALUE_0=from-env"}
	err := r.Stream(t.Context(), StreamOptions{Env: env, Stdout: &out}, "config", "--get", "git2.probe")
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "from-env" {
		t.Errorf("git2.probe = %q, want %q", got, "from-env")
	}

	// The extra variables are per call: the Runner's own environment is
	// unchanged.
	if slices.Contains(r.env, env[0]) {
		t.Errorf("Stream() changed the Runner's environment: %q", r.env)
	}
}

func TestStreamErrorIncludesStderr(t *testing.T) {
	r, _ := newRunner(t)

	err := r.Stream(t.Context(), StreamOptions{Dir: t.TempDir(), Stdout: io.Discard}, "rev-parse", "--git-dir")

	var gitErr *Error
	if !errors.As(err, &gitErr) || gitErr.ExitCode != 128 || !strings.Contains(gitErr.Stderr, "not a git repository") {
		t.Errorf("Stream() error = %v, want *Error with exit code 128 and git's message", err)
	}
}

func TestStreamStopsWhenCanceled(t *testing.T) {
	r, _ := newRunner(t)
	ctx, cancel := context.WithCancel(t.Context())

	// hash-object waits for the end of its input, which never comes.
	pr, pw := io.Pipe()
	defer pw.Close()

	done := make(chan error, 1)
	go func() {
		done <- r.Stream(ctx, StreamOptions{Stdin: pr, Stdout: io.Discard}, "hash-object", "--stdin")
	}()
	time.Sleep(200 * time.Millisecond) // let git start and block on stdin
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Stream() error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stream() did not return after its context was canceled")
	}
}

func TestCheckVersion(t *testing.T) {
	r, _ := newRunner(t)

	v, err := r.CheckVersion(t.Context(), Version{Major: 2})
	if err != nil {
		t.Fatalf("CheckVersion(2.0.0) error = %v", err)
	}
	if v.Less(Version{Major: 2}) {
		t.Errorf("CheckVersion() = %v, want >= 2.0.0", v)
	}

	_, err = r.CheckVersion(t.Context(), Version{Major: 99})
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("CheckVersion(99.0.0) error = %v, want 'too old'", err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 5}
	for _, s := range []string{"abc", "defg", "hij"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got, want := b.String(), "abcde...(truncated)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
