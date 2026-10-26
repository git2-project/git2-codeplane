package repo

import (
	"fmt"
	"strings"
)

// ValidateBranch checks a branch name with the rules of
// `git check-ref-format --branch <name>` (run outside a repository), plus a
// length limit of 255 bytes.
//
// It runs in-process instead of calling git: that saves a subprocess per
// request and keeps user input out of git's argument list. A test compares
// it with the real git on every case in its table (ADR 0009).
func ValidateBranch(name string) error {
	if reason := branchProblem(name); reason != "" {
		return invalid("default_branch", reason)
	}
	return nil
}

// branchProblem returns why name is not a valid branch name, or "".
func branchProblem(name string) string {
	switch {
	case name == "":
		return "must not be empty"
	case len(name) > maxBranchLen:
		return fmt.Sprintf("must be at most %d bytes", maxBranchLen)
	// The next two are what --branch adds on top of the ref rules.
	case name[0] == '-':
		return `must not start with "-"`
	case name == "HEAD":
		return `must not be "HEAD"`
	}
	return refnameProblem(name)
}

// badRefChars are the printable characters git never allows in a ref name.
// Control characters (below 0x20, and 0x7f) are rejected separately.
const badRefChars = " ~^:?*[\\"

// refnameProblem applies git's ref name rules (git help check-ref-format)
// to the part of a ref after "refs/heads/". The prefix itself always
// passes, so checking the rest is enough.
func refnameProblem(name string) string {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c == 0x7f || strings.IndexByte(badRefChars, c) >= 0 {
			return `must not contain spaces, control characters or any of ~ ^ : ? * [ \`
		}
	}
	switch {
	case strings.Contains(name, ".."):
		return `must not contain ".."`
	case strings.Contains(name, "@{"):
		return `must not contain "@{"`
	case strings.HasSuffix(name, "."):
		return `must not end with "."`
	}
	for _, part := range strings.Split(name, "/") {
		switch {
		case part == "":
			return `must not start or end with "/" or contain "//"`
		case part[0] == '.':
			return `no part between slashes may start with "."`
		case strings.HasSuffix(part, ".lock"):
			return `no part between slashes may end with ".lock"`
		}
	}
	return ""
}
