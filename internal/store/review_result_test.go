package store

import (
	"testing"
	"time"
)

func TestSaveReviewResultRollsBackVerdictWhenFindingsFail(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{ID: "rvw_atomic", ThreadID: thread.ID, Status: "reviewing", Repo: "a/b", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateReviewRun(ReviewRun{ReviewID: req.ID, Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	err = st.SaveReviewResult(req.ID, run.ID, "lgtm", "all good", []ReviewFinding{
		{Severity: "P1", Path: "a.go", Message: "must roll back"},
		{ID: "missing-finding", Status: "addressed"},
	})
	if err == nil {
		t.Fatal("expected invalid finding to fail")
	}
	req, _ = st.GetReview(req.ID)
	run, _ = st.CurrentRun(req.ID)
	if req.Verdict != "" || run.Verdict != "" || len(st.ListFindings(req.ID)) != 0 {
		t.Fatalf("partially saved result: review=%+v run=%+v", req, run)
	}
}

func TestCompleteReviewRunCommitsOrRollsBackBothStates(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{ID: "rvw_finish", ThreadID: thread.ID, Status: "reviewing", WorkerID: "worker", Repo: "a/b", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateReviewRun(ReviewRun{ReviewID: req.ID, Status: "running", WorkerID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	req.Status, req.ResultText, req.CompletedAt = "succeeded", "complete", time.Now().UTC()
	if applied, err := st.CompleteReviewRun(req, "old-run"); err != nil || applied {
		t.Fatalf("stale run applied=%v err=%v", applied, err)
	}
	stored, _ := st.GetReview(req.ID)
	if stored.Status != "reviewing" {
		t.Fatalf("stale completion changed the review: %+v", stored)
	}
	if applied, err := st.CompleteReviewRun(req, run.ID); err != nil || !applied {
		t.Fatalf("current run applied=%v err=%v", applied, err)
	}
	stored, _ = st.GetReview(req.ID)
	run, _ = st.LatestRun(req.ID)
	if stored.Status != "succeeded" || run.Status != "succeeded" || run.ResultText != stored.ResultText || !run.CompletedAt.Equal(stored.CompletedAt) {
		t.Fatalf("inconsistent completion: review=%+v run=%+v", stored, run)
	}
}
