# 0017. Comparing and merging branches for merge requests

- Status: Proposed. It becomes Accepted when Dash (Bilge) approves the contract, since this is a contract change (NFR-28).
- Date: 2026-11-24
- Issue: #22. Implementation: #23 (compare) and #24 (merge).

## Context

WBS-8.1, the diff and merge engine, is on the critical path. Merge requests are Dash's (WBS-8.2), but every diff and every merge of one goes through git-svc:

| dash-api (draft, `contracts/dash-api/openapi.yaml`) | Requirement |
|---|---|
| `GET /repos/{owner}/{repo}/compare?base&head` | FR-38: see the commits and changes before opening a merge request |
| `GET .../merge-requests/{number}/diff` | FR-39: the commits and changes, updated on every push |
| `MergeRequest.mergeable` and the merge box | FR-43: show whether the merge request can be merged, and why not |
| `POST .../merge-requests/{number}/merge` `{strategy}` | FR-42: merge commit, squash or fast-forward; UC-18 E1–E3 |

The contract comes before the engine, so that dash-api can be built against it in parallel.

## Decision

**Two services**, as `repository.proto`'s header has planned since W1:
- `DiffService` (`diff.proto`) only reads.
- `OperationService` (`operation.proto`) changes branches.

Keeping writes in a service of their own makes every caller that can move a branch easy to see in review.

### DiffService.CompareRefs

- **Three-dot diff.** It answers with the commits in `merge_base..head` and the changes from the merge base to `head`, as `git diff base...head` shows them. That is what a merge would bring. Changes made on `base` after the merge base are left out, so a busy target branch does not make every merge request look larger. GitHub and GitLab show merge requests the same way.
- **Refs.** `base` and `head` take the forms of `commit.proto`: a branch, a tag, a full ref or a full SHA.
  - Both are required. An empty value does not mean the default branch here: a comparison needs both sides named.
  - SHAs let Dash compare a merge request's old and new heads.
- **`ahead_by` and `behind_by`.** These two counts come from one `git rev-list --left-right --count`. With them Dash can show that a branch is behind, and know in advance whether a fast-forward is possible.
- **Commits.** Newest first, like `ListCommits`. At most 250 are returned, with `commits_truncated` when there are more; GitHub's compare uses the same cap.
- **Changes.** The same `FileDiff`, `DiffStats` and limits as `GetCommit`: 300 files, and patches cut at 64 KiB per file and 1 MiB in total. Dash then renders both the same way, and #14 changes the limits for both at once.
- **Nothing to merge is an answer, not an error.** When `base` already contains `head`, the answer is `ahead_by = 0` with no commits and no files (UC-16 E1). When the two refs have no common history, there is no merge base, and the answer is `FAILED_PRECONDITION`.
- **Nothing is stored.** Every call reads the refs' current commits, so a push shows up on the next call (FR-39). The answer returns `base_sha`, `head_sha` and `merge_base_sha`; dash-api keeps them to anchor inline comments and to mark them outdated (FR-40).

### OperationService

**A query and a command.**
- `CheckMerge` answers the merge box's question, "can this be merged, and how?". A merge that is not possible is a normal answer, so it comes as a `MergeStatus`: `CLEAN`, `CONFLICTS`, `ALREADY_MERGED` or `UNRELATED`. The answer also has `can_fast_forward` and the conflicting files.
- `MergeBranches` does the merge. When it cannot, it fails: `FAILED_PRECONDITION` carrying a `MergeError` detail. That detail holds the reason, the branches' current SHAs and the conflicting files, so dash-api can map it without parsing error text.

**The name `MergeBranches`, not `Merge`.** buf's STANDARD lint requires the request and response messages of an RPC `Merge` to be called `MergeRequest` and `MergeResponse`. `MergeRequest` would clash with dash-api's merge request in every conversation and every generated client.

**Strategies:**
- `MERGE_COMMIT` always makes a commit with two parents, even when a fast-forward would be possible (`--no-ff`). The target's history then shows every merge request as one merge commit; this is GitHub's and GitLab's default.
- `SQUASH` makes one commit on top of the target, with all of the source's changes.
- `FAST_FORWARD` makes no commit. It is possible only when the target is an ancestor of the source.

**The source is pinned; the target is not.**
- `expected_source_sha` is required. It is the commit the reviewers saw. If the source has moved, nothing is merged (`SOURCE_MOVED`). GitHub's `sha` and GitLab's `sha` parameter of their merge APIs work the same way.
- There is no expected target SHA. UC-18 E3 asks that a merge whose target moved goes ahead when it is still clean. git-svc therefore merges again against the new tip.
- The branch moves with `git update-ref <ref> <new> <old>`, which refuses when the ref is no longer at `<old>`, so a push or merge that arrives at the same moment is never overwritten. If the target keeps moving after three tries, the answer is `ABORTED`, gRPC's code for a concurrency conflict that the caller may retry.

**No empty commits, and safe retries.**
- When the merged files equal the target's, there is nothing to merge, and the answer is `ALREADY_MERGED`.
- This also makes a retried call safe. A merge commit or a fast-forward that already happened contains the source. A squash that already happened has the same files. Either way the retry ends in `ALREADY_MERGED`, never in a second commit.
- The rule compares files, not commits, and holds for every strategy. A source whose commits add up to no change (a commit and its revert, or empty commits) is `ALREADY_MERGED` for a fast-forward too, although `CompareRefs` still counts those commits in `ahead_by`. A fast-forward there would change no file either, and one rule keeps `CheckMerge`'s single status true for all three strategies. Dash can show "nothing to merge" and offer to close the merge request; `ahead_by = 0` tells it that the source's commits are on the target as well.

