package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
	"saturnus/internal/worker"
)

func recoveryScheduler(t *testing.T) (*scheduler, *store.Store, *worker.Hub, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
	sched := newScheduler(svc, st)
	svc.SetJobSink(sched.Notify)
	hub := worker.NewHub(sched, func(string, string) bool { return true })
	sched.SetHub(hub)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = hub.ServeWS(w, r) }))
	t.Cleanup(func() { hub.Close(); ts.Close(); st.Close() })
	if _, err := st.RegisterWorker(store.Agent{ID: "worker"}, store.WorkerCapabilities{Repos: map[string]string{"a/b": "/checkout"}, MaxConcurrent: 1}); err != nil {
		t.Fatal(err)
	}
	return sched, st, hub, ts
}

func TestSchedulerDoesNotClaimRecentlySeenDisconnectedWorker(t *testing.T) {
	sched, st, _, _ := recoveryScheduler(t)
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{ID: "rvw_offline", ThreadID: thread.ID, Status: "pending", Repo: "a/b", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		sched.dispatchReview(req.ID)
	}
	req, _ = st.GetReview(req.ID)
	if req.Status != "pending" || len(st.ListRuns(req.ID)) != 0 {
		t.Fatalf("disconnected worker generated run attempts: %+v", req)
	}
}

func TestSchedulerWaitsForBusyOrBackingOffWorker(t *testing.T) {
	sched, _, hub, ts := recoveryScheduler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+ts.URL[4:]+"?worker_id=worker&token=test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "test complete")
	waitFor(t, time.Second, func() bool { return hub.Online("worker") })
	req := store.ReviewRequest{Repo: "a/b"}
	if id, _ := sched.pickReviewWorker(req); id != "worker" {
		t.Fatalf("online worker not selected: %q", id)
	}
	hub.AddInFlight("worker", "other-review")
	if id, wait := sched.pickReviewWorker(req); id != "" || !wait {
		t.Fatalf("busy worker should keep review pending: worker=%q wait=%v", id, wait)
	}
	hub.ClearInFlight("worker", "other-review")
	sched.backoffWorker("worker")
	if id, wait := sched.pickReviewWorker(req); id != "" || !wait {
		t.Fatalf("backoff should keep review pending: worker=%q wait=%v", id, wait)
	}
	for i := 0; i < 10; i++ {
		sched.backoffWorker("worker")
	}
	sched.backoffMu.Lock()
	b := sched.backoffs["worker"]
	if b.delay != 30*time.Second {
		t.Errorf("backoff is not capped: %s", b.delay)
	}
	b.until = time.Now().Add(-time.Second)
	sched.backoffs["worker"] = b
	sched.backoffMu.Unlock()
	if id, _ := sched.pickReviewWorker(req); id != "worker" {
		t.Fatalf("worker not selected after backoff elapsed: %q", id)
	}
}

func TestReviewMarkdownShowsHistoricalFindingsWithoutVerdict(t *testing.T) {
	md := formatReviewMarkdown(store.ReviewRequest{Status: "failed"}, "opencode", []store.ReviewFinding{{Severity: "P1", Path: "old.go", Message: "existing finding", Status: "addressed", RunID: "run_old"}}, []store.ReviewRun{{ID: "run_old", Seq: 2}}, "local")
	if !contains(md, "Previous findings") || !contains(md, "Findings: 1") || !contains(md, "addressed=1") || contains(md, "existing finding") {
		t.Fatalf("missing historical findings: %s", md)
	}
}
