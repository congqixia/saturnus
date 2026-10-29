package store

import (
	"context"
	"testing"
)

func TestPullRequestIdentityAndSnapshotRollback(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pr, err := st.TrackPullRequest(ctx, "A/B", 1)
	if err != nil {
		t.Fatal(err)
	}
	same, err := st.TrackPullRequest(ctx, "a/b", 1)
	if err != nil || pr.ID != same.ID {
		t.Fatalf("duplicate identity: %+v %v", same, err)
	}
	pr.HeadSHA, pr.CheckedHeadSHA, pr.CIState = "old", "old", "FAIL"
	checks := []PRCheck{{Key: "build", Name: "build", Bucket: "fail", HeadSHA: "old"}}
	if err = st.SavePullRequestSnapshot(ctx, pr, checks, true); err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA, pr.CIState = "new", "PASS"
	if err = st.SavePullRequestSnapshot(ctx, pr, []PRCheck{{Key: "duplicate"}, {Key: "duplicate"}}, true); err == nil {
		t.Fatal("expected duplicate key failure")
	}
	stored, err := st.GetPullRequest(ctx, pr.ID)
	if err != nil || stored.HeadSHA != "old" || stored.CIState != "FAIL" {
		t.Fatalf("partially saved metadata: %+v %v", stored, err)
	}
	got, err := st.PullRequestChecks(ctx, pr.ID)
	if err != nil || len(got) != 1 || got[0].Key != "build" {
		t.Fatalf("old snapshot lost: %+v %v", got, err)
	}
	pr.CIState, pr.SyncError = "UNKNOWN", "timeout"
	if err = st.SavePullRequestSnapshot(ctx, pr, nil, false); err != nil {
		t.Fatal(err)
	}
	got, err = st.PullRequestChecks(ctx, pr.ID)
	if err != nil || len(got) != 1 {
		t.Fatal("failed refresh discarded checks")
	}
}
