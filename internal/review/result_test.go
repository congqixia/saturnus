package review

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"saturnus/internal/store"
)

func TestLocalCompletionRequiresConfiguredStructuredResult(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "text mode", true: "structured mode"}[structured], func(t *testing.T) {
			st := refreshStore(t)
			newReviewForRetry(t, st, "rvw_missing", "pending", "head", "", "")
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(t.TempDir(), "tool")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nif [ \"$1 $2\" = 'session list' ]; then echo '[]'; else echo 'permission requested: external_directory (/tmp/*); auto-rejecting'; fi\nexit 0\n"), 0755); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Enabled: true, Tool: tool, GHCli: fakeGHCli(t, "head"), Repos: map[string]string{"a/b": root}, LogDir: t.TempDir()}
			if structured {
				cfg.ResultAPIURL = "http://review.invalid/api/reviews/{review_id}/result"
			}
			svc := New(st, &fakeLark{}, cfg)
			svc.process("rvw_missing")
			req, _ := st.GetReview("rvw_missing")
			want := "succeeded"
			if structured {
				want = "failed"
				if !strings.Contains(req.Error, "missing structured review result") || !strings.Contains(req.Error, "permission was rejected") {
					t.Fatalf("missing diagnostic: %s", req.Error)
				}
			}
			run, err := st.LatestRun(req.ID)
			if err != nil || req.Status != want || run.Status != want || run.Error != req.Error {
				t.Fatalf("review=%+v run=%+v err=%v", req, run, err)
			}
		})
	}
}

func TestRemoteCompletionRequiresThisRunsResult(t *testing.T) {
	for _, submit := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "received"}[submit], func(t *testing.T) {
			st := refreshStore(t)
			newReviewForRetry(t, st, "rvw_remote", "pending", "head", "", "")
			svc := New(st, &fakeLark{}, Config{Enabled: true, ResultAPIURL: "http://review.invalid/api/reviews/{review_id}/result?source=saturnus"})
			req, _ := st.GetReview("rvw_remote")
			p, err := svc.StartRemote(req, "worker")
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(p.ResultAPIURL)
			if err != nil || u.Query().Get("run_id") != p.RunID || u.Query().Get("source") != "saturnus" {
				t.Fatalf("callback does not identify run: %s (%v)", p.ResultAPIURL, err)
			}
			if submit {
				if err := svc.IngestRunResult(req.ID, p.RunID, "lgtm", "Review complete", nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.CompleteRemoteRun(req.ID, p.RunID, "worker", true, "head", "", "review output", ""); err != nil {
				t.Fatal(err)
			}
			req, _ = st.GetReview(req.ID)
			run, _ := st.LatestRun(req.ID)
			want := "failed"
			if submit {
				want = "succeeded"
				if req.Verdict != "lgtm" || run.Verdict != "lgtm" {
					t.Fatalf("lost verdict: review=%+v run=%+v", req, run)
				}
			}
			if req.Status != want || run.Status != want {
				t.Fatalf("review=%+v run=%+v", req, run)
			}
		})
	}
}

func TestRemoteInvalidGuideDoesNotClaimOrRequeue(t *testing.T) {
	st := refreshStore(t)
	newReviewForRetry(t, st, "rvw_bad_guide", "pending", "head", "", "")
	svc := New(st, &fakeLark{}, Config{Enabled: true, Guides: map[string]string{"a/b": filepath.Join(t.TempDir(), "missing-guide")}})
	queued := 0
	svc.SetJobSink(func(string, string) { queued++ })
	req, _ := st.GetReview("rvw_bad_guide")
	if _, err := svc.StartRemote(req, "worker"); err == nil {
		t.Fatal("expected missing guide to fail")
	}
	req, _ = st.GetReview(req.ID)
	if req.Status != "pending" || len(st.ListRuns(req.ID)) != 0 || queued != 0 {
		t.Fatalf("invalid configuration generated dispatch attempts: %+v queued=%d", req, queued)
	}
}

