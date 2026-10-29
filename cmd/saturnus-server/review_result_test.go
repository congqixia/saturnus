package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

func TestReviewResultHTTPRunFenceAndDuplicate(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReviewForResult(t, st, "rvw_bound")
	run, err := st.CreateReviewRun(store.ReviewRun{ReviewID: "rvw_bound", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
	srv := &server{reviews: svc}
	for _, tc := range []struct {
		runID string
		want  int
	}{{"old-run", http.StatusConflict}, {run.ID, http.StatusOK}, {run.ID, http.StatusConflict}} {
		req := httptest.NewRequest(http.MethodPost, "/api/reviews/rvw_bound/result?run_id="+tc.runID, strings.NewReader(`{"verdict":"lgtm","summary":"complete","findings":[]}`))
		req.SetPathValue("review_id", "rvw_bound")
		rr := httptest.NewRecorder()
		srv.submitReviewResult(rr, req)
		if rr.Code != tc.want {
			t.Fatalf("run_id=%s response=%d %s, want %d", tc.runID, rr.Code, rr.Body.String(), tc.want)
		}
	}
}
