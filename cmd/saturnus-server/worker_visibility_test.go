package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
	"saturnus/internal/worker"
	"saturnus/internal/workerproto"
)

func TestReviewCommandsShowWorkerAndHost(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.RegisterWorker(store.Agent{ID: "wrk-remote", Host: "review-host"}, store.WorkerCapabilities{}); err != nil {
		t.Fatal(err)
	}
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{ThreadID: thread.ID, Status: "reviewing", WorkerID: "wrk-remote", Repo: "owner/repo", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
	srv := &server{store: st, reviews: svc}
	for _, command := range []string{"/sat describe review " + req.ID, "/sat review " + req.ID} {
		reply := srv.handleBotCommand(command, review.MessageContext{})
		if !strings.Contains(reply.text, "worker=wrk-remote (host: review-host)") {
			t.Fatalf("%s omitted execution location: %s", command, reply.text)
		}
		if reply.md != nil && (!strings.Contains(reply.md.Content, "**Worker**:") || !strings.Contains(reply.md.Content, "review-host")) {
			t.Fatalf("card omitted execution location: %s", reply.md.Content)
		}
	}
	listed := srv.reviewsReply("")
	if len(listed.table.Rows) != 1 || listed.table.Rows[0]["worker"] != "wrk-remote" || !strings.Contains(listed.text, "worker=wrk-remote") {
		t.Fatalf("list omitted worker: %+v", listed)
	}
	for _, tc := range []struct{ status, workerID, want string }{
		{"pending", "wrk-remote", "unassigned (previous: wrk-remote)"},
		{"pending", "", "unassigned"},
		{"reviewing", "", "server (local)"},
		{"succeeded", "wrk-remote", "wrk-remote"},
	} {
		req.Status, req.WorkerID = tc.status, tc.workerID
		if got := svc.WorkerDescription(req); !strings.Contains(got, tc.want) {
			t.Fatalf("status=%s worker=%s label=%s want=%s", tc.status, tc.workerID, got, tc.want)
		}
		if _, err := st.UpdateReview(req); err != nil {
			t.Fatal(err)
		}
		reply := srv.describeReviewReply(req.ID)
		if !strings.Contains(reply.text, "worker="+tc.want) {
			t.Fatalf("wrong execution location: %s", reply.text)
		}
	}
}

func TestExecutionHistoryShowsEachWorkersIdentity(t *testing.T) {
	runs := formatReviewRuns([]store.ReviewRun{
		{Seq: 1, Status: "failed", WorkerID: "wrk-old"},
		{Seq: 2, Status: "running", WorkerID: "wrk-new"},
		{Seq: 3, Status: "succeeded"},
	})
	for _, want := range []string{"| Worker |", "wrk-old", "wrk-new", "server (local)"} {
		if !strings.Contains(runs, want) {
			t.Fatalf("run history omitted %q: %s", want, runs)
		}
	}
	reactions := formatReviewReactions([]store.ReviewReaction{
		{Status: "running", WorkerID: "wrk-action"}, {Status: "pending"},
	})
	for _, want := range []string{"| Worker |", "wrk-action", "unassigned"} {
		if !strings.Contains(reactions, want) {
			t.Fatalf("reaction history omitted %q: %s", want, reactions)
		}
	}
}

func TestStaleCompletionDoesNotClearCurrentWorkerAssignment(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("chat", "msg", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{ThreadID: thread.ID, Status: "reviewing", WorkerID: "wrk-1", Repo: "a/b", PRNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateReviewRun(store.ReviewRun{ReviewID: req.ID, Status: "running", WorkerID: "wrk-1", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
	sched := newScheduler(svc, st)
	hub := worker.NewHub(sched, func(string, string) bool { return true })
	defer hub.Close()
	sched.SetHub(hub)
	hub.AddInFlight("wrk-1", req.ID)
	for _, runID := range []string{"run-previous", run.ID} {
		if err := sched.OnEvent(context.Background(), "wrk-1", workerproto.NewFrame(workerproto.EvComplete, req.ID, workerproto.CompletePayload{RunID: runID, OK: true})); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetReview(req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if runID != run.ID {
			if got.Status != "reviewing" || hub.InFlightCount("wrk-1") != 1 {
				t.Fatal("stale completion consumed the current job")
			}
		} else if got.Status != "succeeded" || hub.InFlightCount("wrk-1") != 0 {
			t.Fatal("current completion was not recorded")
		}
	}
}
