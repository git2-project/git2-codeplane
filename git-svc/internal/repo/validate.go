package repo

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DefaultBranch is the branch HEAD points to when the caller names none.
const DefaultBranch = "main"

// Limits from the API contract (repository.proto).
const (
	maxNameLen        = 100  // characters; names are ASCII, so also bytes
	maxBranchLen      = 255  // bytes; also the usual file name limit for loose refs
	maxDescriptionLen = 2000 // characters (runes)
)

var nameChars = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateName checks a repository name:
//   - 1 to 100 characters from A-Z, a-z, 0-9, ".", "_" and "-";
//   - not starting with "." or "-" (this also rules out "." and "..");
//   - not ending with ".git" in any letter case, so URLs like
//     /owner/name.git stay unambiguous.
func ValidateName(name string) error {
	switch {
	case name == "":
		return invalid("name", "is required")
	case !nameChars.MatchString(name):
		return invalid("name", `may contain only letters, digits, ".", "_" and "-"`)
	case len(name) > maxNameLen:
		return invalid("name", fmt.Sprintf("must be at most %d characters", maxNameLen))
	case name[0] == '.' || name[0] == '-':
		return invalid("name", `must not start with "." or "-"`)
	case strings.HasSuffix(strings.ToLower(name), ".git"):
		return invalid("name", `must not end with ".git"`)
	}
	return nil
}

// ValidateDescription checks a repository description: valid UTF-8 and at
// most 2000 characters.
func ValidateDescription(description string) error {
	switch {
	case !utf8.ValidString(description):
		return invalid("description", "must be valid UTF-8")
	case utf8.RuneCountInString(description) > maxDescriptionLen:
		return invalid("description", fmt.Sprintf("must be at most %d characters", maxDescriptionLen))
	}
	return nil
}
