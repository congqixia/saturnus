package main

import (
	"strings"
	"testing"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

func TestRequesterNotificationsCommand(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReviewForResult(t, st, "rvw_notice")
	srv := &server{store: st, reviews: review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})}
	command := func(text string) string { return srv.handleBotCommand(text, review.MessageContext{}).text }
	if got := command("/sat notifications rvw_notice"); !strings.Contains(got, "No requester result notifications") {
		t.Fatal(got)
	}
	run, err := st.CreateReviewRun(store.ReviewRun{ReviewID: "rvw_notice", Status: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ message, status, errText string }{{"first message\nFindings: 1", "sent", ""}, {"retry message\nFindings: 2", "failed", "delivery failed"}} {
		n, err := st.CreateRequesterNotification(store.ReviewRequesterNotification{ReviewID: "rvw_notice", RunID: run.ID, RequesterOpenID: "ou_requester", Message: tc.message})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FinishRequesterNotification(n.ID, tc.status, tc.errText); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		cmd       string
		fragments []string
	}{
		{"/sat notifications rvw_notice", []string{"1/2", "#1", run.ID, "ou_requester", "Status: sent", "first message\nFindings: 1", "/sat notifications rvw_notice 2"}},
		{"/sat notifications rvw_notice 2", []string{"2/2", "Status: failed", "Error: delivery failed", "retry message\nFindings: 2"}},
		{"/sat notifications", []string{"Usage:"}},
		{"/sat notifications rvw_notice 0", []string{"positive integer"}},
		{"/sat notifications rvw_notice bad", []string{"positive integer"}},
		{"/sat notifications rvw_notice 3", []string{"out of range"}},
		{"/sat notifications missing", []string{"Review not found"}},
	} {
		got := command(tc.cmd)
		for _, fragment := range tc.fragments {
			if !strings.Contains(got, fragment) {
				t.Fatalf("%s: missing %q in %q", tc.cmd, fragment, got)
			}
		}
	}
	if !isReviewRequest("/sat notifications rvw_notice") || !strings.Contains(helpText(), "/sat notifications <review_id> [page]") {
		t.Fatal("command missing from group routing or help")
	}
}
