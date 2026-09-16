package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStoreSessionEventApprovalFlow(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	agent, session, err := st.RegisterAgentSession(
		Agent{Name: "codex", Type: "codex", Host: "localhost", Version: "test"},
		Session{CWD: "/tmp/work", Repo: "repo", Branch: "main", Model: "gpt-test"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if agent.ID == "" || session.ID == "" {
		t.Fatalf("expected generated ids, got agent=%q session=%q", agent.ID, session.ID)
	}

	eventPayload := json.RawMessage(`{"prompt":"hello"}`)
	event, err := st.AddEvent(Event{
		SessionID: session.ID,
		Type:      "UserPromptSubmit",
		Payload:   eventPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.ID == "" {
		t.Fatal("expected generated event id")
	}

	gotSession, events, err := st.GetSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession.ID != session.ID {
		t.Fatalf("got session %q, want %q", gotSession.ID, session.ID)
	}
	if len(events) != 1 || string(events[0].Payload) != string(eventPayload) {
		t.Fatalf("unexpected events: %#v", events)
	}

	req, err := st.CreateApproval(ApprovalRequest{
		SessionID: session.ID,
		ToolName:  "exec_command",
		ToolInput: json.RawMessage(`{"cmd":"git status --short"}`),
		RiskLevel: "medium",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" {
		t.Fatalf("approval status = %q, want pending", req.Status)
	}

	decidedReq, decision, err := st.DecideApproval(req.ID, ApprovalDecision{
		Decision:  "approved",
		DecidedBy: "tester",
		Source:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decidedReq.Status != "approved" || decision.RequestID != req.ID {
		t.Fatalf("unexpected decision result: req=%#v decision=%#v", decidedReq, decision)
	}

	_, _, err = st.DecideApproval(req.ID, ApprovalDecision{Decision: "denied"})
	if err != ErrAlreadyDecided {
		t.Fatalf("second decision error = %v, want ErrAlreadyDecided", err)
	}
}

func TestStoreAutoPassAndHeartbeat(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	agent, session, err := st.RegisterAgentSession(
		Agent{Name: "codex", Type: "codex", Host: "localhost", Version: "test"},
		Session{CWD: "/tmp/work"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Heartbeat(agent.ID); err != nil {
		t.Fatal(err)
	}

	updated, err := st.UpdateAutoPass(session.ID, true, time.Minute, "test", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.AutoPass || updated.AutoPassUntil.IsZero() {
		t.Fatalf("expected enabled auto-pass with expiry, got %#v", updated)
	}

	updated, err = st.UpdateAutoPass(session.ID, false, time.Minute, "test", "tester")
	if err != nil {
		t.Fatal(err)
	}
	if updated.AutoPass || !updated.AutoPassUntil.IsZero() {
		t.Fatalf("expected disabled auto-pass with zero expiry, got %#v", updated)
	}
}

func TestStoreReviewFlow(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "topic-1", []string{"ou_a", "ou_b"})
	if err != nil {
		t.Fatal(err)
	}
	if thread.ID == "" || len(thread.Participants) != 2 {
		t.Fatalf("unexpected thread: %#v", thread)
	}

	updated, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "topic-2", []string{"ou_a", "ou_c"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != thread.ID {
		t.Fatalf("expected same thread id, got %q want %q", updated.ID, thread.ID)
	}
	if len(updated.Participants) != 3 {
		t.Fatalf("expected 3 merged participants, got %#v", updated.Participants)
	}

	req, err := st.CreateReview(ReviewRequest{
		ThreadID:        thread.ID,
		PRURL:           "https://github.com/congqixia/saturnus/pull/1",
		Repo:            "congqixia/saturnus",
		PRNumber:        1,
		RequesterOpenID: "ou_a",
		ChatID:          "oc_chat",
		MessageID:       "om_1",
		Tool:            "opencode",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.ID == "" || req.Status != "pending" {
		t.Fatalf("unexpected review request: %#v", req)
	}

	got, err := st.GetReview(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != req.Repo || got.PRNumber != req.PRNumber {
		t.Fatalf("review round-trip mismatch: %#v", got)
	}

	req.Status = "reviewing"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	req.ResultText = "LGTM"
	req.SessionID = "ses_x"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	byPR, err := st.GetReviewByPR("congqixia/saturnus", 1)
	if err != nil {
		t.Fatal(err)
	}
	if byPR.ID != req.ID || byPR.SessionID != "ses_x" || byPR.Status != "succeeded" {
		t.Fatalf("GetReviewByPR mismatch: %#v", byPR)
	}

	reviews := st.ListReviews("")
	if len(reviews) != 1 || reviews[0].Status != "succeeded" {
		t.Fatalf("unexpected reviews: %#v", reviews)
	}

	req.Status = "reviewing"
	req.ResultText = ""
	req.Error = ""
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	ids, err := st.FailInterruptedReviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 interrupted review, got %#v", ids)
	}
	after, err := st.GetReview(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "failed" {
		t.Fatalf("expected failed status, got %q", after.Status)
	}
}