func TestResultAndCompletionRejectPreviousRunAfterRequeue(t *testing.T) {
	st := refreshStore(t)
	newReviewForRetry(t, st, "rvw_fence", "pending", "head", "", "")
	svc := New(st, &fakeLark{}, Config{Enabled: true, ResultAPIURL: "http://review.invalid/api/reviews/{review_id}/result"})
	req, _ := st.GetReview("rvw_fence")
	first, err := svc.StartRemote(req, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestRunResult(req.ID, first.RunID, "lgtm", "First execution", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.RequeueForOrphan(req.ID, "worker", "disconnected"); err != nil {
		t.Fatal(err)
	}
	req, _ = st.GetReview(req.ID)
	second, err := svc.StartRemote(req, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestRunResult(req.ID, first.RunID, "lgtm", "Stale execution", []Finding{{Severity: "P1", Path: "old.go", Message: "stale"}}); !errors.Is(err, ErrStaleResult) {
		t.Fatalf("accepted stale result: %v", err)
	}
	if err := svc.CompleteRemoteRun(req.ID, first.RunID, "worker", true, "head", "", "stale output", ""); !errors.Is(err, ErrStaleResult) {
		t.Fatalf("accepted stale completion: %v", err)
	}
	req, _ = st.GetReview(req.ID)
	if req.Status != "reviewing" || req.Verdict != "" || len(st.ListFindings(req.ID)) != 0 {
		t.Fatalf("stale result changed the new run: %+v", req)
	}
	// The previous execution's valid result must not make this one succeed.
	if err := svc.CompleteRemoteRun(req.ID, second.RunID, "worker", true, "head", "", "no result submitted", ""); err != nil {
		t.Fatal(err)
	}
	req, _ = st.GetReview(req.ID)
	if req.Status != "failed" || req.Verdict != "" {
		t.Fatalf("accepted old result for the new run: %+v", req)
	}
}

func TestFormatResultShowsPreviousFindingsWithoutVerdict(t *testing.T) {
	st := refreshStore(t)
	req := newReviewingReview(t, st, "rvw_history")
	if err := st.SaveFindings(req.ID, "", []store.ReviewFinding{{Severity: "P1", Path: "old.go", Message: "existing finding"}}); err != nil {
		t.Fatal(err)
	}
	svc := New(st, &fakeLark{}, Config{})
	out := svc.FormatResult(req)
	if !strings.Contains(out, "Previous findings") || !strings.Contains(out, "existing finding") {
		t.Fatalf("missing historical findings: %s", out)
	}
}

func TestRetryMissingResultDoesNotRequireHeadChange(t *testing.T) {
	st := refreshStore(t)
	newReviewForRetry(t, st, "rvw_incomplete", "succeeded", "same-head", "session", "")
	svc := New(st, &fakeLark{}, Config{Enabled: true, AllowedUsers: []string{"reviewer"}, ResultAPIURL: "http://review.invalid/result"})
	svc.SetJobSink(func(string, string) {})
	req, err := svc.Retry("rvw_incomplete", MessageContext{SenderOpenID: "reviewer"})
	if err != nil || req.Status != "pending" || req.SessionID != "session" || !strings.Contains(req.RetryReason, "missing structured review result") {
		t.Fatalf("incomplete result could not be retried: %+v (%v)", req, err)
	}
}

func TestResultIngestionAndCompletionAreSerialized(t *testing.T) {
	st := refreshStore(t)
	svc := New(st, &fakeLark{}, Config{Enabled: true, ResultAPIURL: "http://review.invalid/result"})
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("rvw_race_%d", i)
		newReviewForRetry(t, st, id, "pending", "head", "", "")
		req, _ := st.GetReview(id)
		p, err := svc.StartRemote(req, "worker")
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		ingested, completed := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			ingested <- svc.IngestRunResult(id, p.RunID, "nit_comments", "Summary", []Finding{{Severity: "P2", Path: "a.go", Message: "nit"}})
		}()
		go func() {
			<-start
			completed <- svc.CompleteRemoteRun(id, p.RunID, "worker", true, "head", "", "output", "")
		}()
		close(start)
		ingestErr, completeErr := <-ingested, <-completed
		if completeErr != nil {
			t.Fatal(completeErr)
		}
		req, _ = st.GetReview(id)
		run, _ := st.LatestRun(id)
		if req.Status != run.Status || req.Verdict != run.Verdict {
			t.Fatalf("completion and ingestion disagree: review=%+v run=%+v", req, run)
		}
		if ingestErr == nil {
			if req.Status != "succeeded" || req.Verdict != "nit_comments" || len(st.ListFindings(id)) != 1 {
				t.Fatalf("accepted result was lost: %+v", req)
			}
		} else if !errors.Is(ingestErr, ErrStaleResult) || req.Status != "failed" || req.Verdict != "" || len(st.ListFindings(id)) != 0 {
			t.Fatalf("late result partially changed a finished run: %+v (%v)", req, ingestErr)
		}
	}
}
