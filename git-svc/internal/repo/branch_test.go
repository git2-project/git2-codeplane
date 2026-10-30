package repo

import (
	"errors"
	"strings"
	"testing"

	"github.com/git2-project/git2-codeplane/git-svc/internal/gitcmd"
)

// branchCases is shared by the table test and the comparison with git.
var branchCases = []struct {
	name  string
	valid bool
	// gitDiffers marks the cases where git-svc is deliberately stricter
	// than git, or that git cannot receive as an argument.
	gitDiffers bool
}{
	// Valid.
	{name: "main", valid: true},
	{name: "master", valid: true},
	{name: "feature/login", valid: true},
	{name: "user/name/topic", valid: true},
	{name: "release-1.0", valid: true},
	{name: "v1.0.0", valid: true},
	{name: "fix_123", valid: true},
	{name: "a", valid: true},
	{name: "a-", valid: true},
	{name: "a@b", valid: true},
	{name: "@", valid: true},
	{name: "x{y}", valid: true},
	{name: "a.lock.b", valid: true},
	{name: "HEADS", valid: true},
	{name: "head", valid: true},
	{name: "feature/über", valid: true},
	{name: "refs/heads/x", valid: true},
	{name: strings.Repeat("a", 255), valid: true},

	// Rejected by --branch on top of the ref rules.
	{name: "-main"},
	{name: "HEAD"},

	// Rejected by the ref rules.
	{name: ""},
	{name: "a..b"},
	{name: ".hidden"},
	{name: "feature/.x"},
	{name: "a.lock"},
	{name: "feature/x.lock"},
	{name: "/main"},
	{name: "main/"},
	{name: "a//b"},
	{name: "main."},
	{name: "a@{b"},
	{name: "a b"},
	{name: "a\tb"},
	{name: "a\nb"},
	{name: "a\x7fb"},
	{name: "a~b"},
	{name: "a^b"},
	{name: "a:b"},
	{name: "a?b"},
	{name: "a*b"},
	{name: "a[b"},
	{name: `a\b`},

	// Stricter than git: git has no length limit.
	{name: strings.Repeat("a", 256), gitDiffers: true},
	// A NUL byte cannot be passed to git as an argument.
	{name: "a\x00b", gitDiffers: true},
}

func TestValidateBranch(t *testing.T) {
	for _, tt := range branchCases {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBranch(tt.name)
			if got := err == nil; got != tt.valid {
				t.Errorf("ValidateBranch(%q) = %v, want valid=%v", tt.name, err, tt.valid)
			}
			var verr *ValidationError
			if err != nil && (!errors.As(err, &verr) || verr.Field != "default_branch") {
				t.Errorf("ValidateBranch(%q) error = %v, want a *ValidationError for default_branch", tt.name, err)
			}
		})
	}
}

// TestValidateBranchMatchesGit runs `git check-ref-format --branch` on every
// case and requires the same verdict as ValidateBranch (ADR 0009).
func TestValidateBranchMatchesGit(t *testing.T) {
	git, err := gitcmd.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() // not inside a repository, like git-svc's own checks

	for _, tt := range branchCases {
		if tt.gitDiffers {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			_, gitErr := git.Run(t.Context(), dir, "check-ref-format", "--branch", tt.name)
			gitValid := gitErr == nil
			if gitValid != tt.valid {
				t.Errorf("git check-ref-format --branch %q: valid=%v, but the table says %v (git: %v)", tt.name, gitValid, tt.valid, gitErr)
			}
		})
	}
}
