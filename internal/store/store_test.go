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
