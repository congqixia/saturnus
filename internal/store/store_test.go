package store

import (
	"encoding/json"
	"errors"
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

	reviews := st.ListReviews("", false)
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

func TestStoreReviewRunsFlow(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{
		ThreadID: thread.ID, PRURL: "https://github.com/a/b/pull/1",
		Repo: "a/b", PRNumber: 1, RequesterOpenID: "ou_a",
	})
	if err != nil {
		t.Fatal(err)
	}

	run1, err := st.CreateReviewRun(ReviewRun{ReviewID: req.ID, Status: "running", RetryReason: "", Tool: "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if run1.Seq != 1 {
		t.Fatalf("expected first run seq 1, got %d", run1.Seq)
	}

	// Simulate a retry: another run rows in sequence while run1 stays terminal.
	run1.Status = "succeeded"
	run1.HeadCommit = "abc1234"
	run1.ResultText = "LGTM"
	run1.CompletedAt = time.Now().UTC()
	if err := st.UpdateReviewRun(run1); err != nil {
		t.Fatal(err)
	}

	run2, err := st.CreateReviewRun(ReviewRun{ReviewID: req.ID, Status: "running", RetryReason: "retry after failure"})
	if err != nil {
		t.Fatal(err)
	}
	if run2.Seq != 2 {
		t.Fatalf("expected second run seq 2, got %d", run2.Seq)
	}

	cur, err := st.CurrentRun(req.ID)
	if err != nil {
		t.Fatalf("expected current running run: %v", err)
	}
	if cur.ID != run2.ID || cur.RetryReason != "retry after failure" {
		t.Fatalf("unexpected current run: %#v", cur)
	}

	runs := st.ListRuns(req.ID)
	if len(runs) != 2 || runs[0].Seq != 1 || runs[1].Seq != 2 {
		t.Fatalf("unexpected run list: %#v", runs)
	}
	if runs[0].Status != "succeeded" || runs[0].HeadCommit != "abc1234" {
		t.Fatalf("run1 outcome not persisted: %#v", runs[0])
	}
}

func TestStoreFailInterruptedReviewsFailsRunningRuns(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{
		ThreadID: thread.ID, PRURL: "https://github.com/a/b/pull/1",
		Repo: "a/b", PRNumber: 1, RequesterOpenID: "ou_a", Status: "reviewing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReviewRun(ReviewRun{ReviewID: req.ID, Status: "running"}); err != nil {
		t.Fatal(err)
	}

	ids, err := st.FailInterruptedReviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != req.ID {
		t.Fatalf("expected interrupted review %q, got %#v", req.ID, ids)
	}

	runs := st.ListRuns(req.ID)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("expected run marked failed, got %#v", runs)
	}
}

