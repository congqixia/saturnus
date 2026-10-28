package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

func TestResetContextForcesFreshRunAndPreservesHistory(t *testing.T) {
	st := refreshStore(t)
	newReviewForRetry(t, st, "rvw_reset", "succeeded", "same-head", "ses-expired", "")
	oldRun, err := st.CreateReviewRun(store.ReviewRun{ReviewID: "rvw_reset", Status: "succeeded", SessionID: "ses-expired", WorkerID: "wrk-old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFindings("rvw_reset", oldRun.ID, []store.ReviewFinding{{Severity: "P1", Path: "file.go", Line: 1, Message: "existing finding"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReaction(store.ReviewReaction{ReviewID: "rvw_reset", Status: "succeeded", SessionID: "ses-expired"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetReviewResult("rvw_reset", "request_change", "old summary"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCommentsPosted("rvw_reset", time.Now()); err != nil {
		t.Fatal(err)
	}
	svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, ReviewerOpenID: "ou_reviewer"})
	queued := []string{}
	svc.SetJobSink(func(kind, id string) { queued = append(queued, kind+":"+id) })
	before, _ := st.GetReview("rvw_reset")
	req, err := svc.ResetContext("rvw_reset", MessageContext{SenderOpenID: "ou_reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" || req.SessionID != "" || req.Verdict != "" || req.Summary != "" || !req.CompletedAt.IsZero() || !req.CommentsPostedAt.IsZero() {
		t.Fatalf("reset incomplete: %+v", req)
	}
	if req.RequesterOpenID != before.RequesterOpenID || req.ThreadID != before.ThreadID || req.HeadCommit != before.HeadCommit {
		t.Fatal("reset changed PR/requester/thread metadata")
	}
	contextID, err := svc.ContextID(req.ID)
	if err != nil || contextID == "" {
		t.Fatalf("context not persisted: %q %v", contextID, err)
	}
	payload, err := svc.JobPayload(req)
	if err != nil || payload.SessionID != "" || payload.ContextID != contextID || !strings.Contains(payload.PriorFindings, "existing finding") {
		t.Fatalf("fresh payload=%+v err=%v", payload, err)
	}
	if len(queued) != 1 || queued[0] != "review:rvw_reset" {
		t.Fatalf("wrong queued job: %v", queued)
	}
	runs := st.ListRuns(req.ID)
	if len(runs) != 1 || runs[0].SessionID != "ses-expired" || len(st.ListFindings(req.ID)) != 1 || st.ListReactions(req.ID)[0].SessionID != "ses-expired" {
		t.Fatal("reset lost historical context")
	}
	if _, err := svc.ResetContext(req.ID, MessageContext{SenderOpenID: "ou_reviewer"}); !errors.Is(err, store.ErrReviewContextBusy) {
		t.Fatalf("active reset allowed: %v", err)
	}
}

func TestResetSessionDiscoveryNeverFallsBackToExpiredContext(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ title, dir, want string }{
		{SessionTitle("rvw", ""), root, ""},
		{SessionTitle("rvw", "ctx-old"), root, ""},
		{SessionTitle("rvw", "ctx-new"), t.TempDir(), ""},
		{SessionTitle("rvw", "ctx-new"), root, "candidate"},
	} {
		data, _ := json.Marshal([]map[string]any{
			{"id": "candidate", "title": tc.title, "directory": tc.dir, "updated": 1},
			{"id": "ses-expired", "title": SessionTitle("rvw", ""), "directory": root, "updated": 999},
		})
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "sessions.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		tool := filepath.Join(dir, "tool")
		if err := os.WriteFile(tool, []byte("#!/bin/sh\ncat '"+filepath.Join(dir, "sessions.json")+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
		if got := CaptureContextSessionID(context.Background(), tool, root, "rvw", "ctx-new"); got != tc.want {
			t.Fatalf("title=%s dir=%s got=%s want=%s", tc.title, tc.dir, got, tc.want)
		}
	}
}

func TestResetContextPersistsAndRotatesAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	newReviewForRetry(t, st, "rvw_persist", "failed", "head", "ses-old", "")
	req, first, err := st.ResetReviewContext("rvw_persist", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if id, err := st.ReviewContextID(req.ID); err != nil || id != first {
		t.Fatalf("context lost on restart: %s %v", id, err)
	}
	req, _ = st.GetReview(req.ID)
	req.Status = "failed"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	_, second, err := st.ResetReviewContext(req.ID, "reviewer")
	if err != nil || second == "" || second == first {
		t.Fatalf("second reset reused a context: %s %v", second, err)
	}
}

func TestResetContextRequiresAnUpgradedRemoteWorker(t *testing.T) {
	st := refreshStore(t)
	newReviewForRetry(t, st, "rvw_cap", "failed", "head", "ses-old", "")
	svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, ReviewerOpenID: "reviewer"})
	svc.SetJobSink(func(string, string) {})
	req, err := svc.ResetContext("rvw_cap", MessageContext{SenderOpenID: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterWorker(store.Agent{ID: "worker"}, store.WorkerCapabilities{Repos: map[string]string{"a/b": "/checkout"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartRemote(req, "worker"); err == nil || !strings.Contains(err.Error(), "does not support context reset") {
		t.Fatalf("legacy worker accepted isolated context: %v", err)
	}
	got, _ := st.GetReview(req.ID)
	if got.Status != "pending" || len(st.ListRuns(req.ID)) != 0 {
		t.Fatal("unsupported worker claimed the reset job")
	}
	if _, err := st.RegisterWorker(store.Agent{ID: "worker"}, store.WorkerCapabilities{ContextReset: true, Repos: map[string]string{"a/b": "/checkout"}}); err != nil {
		t.Fatal(err)
	}
	payload, err := svc.StartRemote(req, "worker")
	if err != nil || payload.ContextID == "" || payload.SessionID != "" || payload.RunID == "" {
		t.Fatalf("upgraded worker payload=%+v err=%v", payload, err)
	}
}

func TestResetContextRejectsUnauthorizedAndActiveJobs(t *testing.T) {
	for _, tc := range []struct{ status, reaction, run string }{
		{"pending", "", ""}, {"reviewing", "", ""}, {"archived", "", ""},
		{"succeeded", "pending", ""}, {"failed", "running", ""}, {"succeeded", "", "running"},
	} {
		t.Run(tc.status+tc.reaction+tc.run, func(t *testing.T) {
			st := refreshStore(t)
			newReviewForRetry(t, st, "rvw_busy", tc.status, "head", "ses-old", "")
			if tc.reaction != "" {
				if _, err := st.CreateReaction(store.ReviewReaction{ReviewID: "rvw_busy", Status: tc.reaction}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.run != "" {
				if _, err := st.CreateReviewRun(store.ReviewRun{ReviewID: "rvw_busy", Status: tc.run}); err != nil {
					t.Fatal(err)
				}
			}
			svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, ReviewerOpenID: "reviewer"})
			svc.SetJobSink(func(string, string) { t.Error("rejected reset was enqueued") })
			if _, err := svc.ResetContext("rvw_busy", MessageContext{SenderOpenID: "outsider"}); err == nil {
				t.Fatal("unauthorized reset allowed")
			}
			_, err := svc.ResetContext("rvw_busy", MessageContext{SenderOpenID: "reviewer"})
			want := store.ErrReviewContextBusy
			if tc.status == "archived" {
				want = ErrArchived
			}
			if !errors.Is(err, want) {
				t.Fatalf("want %v got %v", want, err)
			}
			req, _ := st.GetReview("rvw_busy")
			id, _ := svc.ContextID(req.ID)
			if req.Status != tc.status || req.SessionID != "ses-old" || id != "" {
				t.Fatal("rejected reset changed saved context")
			}
		})
	}
	st := refreshStore(t)
	svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, ReviewerOpenID: "reviewer"})
	if _, err := svc.ResetContext("missing", MessageContext{SenderOpenID: "reviewer"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing review: %v", err)
	}
	svc.cfg.Enabled = false
	if _, err := svc.ResetContext("missing", MessageContext{SenderOpenID: "reviewer"}); err == nil {
		t.Fatal("disabled reset allowed")
	}
}

func TestLocalResetCreatesAndThenResumesNewSession(t *testing.T) {
	st := refreshStore(t)
	newReviewForRetry(t, st, "rvw_fresh", "failed", "head", "ses-expired", "")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	toolDir := t.TempDir()
	sessions := filepath.Join(toolDir, "sessions.json")
	tool := filepath.Join(toolDir, "tool")
	script := "#!/bin/sh\nif [ \"$1 $2\" = \"session list\" ]; then cat '" + sessions + "'; else printf '%s\\n' \"$@\"; fi\n"
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, ReviewerOpenID: "reviewer", AllowedUsers: []string{"reviewer"}, Repos: map[string]string{"a/b": root}, Tool: tool, GHCli: fakeGHCli(t, "head"), LogDir: t.TempDir()})
	svc.SetJobSink(func(string, string) {})
	if _, err := svc.ResetContext("rvw_fresh", MessageContext{SenderOpenID: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	ctxID, _ := svc.ContextID("rvw_fresh")
	data, _ := json.Marshal([]map[string]any{
		{"id": "ses-expired", "title": SessionTitle("rvw_fresh", ""), "directory": root, "updated": 999},
		{"id": "ses-new", "title": SessionTitle("rvw_fresh", ctxID), "directory": root, "updated": 1},
	})
	if err := os.WriteFile(sessions, data, 0600); err != nil {
		t.Fatal(err)
	}
	svc.process("rvw_fresh")
	req, _ := st.GetReview("rvw_fresh")
	if req.Status != "succeeded" || req.SessionID != "ses-new" || strings.Contains(req.ResultText, "\n--session\n") || strings.Contains(req.ResultText, "ses-expired") || !strings.Contains(req.ResultText, SessionTitle(req.ID, ctxID)) {
		t.Fatalf("fresh session was not used: %+v", req)
	}
	// An ordinary retry after failure keeps the new context and resumes it.
	req.Status = "failed"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retry(req.ID, MessageContext{SenderOpenID: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	svc.process(req.ID)
	req, _ = st.GetReview(req.ID)
	if !strings.Contains(req.ResultText, "\n--session\nses-new\n") || strings.Contains(req.ResultText, "ses-expired") {
		t.Fatalf("retry did not retain new session: %s", req.ResultText)
	}
}
