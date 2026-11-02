package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

func TestGetReviewIncludesRequesterNotifications(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReviewForResult(t, st, "rvw_notice")
	n, err := st.CreateRequesterNotification(store.ReviewRequesterNotification{ReviewID: "rvw_notice", RequesterOpenID: "ou_test", Message: "Findings: 1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRequesterNotification(n.ID, "sent", ""); err != nil {
		t.Fatal(err)
	}
	srv := &server{reviews: review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})}
	req := httptest.NewRequest(http.MethodGet, "/api/reviews/rvw_notice", nil)
	req.SetPathValue("review_id", "rvw_notice")
	rr := httptest.NewRecorder()
	srv.getReview(rr, req)
	var body struct {
		Notifications []store.ReviewRequesterNotification `json:"requester_notifications"`
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("response: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Notifications) != 1 || body.Notifications[0].Message != "Findings: 1" || body.Notifications[0].Status != "sent" {
		t.Fatalf("notifications: %#v", body.Notifications)
	}
}

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