func TestStoreReviewReactionFlow(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{
		ThreadID: thread.ID, PRURL: "https://github.com/a/b/pull/1",
		Repo: "a/b", PRNumber: 1, RequesterOpenID: "ou_a",
	})
	if err != nil {
		t.Fatal(err)
	}

	reac, err := st.CreateReaction(ReviewReaction{
		ReviewID: req.ID, Instruction: "post inline comments for issue 1", SessionID: "ses_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reac.ID == "" || reac.Status != "pending" {
		t.Fatalf("unexpected reaction: %#v", reac)
	}
	if _, err := st.CreateReaction(ReviewReaction{ReviewID: "rvw_missing", Instruction: "x"}); err == nil {
		t.Fatal("expected error for reaction on missing review")
	}

	reac2, err := st.CreateReaction(ReviewReaction{
		ReviewID: req.ID, Instruction: "give concrete code for issue 2",
		ChatID: "oc_c", SenderOpenID: "ou_r", SenderName: "reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}

	reac2.Status = "succeeded"
	reac2.ResultText = "done"
	reac2.CompletedAt = time.Now().UTC()
	if _, err := st.UpdateReaction(reac2); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetReaction(reac2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.ResultText != "done" || got.CompletedAt.IsZero() {
		t.Fatalf("reaction outcome not persisted: %#v", got)
	}
	if got.ChatID != "oc_c" || got.SenderOpenID != "ou_r" || got.SenderName != "reviewer" {
		t.Fatalf("reaction context not persisted: %#v", got)
	}

	reacs := st.ListReactions(req.ID)
	if len(reacs) != 2 || reacs[0].ID != reac.ID || reacs[1].ID != reac2.ID {
		t.Fatalf("unexpected reaction list: %#v", reacs)
	}

	// Only the still-pending reaction is picked up for re-enqueue.
	pending := st.ListPendingReactions()
	if len(pending) != 1 || pending[0].ID != reac.ID {
		t.Fatalf("unexpected pending reactions: %#v", pending)
	}
}

func TestStoreFailInterruptedReactions(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{
		ThreadID: thread.ID, PRURL: "https://github.com/a/b/pull/1",
		Repo: "a/b", PRNumber: 1, RequesterOpenID: "ou_a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReaction(ReviewReaction{ReviewID: req.ID, Instruction: "x", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	ids, err := st.FailInterruptedReactions()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 interrupted reaction, got %#v", ids)
	}
	reacs := st.ListReactions(req.ID)
	if len(reacs) != 1 || reacs[0].Status != "failed" {
		t.Fatalf("expected reaction marked failed, got %#v", reacs)
	}
}

func TestStoreReviewThreadLinks(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	original, err := st.GetOrCreateReviewThread("oc_alice", "om_root_a", "topic-a", []string{"ou_alice"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer, err := st.GetOrCreateReviewThread("oc_bob", "om_root_b", "topic-b", []string{"ou_bob"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(ReviewRequest{
		ThreadID: original.ID, PRURL: "https://github.com/a/b/pull/1",
		Repo: "a/b", PRNumber: 1, RequesterOpenID: "ou_alice",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Link the same thread twice: the pair is deduped.
	if err := st.LinkReviewThread(req.ID, original.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkReviewThread(req.ID, reviewer.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkReviewThread(req.ID, original.ID); err != nil {
		t.Fatal(err)
	}

	threads := st.ListReviewThreads(req.ID)
	if len(threads) != 2 {
		t.Fatalf("expected 2 linked threads, got %#v", threads)
	}
	seen := map[string]bool{}
	for _, t := range threads {
		seen[t.ID] = true
	}
	if !seen[original.ID] || !seen[reviewer.ID] {
		t.Fatalf("missing linked threads: original=%v reviewer=%v in %#v", seen[original.ID], seen[reviewer.ID], threads)
	}

	got, err := st.GetThread(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChatID != "oc_alice" || got.Topic != "topic-a" || len(got.Participants) != 1 {
		t.Fatalf("unexpected thread: %#v", got)
	}

	// Unrelated review has no links.
	req2, err := st.CreateReview(ReviewRequest{
		ThreadID: original.ID, PRURL: "https://github.com/a/b/pull/2",
		Repo: "a/b", PRNumber: 2, RequesterOpenID: "ou_a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if links := st.ListReviewThreads(req2.ID); len(links) != 0 {
		t.Fatalf("expected no links for unrelated review, got %#v", links)
	}
}

func TestStoreListReviewsFiltersArchived(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(ReviewRequest{
		ID: "rvw_active", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(ReviewRequest{
		ID: "rvw_arch", ThreadID: thread.ID, Status: "archived",
		PRURL: "https://github.com/a/b/pull/2", Repo: "a/b", PRNumber: 2,
		RequesterOpenID: "ou_a", ArchivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Default listings exclude archived reviews.
	active := st.ListReviews("", false)
	if len(active) != 1 || active[0].ID != "rvw_active" {
		t.Fatalf("expected only active review by default, got %#v", active)
	}
	// Explicitly including archived returns everything.
	all := st.ListReviews("", true)
	if len(all) != 2 {
		t.Fatalf("expected all reviews when includeArchived, got %#v", all)
	}
	// status=archived returns archived reviews regardless of the flag.
	archived := st.ListReviews("archived", false)
	if len(archived) != 1 || archived[0].ID != "rvw_arch" {
		t.Fatalf("expected archived review for status filter, got %#v", archived)
	}
	// A status filter still excludes archived reviews.
	succeeded := st.ListReviews("succeeded", false)
	if len(succeeded) != 1 || succeeded[0].ID != "rvw_active" {
		t.Fatalf("expected only active succeeded review, got %#v", succeeded)
	}
}

func TestChatModes(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if got := st.GetChatMode("oc_none"); got != ChatModeReview {
		t.Fatalf("default mode = %q, want review", got)
	}
	if got := st.GetChatMode(""); got != ChatModeReview {
		t.Fatalf("empty chat mode = %q, want review", got)
	}

	if _, err := st.SetChatMode("oc_a", ChatModeReport); err != nil {
		t.Fatal(err)
	}
	if got := st.GetChatMode("oc_a"); got != ChatModeReport {
		t.Fatalf("mode = %q, want report", got)
	}

	if _, err := st.SetChatMode("oc_b", ChatModeReview); err != nil {
		t.Fatal(err)
	}
	// Upsert overrides.
	if _, err := st.SetChatMode("oc_a", ChatModeReview); err != nil {
		t.Fatal(err)
	}
	if got := st.GetChatMode("oc_a"); got != ChatModeReview {
		t.Fatalf("mode after upsert = %q, want review", got)
	}

	modes := st.ListChatModes()
	if len(modes) != 2 {
		t.Fatalf("list len = %d, want 2", len(modes))
	}
	seen := map[string]bool{}
	for _, m := range modes {
		seen[m.ChatID] = true
	}
	if !seen["oc_a"] || !seen["oc_b"] {
		t.Fatalf("list = %#v, want oc_a and oc_b", modes)
	}
}

func TestChatModesCombinedFlags(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for input, want := range map[string]string{
		"report|review":   "report|review",
		"review|report":   "report|review",
		"report,review":   "report|review",
		"review | report": "report|review",
		"report":          "report",
		"review":          "review",
	} {
		if got, err := NormalizeChatMode(input); err != nil || got != want {
			t.Fatalf("NormalizeChatMode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}

	if _, err := NormalizeChatMode(""); err == nil {
		t.Fatal("expected error for empty mode")
	}
	if _, err := NormalizeChatMode("report|bogus"); err == nil {
		t.Fatal("expected error for unknown flag")
	}

	if _, err := st.SetChatMode("oc_mix", "review|report"); err != nil {
		t.Fatal(err)
	}
	if got := st.GetChatMode("oc_mix"); got != "report|review" {
		t.Fatalf("mode = %q, want report|review", got)
	}
	flags := ChatModeFlags(st.GetChatMode("oc_mix"))
	if !flags[ChatModeReport] || !flags[ChatModeReview] {
		t.Fatalf("flags = %#v, want both report and review", flags)
	}
}

func TestGetLatestReviewByThread(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_c", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(ReviewRequest{
		ID: "rvw_1", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// A second, older review in the same thread loses to the newest.
	if _, err := st.CreateReview(ReviewRequest{
		ID: "rvw_old", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/0", Repo: "a/b", PRNumber: 0,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetLatestReviewByThread("oc_c", "om_root")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "rvw_1" {
		t.Fatalf("got review %q, want rvw_1 (newest)", got.ID)
	}

	if _, err := st.GetLatestReviewByThread("oc_c", "om_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown thread root, got %v", err)
	}
}

func TestStoreTasksFlow(t *testing.T) {
	st, err := Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	task, err := st.CreateTask(Task{
		Title:       "Feature X",
		IssueRepo:   "a/b",
		IssueNumber: 10,
		IssueURL:    "https://github.com/a/b/issues/10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID == "" || task.Status != TaskStatusOpen {
		t.Fatalf("unexpected task: %#v", task)
	}

	// Attach a design doc.
	doc, err := st.AddTaskDoc(TaskDoc{TaskID: task.ID, Repo: "a/b", Path: "docs/plan/x.md"})
	if err != nil {
		t.Fatal(err)
	}
	// Adding the same doc path again dedupes.
	if _, err := st.AddTaskDoc(TaskDoc{TaskID: task.ID, Repo: "a/b", Path: "docs/plan/x.md", Title: "X"}); err != nil {
		t.Fatal(err)
	}
	docs := st.ListTaskDocs(task.ID)
	if len(docs) != 1 || docs[0].Title != "X" {
		t.Fatalf("unexpected docs: %#v", docs)
	}

	// Attach a PR with a linked review snapshot.
	pr, err := st.AddTaskPR(TaskPR{
		TaskID:       task.ID,
		Repo:         "a/b",
		PRNumber:     1,
		PRURL:        "https://github.com/a/b/pull/1",
		PRTitle:      "PR one",
		PRState:      "OPEN",
		ReviewID:     "rvw_1",
		ReviewStatus: "succeeded",
		FsTaskID:     "guid1",
		FsTaskURL:    "https://task",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same PR re-added updates the snapshot rather than duplicating.
	_, err = st.AddTaskPR(TaskPR{
		TaskID:       task.ID,
		Repo:         "a/b",
		PRNumber:     1,
		PRURL:        "https://github.com/a/b/pull/1",
		PRState:      "MERGED",
		ReviewID:     "rvw_1",
		ReviewStatus: "archived",
	})
	if err != nil {
		t.Fatal(err)
	}
	prs := st.ListTaskPRs(task.ID)
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %#v", prs)
	}
	if prs[0].PRState != "MERGED" || prs[0].ReviewStatus != "archived" || prs[0].FsTaskID != "guid1" {
		t.Fatalf("PR snapshot not updated: %#v", prs[0])
	}

	// Update and reload a task.
	task.Status = TaskStatusInProgress
	if _, err := st.UpdateTask(task); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskStatusInProgress || got.IssueNumber != 10 {
		t.Fatalf("task round-trip mismatch: %#v", got)
	}

	// Close archives it.
	closed, err := st.CloseTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != TaskStatusArchived || closed.ArchivedAt.IsZero() {
		t.Fatalf("expected archived task, got %#v", closed)
	}
	// Closing twice is idempotent.
	if _, err := st.CloseTask(task.ID); err != nil {
		t.Fatal(err)
	}

	if tasks := st.ListTasks("", false); len(tasks) != 0 {
		t.Fatalf("expected archived task hidden from default listing, got %#v", tasks)
	}
	if tasks := st.ListTasks("", true); len(tasks) != 1 {
		t.Fatalf("expected archived task with includeArchived, got %#v", tasks)
	}

	// Detach doc and PR.
	if err := st.RemoveTaskDoc(doc.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RemoveTaskPR(pr.ID); err != nil {
		t.Fatal(err)
	}
	if len(st.ListTaskDocs(task.ID)) != 0 || len(st.ListTaskPRs(task.ID)) != 0 {
		t.Fatalf("expected empty docs and prs after removal")
	}

	if _, err := st.GetTask("tsk_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown task, got %v", err)
	}
}
