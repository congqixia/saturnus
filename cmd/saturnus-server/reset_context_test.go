package main

import (
	"strings"
	"testing"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

func TestResetReviewCommand(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{ThreadID: thread.ID, Status: "succeeded", SessionID: "ses-expired", HeadCommit: "unchanged", Repo: "owner/repo", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true, ReviewerOpenID: "reviewer"})
	queued := 0
	svc.SetJobSink(func(kind, id string) {
		if kind != "review" || id != req.ID {
			t.Errorf("wrong job %s %s", kind, id)
		}
		queued++
	})
	srv := &server{store: st, reviews: svc}
	if reply := srv.handleBotCommand("/sat reset", review.MessageContext{}); !strings.Contains(reply.text, "Usage:") {
		t.Fatalf("missing usage: %s", reply.text)
	}
	if reply := srv.handleBotCommand("/sat reset "+req.ID, review.MessageContext{SenderOpenID: "outsider"}); !strings.Contains(reply.text, "not the reviewer") {
		t.Fatalf("unauthorized reset: %s", reply.text)
	}
	reply := srv.handleBotCommand("/sat reset "+req.ID, review.MessageContext{SenderOpenID: "reviewer"})
	if !strings.Contains(reply.text, "Review context reset:") || queued != 1 {
		t.Fatalf("reset failed: %s jobs=%d", reply.text, queued)
	}
	got, _ := st.GetReview(req.ID)
	if got.Status != "pending" || got.SessionID != "" || got.HeadCommit != "unchanged" {
		t.Fatalf("wrong reset: %+v", got)
	}
	description := srv.describeReviewReply(req.ID)
	if !strings.Contains(description.text, "context=ctx_") || !strings.Contains(description.md.Content, "**Context**:") {
		t.Fatal("reset context missing from describe")
	}
	if !isReviewRequest("/sat reset "+req.ID) || !strings.Contains(helpText(), "/sat reset <review_id>") {
		t.Fatal("reset command missing from routing/help")
	}
}
