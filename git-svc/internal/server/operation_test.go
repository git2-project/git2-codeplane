package server

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitv1 "github.com/git2-project/git2-codeplane/gen/go/git/v1"
	"github.com/git2-project/git2-codeplane/git-svc/internal/merge"
)

// The OperationService tests use branchFixture from diff_test.go.

func (f *branchFixture) mergeRequest(source, sha string, strategy gitv1.MergeStrategy) *gitv1.MergeBranchesRequest {
	return &gitv1.MergeBranchesRequest{
		RepositoryId:      f.repo.id.String(),
		SourceBranch:      source,
		TargetBranch:      "main",
		Strategy:          strategy,
		ExpectedSourceSha: sha,
		Message:           "Merge branch '" + source + "' into 'main'",
		Author:            &gitv1.CommitIdentity{Name: "Deniz", Email: "deniz@example.com"},
		MergerId:          "22222222-2222-4222-8222-222222222222",
	}
}

func TestCheckMergeRPC(t *testing.T) {
	f := newBranchFixture(t)
	check := func(source string) *gitv1.CheckMergeResponse {
		resp, err := f.ops.CheckMerge(t.Context(), &gitv1.CheckMergeRequest{RepositoryId: f.repo.id.String(), SourceBranch: source, TargetBranch: "main"})
		if err != nil {
			t.Fatalf("CheckMerge(%s): %v", source, err)
		}
		return resp
	}

	if resp := check("feature"); resp.GetStatus() != gitv1.MergeStatus_MERGE_STATUS_CLEAN || resp.GetCanFastForward() ||
		resp.GetSourceSha() != f.feat || resp.GetTargetSha() != f.main {
		t.Errorf("CheckMerge(feature) = %v, want clean, no fast-forward", resp)
	}
	if resp := check("conflict"); resp.GetStatus() != gitv1.MergeStatus_MERGE_STATUS_CONFLICTS ||
		len(resp.GetConflicts()) != 1 || resp.GetConflicts()[0].GetPath() != "f.txt" {
		t.Errorf("CheckMerge(conflict) = %v, want f.txt conflicting", resp)
	}

	_, err := f.ops.CheckMerge(t.Context(), &gitv1.CheckMergeRequest{RepositoryId: f.repo.id.String(), SourceBranch: "nope", TargetBranch: "main"})
	wantCode(t, "CheckMerge of an unknown branch", err, codes.NotFound)
	if status.Convert(err).Message() != "branch not found" {
		t.Errorf("message = %q, want %q", status.Convert(err).Message(), "branch not found")
	}
}

func TestMergeBranchesRPC(t *testing.T) {
	f := newBranchFixture(t)

	resp, err := f.ops.MergeBranches(t.Context(), f.mergeRequest("feature", f.feat, gitv1.MergeStrategy_MERGE_STRATEGY_MERGE_COMMIT))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOldTargetSha() != f.main || len(resp.GetNewTargetSha()) != 40 || resp.GetNewTargetSha() == f.main {
		t.Errorf("MergeBranches = %v, want main moved from %s", resp, f.main)
	}
	if n := f.repo.events(); n != 1 {
		t.Errorf("%d events, want 1", n)
	}

	// The same request again: already merged, with the reason in the detail.
	_, err = f.ops.MergeBranches(t.Context(), f.mergeRequest("feature", f.feat, gitv1.MergeStrategy_MERGE_STRATEGY_MERGE_COMMIT))
	if d := mergeErrorDetail(t, err); d.GetReason() != gitv1.MergeErrorReason_MERGE_ERROR_REASON_ALREADY_MERGED ||
		d.GetTargetSha() != resp.GetNewTargetSha() {
		t.Errorf("retry detail = %v, want ALREADY_MERGED at the new tip", d)
	}
}

func TestMergeBranchesRPCErrors(t *testing.T) {
	f := newBranchFixture(t)

	_, err := f.ops.MergeBranches(t.Context(), f.mergeRequest("conflict", f.confl, gitv1.MergeStrategy_MERGE_STRATEGY_MERGE_COMMIT))
	d := mergeErrorDetail(t, err)
	if d.GetReason() != gitv1.MergeErrorReason_MERGE_ERROR_REASON_CONFLICTS || d.GetSourceSha() != f.confl ||
		d.GetTargetSha() != f.main || len(d.GetConflicts()) != 1 || d.GetConflicts()[0].GetPath() != "f.txt" {
		t.Errorf("detail = %v, want CONFLICTS on f.txt", d)
	}

	_, err = f.ops.MergeBranches(t.Context(), f.mergeRequest("feature", f.feat, gitv1.MergeStrategy_MERGE_STRATEGY_FAST_FORWARD))
	if d := mergeErrorDetail(t, err); d.GetReason() != gitv1.MergeErrorReason_MERGE_ERROR_REASON_FAST_FORWARD_IMPOSSIBLE {
		t.Errorf("fast-forward detail = %v, want FAST_FORWARD_IMPOSSIBLE", d)
	}

	noStrategy := f.mergeRequest("feature", f.feat, gitv1.MergeStrategy_MERGE_STRATEGY_UNSPECIFIED)
	_, err = f.ops.MergeBranches(t.Context(), noStrategy)
	wantCode(t, "MergeBranches without a strategy", err, codes.InvalidArgument)

	badMerger := f.mergeRequest("feature", f.feat, gitv1.MergeStrategy_MERGE_STRATEGY_SQUASH)
	badMerger.MergerId = "nobody"
	_, err = f.ops.MergeBranches(t.Context(), badMerger)
	wantCode(t, "MergeBranches with a bad merger_id", err, codes.InvalidArgument)

	unknown := f.mergeRequest("nope", f.feat, gitv1.MergeStrategy_MERGE_STRATEGY_SQUASH)
	_, err = f.ops.MergeBranches(t.Context(), unknown)
	wantCode(t, "MergeBranches of an unknown branch", err, codes.NotFound)
}

// The target kept moving: ABORTED, which the caller may retry.
func TestTargetBusyIsAborted(t *testing.T) {
	err := toStatus(t.Context(), newTestLogger(t), merge.ErrTargetBusy)
	wantCode(t, "toStatus(ErrTargetBusy)", err, codes.Aborted)
}

// mergeErrorDetail returns the MergeError of a FAILED_PRECONDITION, as
// dash-api reads it.
func mergeErrorDetail(t *testing.T, err error) *gitv1.MergeError {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		t.Fatalf("error = %v, want FAILED_PRECONDITION", err)
	}
	for _, d := range st.Details() {
		if me, ok := d.(*gitv1.MergeError); ok {
			return me
		}
	}
	t.Fatalf("FAILED_PRECONDITION without a MergeError detail: %v", st.Details())
	return nil
}
