package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/reviewworker"
	"saturnus/internal/store"
	"saturnus/internal/worker"
	"saturnus/internal/workerproto"
)

type recordingWorkerHandler struct {
	worker.Handler
	mu     sync.Mutex
	starts []workerproto.StartedPayload
}

func (h *recordingWorkerHandler) OnEvent(ctx context.Context, id string, frame workerproto.Frame) error {
	if frame.Type == workerproto.EvStarted {
		var p workerproto.StartedPayload
		if err := frame.Decode(&p); err != nil {
			return err
		}
		h.mu.Lock()
		h.starts = append(h.starts, p)
		h.mu.Unlock()
	}
	return h.Handler.OnEvent(ctx, id, frame)
}

func (h *recordingWorkerHandler) hasStart(runID, reactionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.starts {
		if p.RunID == runID && p.ReactionID == reactionID {
			return true
		}
	}
	return false
}

// TestDistributedReviewEndToEnd exercises the full remote-worker flow: a worker
// registers, connects its WebSocket control channel, the scheduler dispatches a
// review to it, the worker runs a (fake) review tool that streams progress and
// POSTs a structured result, and the server records the completion on the right
// worker with the verdict intact.
func TestDistributedReviewEndToEnd(t *testing.T) {
	const repo = "owner/repo"

	// A fake checkout (git dir marker is enough; the tool never uses git).
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A fake review tool that prints a REVIEW SUMMARY and POSTs a structured
	// result to the result API URL it finds in its prompt args.
	tool := filepath.Join(t.TempDir(), "fake-review-tool")
	script := `#!/bin/bash
url=$(printf '%s\n' "$@" | grep -oE 'https?://[^ ]*/api/reviews/[^ ]*/result' | head -n1)
echo "FAKE-TOOL ARGS: $*"
echo "REVIEW SUMMARY:"
echo "PR summary: fake PR adds tests"
echo "Issues: None"
echo "Review suggestions: LGTM"
if [ -n "$url" ]; then
  curl -s -X POST "$url" -H 'Content-Type: application/json' -d '{"verdict":"lgtm","summary":"fake summary","findings":[]}' >/dev/null 2>&1
fi
exit 0
`
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Server-side handlers used by the worker (WS upgrade + result POST). The
	// reviews service is wired in below, before the worker starts.
	srv := &server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/workers/ws", srv.workerWS)
	mux.HandleFunc("POST /api/reviews/{review_id}/result", srv.submitReviewResult)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// The server itself has no checkout: only the remote worker can run it.
	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:       true,
		AllowedUsers:  []string{"*"},
		Tool:          tool,
		Repos:         map[string]string{},
		MaxConcurrent: 2,
		LogDir:        t.TempDir(),
		// The fake tool POSTs the structured result back here.
		ResultAPIURL: ts.URL + "/api/reviews/{review_id}/result",
	})
	srv.reviews = svc
	defer svc.Stop()

	sched := newScheduler(svc, st)
	sched.onlineWindow = 60 * time.Second
	svc.SetJobSink(sched.Notify)
	svc.SetRepoProvider(sched)
	recorder := &recordingWorkerHandler{Handler: sched}
	hub := worker.NewHub(recorder, func(workerID, token string) bool {
		return workerID == "wrk-test" && token == "test-token"
	})
	sched.SetHub(hub)
	srv.hub = hub
	defer hub.Close()

	// Register the remote worker with its capabilities and token.
	if _, err := st.RegisterWorker(store.Agent{ID: "wrk-test", Name: "review-worker", Type: "review-worker", Host: "test-host"},
		store.WorkerCapabilities{Repos: map[string]string{repo: checkout}, Tool: tool, MaxConcurrent: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWorkerToken("wrk-test", "test-token"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start()
	go sched.Run(ctx)

	// Start the worker daemon against the test server.
	wk := reviewworker.New(reviewworker.Config{
		ServerURL:     ts.URL,
		WorkerID:      "wrk-test",
		Token:         "test-token",
		Tool:          tool,
		Repos:         map[string]string{repo: checkout},
		GH:            false,
		MaxConcurrent: 1,
		LogDir:        t.TempDir(),
	})
	defer wk.Close()
	go func() {
		if err := wk.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("worker run: %v", err)
		}
	}()

	// Wait until the worker's control channel is live so the scheduler can
	// dispatch (RepoAvailable requires a healthy connection).
	waitFor(t, 5*time.Second, func() bool { return hub.Online("wrk-test") })

	created, err := svc.Submit(review.MessageContext{
		Text:          "https://github.com/" + repo + "/pull/1",
		ChatID:        "oc_test",
		MessageID:     "om_1",
		SenderOpenID:  "ou_user",
		SenderName:    "User",
		RootMessageID: "om_1",
	})
	if err != nil {
		t.Fatalf("submit review: %v", err)
	}
	if created.Status != "pending" {
		t.Fatalf("expected pending review, got %s", created.Status)
	}

	// Wait for the remote worker to run the fake tool and complete.
	var final store.ReviewRequest
	waitFor(t, 15*time.Second, func() bool {
		req, err := svc.Get(created.ID)
		if err != nil {
			return false
		}
		if req.Status == "succeeded" {
			final = req
			return true
		}
		return false
	})

	if final.Status != "succeeded" {
		t.Fatalf("review did not succeed: status=%s error=%s", final.Status, final.Error)
	}
	if final.WorkerID != "wrk-test" {
		t.Fatalf("review executed by %q, want wrk-test", final.WorkerID)
	}
	if final.Verdict != "lgtm" {
		t.Fatalf("verdict = %q, want lgtm (structured result must round-trip)", final.Verdict)
	}
	if final.Summary != "fake summary" {
		t.Fatalf("summary = %q, want fake summary", final.Summary)
	}
	if final.ResultText == "" || !contains(final.ResultText, "REVIEW SUMMARY") {
		t.Fatalf("result text missing summary block: %q", final.ResultText)
	}
	runs := svc.ListRuns(created.ID)
	if len(runs) != 1 || runs[0].WorkerID != "wrk-test" || runs[0].Status != "succeeded" {
		t.Fatalf("run record not persisted for worker: %#v", runs)
	}
	if !recorder.hasStart(runs[0].ID, "") {
		t.Fatal("worker did not report the actual review run starting")
	}
	// No lingering in-flight job on the worker.
	if n := hub.InFlightCount("wrk-test"); n != 0 {
		t.Fatalf("worker still has %d in-flight jobs", n)
	}

	// --- Reviewer reaction: resumes the tool session on the same worker. ---
	// Give the review a tool session (normally captured on the worker host) so
	// the reaction has something to resume.
	withSession, err := svc.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	withSession.SessionID = "ses_remote-1"
	if _, err := st.UpdateReview(withSession); err != nil {
		t.Fatal(err)
	}
	reac, err := svc.Act(review.MessageContext{
		ChatID:        "oc_test",
		MessageID:     "om_2",
		SenderOpenID:  "ou_user",
		SenderName:    "User",
		RootMessageID: "om_1",
	}, created.ID, "post inline comments for the P0 finding")
	if err != nil {
		t.Fatalf("act: %v", err)
	}
	var reacFinal store.ReviewReaction
	waitFor(t, 15*time.Second, func() bool {
		r, err := st.GetReaction(reac.ID)
		if err != nil {
			return false
		}
		if r.Status == "succeeded" {
			reacFinal = r
			return true
		}
		return false
	})
	if reacFinal.Status != "succeeded" {
		t.Fatalf("reaction did not succeed: status=%s error=%s", reacFinal.Status, reacFinal.Error)
	}
	if reacFinal.WorkerID != "wrk-test" {
		t.Fatalf("reaction executed by %q, want wrk-test", reacFinal.WorkerID)
	}
	if !recorder.hasStart("", reac.ID) {
		t.Fatal("worker did not report the reaction starting")
	}

	// --- Retry keeps session affinity: the re-run goes back to the same worker. ---
	retried, err := svc.Retry(created.ID, review.MessageContext{
		ChatID:       "oc_test",
		MessageID:    "om_3",
		SenderOpenID: "ou_user",
		SenderName:   "User",
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.Status != "pending" {
		t.Fatalf("expected retried review pending, got %s", retried.Status)
	}
	waitFor(t, 15*time.Second, func() bool {
		req, err := svc.Get(created.ID)
		return err == nil && req.Status == "succeeded"
	})
	final2, err := svc.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final2.WorkerID != "wrk-test" {
		t.Fatalf("retry executed by %q, want wrk-test (session affinity)", final2.WorkerID)
	}
	runs = svc.ListRuns(created.ID)
	if len(runs) != 2 || runs[1].WorkerID != "wrk-test" {
		t.Fatalf("retry run not recorded for worker: %#v", runs)
	}
	if !recorder.hasStart(runs[1].ID, "") {
		t.Fatal("retry did not report its distinct run id")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSchedulerLocalFallback verifies that with the scheduler installed but no
// registered remote workers, a review for a repo the server has checked out
// still runs on the server's own local executor (backwards compatibility).
func TestSchedulerLocalFallback(t *testing.T) {
	const repo = "owner/repo"
	checkout := t.TempDir()
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(t.TempDir(), "fake-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/bash\necho 'REVIEW SUMMARY:'\necho 'PR summary: fake'\necho 'Issues: None'\necho 'Review suggestions: LGTM'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:       true,
		AllowedUsers:  []string{"*"},
		Tool:          tool,
		Repos:         map[string]string{repo: checkout}, // server owns this checkout
		MaxConcurrent: 2,
		LogDir:        t.TempDir(),
	})
	defer svc.Stop()

	sched := newScheduler(svc, st)
	svc.SetJobSink(sched.Notify)
	svc.SetRepoProvider(sched)
	hub := worker.NewHub(sched, func(workerID, token string) bool { return false })
	sched.SetHub(hub)
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start()
	go sched.Run(ctx)

	created, err := svc.Submit(review.MessageContext{
		Text:          "https://github.com/" + repo + "/pull/1",
		ChatID:        "oc_test",
		MessageID:     "om_1",
		SenderOpenID:  "ou_user",
		SenderName:    "User",
		RootMessageID: "om_1",
	})
	if err != nil {
		t.Fatalf("submit review: %v", err)
	}

	var final store.ReviewRequest
	waitFor(t, 10*time.Second, func() bool {
		req, err := svc.Get(created.ID)
		if err != nil {
			return false
		}
		if req.Status == "succeeded" {
			final = req
			return true
		}
		return false
	})
	if final.Status != "succeeded" {
		t.Fatalf("local review did not succeed: status=%s error=%s", final.Status, final.Error)
	}
	if final.WorkerID != "" {
		t.Fatalf("local review unexpectedly executed by worker %q", final.WorkerID)
	}
	if runs := svc.ListRuns(created.ID); len(runs) != 1 || runs[0].WorkerID != "" {
		t.Fatalf("local run record wrong: %#v", runs)
	}
}