**Who wrote the commit.**
- git-svc does not look users up; auth-svc owns them. dash-api sends a `CommitIdentity` with the name and email for `author`, and optionally a different `committer`.
- With a separate committer, a squash can credit the merge request's author as author and the merging user as committer.
- The commit time is git-svc's clock. That is why `CommitIdentity` is a new message and not `Signature`, whose `time` field would be ignored.
- `merger_id` becomes `pusher_id` of the event.

**The same event as a push.**
- After the target moves, `MergeBranches` writes `RepositoryPushed`, with one `RefUpdate` for the target and `forced = false`. ci-svc starts the target's pipeline with no change on its side (FR-51, UC-18 step 5).
- Like a push, the merge and its event cannot share a transaction. The loss window and its logging are those of ADR 0012.

**Who may merge.**
- dash-api checks the user's role, the branch rules and the approvals before it calls `MergeBranches` (FR-36). This follows the package rule that callers authorize.
- Merges do not run the push hook, so the push rules of WBS-7.1 (#25) do not apply to them. dash-api is the only gate for merges, and #25 must keep it that way.

**Validation.**
- Branch names follow ADR 0009.
- The source and the target must differ.
- `CommitIdentity` fields are required, at most 255 bytes of UTF-8, and must not contain `<`, `>`, a line break or NUL; git would refuse such a commit or record it wrongly.
- Each field must also have a character that git keeps. git drops spaces, control characters and `. , : ; " ' \` at both ends of a name or email (the "crud" of its `ident.c`). It refuses a name with nothing left, and records such an email as empty. Recent git keeps `.`, but git-svc counts it anyway, so the rule does not depend on the git version. Without this check, a name like `", "` would pass validation and fail in `commit-tree` as `INTERNAL`.
- At most 100 conflicting files are listed, with `conflicts_truncated` when there are more. Each one is a `ConflictFile` message, not a string, so fields such as the kind of conflict can be added later without breaking callers.

**Engine.** Only git plumbing on the bare repository, never a working tree, as risk R-13's mitigation says.
- **`CompareRefs`** lives in package `browse`, next to `GetCommit`.
  - It runs `merge-base` and `rev-list --left-right --count`.
  - The changes come from the same code as `GetCommit`'s, now shared by both, so the limits and the patch splitting cannot drift apart.
- **`CheckMerge` and `MergeBranches`** live in package `merge`.
  - `merge-tree --write-tree --name-only --no-messages -z` gives the merged tree, or the conflicting paths with exit status 1. It needs git 2.38, our minimum.
  - `commit-tree` writes the merge or squash commit. The names and emails go in its environment and the message on its standard input, so user input is never an argument.
  - `update-ref <ref> <new> <old>` moves the branch.
- **`CheckMerge` writes objects.** `merge-tree` stores the merged tree, and blobs with conflict markers for conflicting files, even when no commit is made.
  - Objects are content-addressed, and git skips an object that exists. Checking the same two tips again writes nothing new, so the objects grow with pushes to the branches, not with views of the merge request.
  - Nothing references them, so `git gc` prunes them once they are older than its expiry (two weeks by default). The maintenance job that ADR 0012 lists as a TODO must keep an expiry: `--prune=now` could remove a tree that a running `MergeBranches` is about to commit.
- **When the update is refused,** git-svc reads the branch again:
  - if it moved, the merge is computed again, up to three times, with a short pause in between;
  - if it did not move, git failed for another reason, which is an `INTERNAL` error, not `ABORTED`.
- **Two cases count as "already merged":** the source is an ancestor of the target, or the merged tree equals the target's tree.

**Limits.** A commit message is at most 64 KiB, without NUL; a final line break is added if it has none. Names and emails are at most 255 bytes each, with the rules under Validation.

**Not in v0:**
- Keeping a merge request's commits after its source branch is deleted. That needs `refs/merge-requests/<n>/head`, and it comes if Dash asks for it.
- Rebase merges, resolving conflicts in the browser, and merge options such as `-X ours`.
- Deleting the source branch after a merge (#26).
- Default commit messages. dash-api builds them, because it knows the merge request's number and title.

## Consequences

- **Dash (Bilge).** Approve or change the contract; then this ADR is Accepted. In dash-api:
  - `compareRefs` and the merge request diff call `CompareRefs`;
  - `MergeRequest.mergeable` and the merge box call `CheckMerge`;
  - the merge passes the source SHA that the reviewers saw (from `CheckMerge` or the merge request) as `expected_source_sha`, and maps `FAILED_PRECONDITION` and `ABORTED` to `409`, with the `MergeError` reason in the body.
- **Release.** After the merge, the contract is tagged `v0.5.0`, and git2's `contracts/git/README.md` pins it.
- **CI.** Nothing to do: a merge arrives as a `RepositoryPushed`.
- **#14.** Its patch limits also apply to `CompareRefs`.
- **#25.** The design of branch protection keeps merges under dash-api's check.
