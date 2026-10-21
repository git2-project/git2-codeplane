package gitcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
