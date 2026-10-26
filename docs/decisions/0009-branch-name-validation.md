# 0009. Branch name validation

- Status: Accepted
- Date: 2026-10-22

## Context

`CreateRepository` takes an optional `default_branch`. The brief asks to validate it with git's ref-format rules, either through `git check-ref-format --branch` or with an equivalent check.

## Decision

- **In-process check.** `repo.ValidateBranch` implements the rules of `git check-ref-format --branch <name>` as git applies them outside a repository:
  - not empty, not starting with `-`, not `HEAD`;
  - no `..`, no `@{`, no ASCII control characters, and none of space `~ ^ : ? * [ \`;
  - not starting or ending with `/` and no `//`;
  - no slash-separated part starting with `.` or ending with `.lock`;
  - not ending with `.`.
- **One extra rule:** at most 255 bytes. Git has no limit, but loose ref files are subject to the usual file name limit.
- **Defaults:** a missing `default_branch` means `main`; an empty string is invalid.
- **Cross-check test.** `TestValidateBranchMatchesGit` runs the real `git check-ref-format --branch` on every case of the table and requires the same verdict (42 cases). The only cases it skips are the deliberate differences: the length limit, and a NUL byte, which cannot be passed to git as an argument.

## Consequences

- No subprocess per request, and user input never reaches git's argument list for validation. Git still gets the name later as `--initial-branch=<name>`, but only after it passed the check.
- If a future git version changes its rules, the cross-check test fails and shows which case differs.
- Some unusual names are accepted because git accepts them, for example `@` and non-ASCII names like `feature/über`.
