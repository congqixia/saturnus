package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"saturnus/internal/store"
)

func fakeInlineGH(t *testing.T, response string, fail bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	responsePath := filepath.Join(dir, "comments.json")
	if err := os.WriteFile(responsePath, []byte(response), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1 $2" = 'pr view' ]; then
  echo '{"headRefOid":"head","title":"PR","baseRefName":"main"}'
  exit 0
fi
if [ "$1 $2 $3 $4" != 'api repos/a/b/pulls/1/comments --paginate --jq' ]; then
  echo 'unexpected request' >&2
  exit 1
fi
`
	script += "cat '" + responsePath + "'\n"
	if fail {
		script += "exit 1\n"
	}
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return gh, responsePath
}

const inlineRootPage = `[{"id":100,"user":"reviewer","body":"original concern","created_at":"2026-10-30T02:35:59Z","html_url":"https://github.com/a/b/pull/1#discussion_r100"}]`
const inlineReplyPage = `[{"id":101,"in_reply_to_id":100,"user":"author","body":"The server guarantees exactly nq entries. Keep \"topks\" as is.\nPath: C:\\tmp","created_at":"2026-10-30T03:55:45Z","html_url":"https://github.com/a/b/pull/1#discussion_r101"}]`

func TestFetchPRInlineCommentsAllPagesOrError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		fail     bool
		want     int
		wantErr  bool
	}{
		{name: "reply on later page", response: inlineRootPage + "\n" + inlineReplyPage, want: 2},
		{name: "no comments", response: "[]", want: 0},
		{name: "empty output", wantErr: true},
		{name: "null page", response: "null", wantErr: true},
		{name: "invalid later page", response: inlineRootPage + "\ninvalid", wantErr: true},
		{name: "null later page", response: inlineRootPage + "\nnull", wantErr: true},
		{name: "missing id", response: `[{"body":"bad comment"}]`, wantErr: true},
		{name: "missing timestamp", response: `[{"id":101}]`, wantErr: true},
		{name: "command failure after output", response: inlineRootPage, fail: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, _ := fakeInlineGH(t, tc.response, tc.fail)
			comments, err := FetchPRInlineComments(context.Background(), gh, "a/b", 1)
			if tc.wantErr {
				if err == nil || len(comments) != 0 {
					t.Fatalf("partial/invalid feedback accepted: comments=%+v err=%v", comments, err)
				}
				return
			}
			if err != nil || len(comments) != tc.want {
				t.Fatalf("comments=%+v err=%v", comments, err)
			}
			if tc.want == 2 && (comments[1].InReplyToID != 100 || comments[1].User != "author") {
				t.Fatalf("lost reply association: %+v", comments[1])
			}
		})
	}
	t.Run("canceled fetch", func(t *testing.T) {
		gh, _ := fakeInlineGH(t, inlineRootPage, false)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := FetchPRInlineComments(ctx, gh, "a/b", 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	})
}

func newInlineReview(t *testing.T, st *store.Store, session string) (store.ReviewRequest, string) {
	t.Helper()
	newReviewForRetry(t, st, "rvw_inline", "pending", "head", session, "")
	if err := st.SaveFindings("rvw_inline", "", []store.ReviewFinding{
		{Severity: "P2", Path: "read.go", Line: 87, Title: "topks count", Message: "original concern"},
	}); err != nil {
		t.Fatal(err)
	}
	finding := st.ListFindings("rvw_inline")[0]
	if err := st.SetFindingCommentID(finding.ID, "100"); err != nil {
		t.Fatal(err)
	}
	req, err := st.GetReview("rvw_inline")
	if err != nil {
		t.Fatal(err)
	}
	return req, finding.ID
}

func TestRemoteRetryReceivesFreshInlineReplies(t *testing.T) {
	st := refreshStore(t)
	req, findingID := newInlineReview(t, st, "ses_previous")
	gh, responsePath := fakeInlineGH(t, inlineRootPage, false)
	svc := New(st, &fakeLark{}, Config{Enabled: true, GHCli: gh})
	defer svc.Stop()
	first, err := svc.JobPayload(req)
	if err != nil || !strings.Contains(first.PriorFindings, "No replies in the fetched thread") {
		t.Fatalf("first context=%q err=%v", first.PriorFindings, err)
	}
	// A reply arriving after an earlier run/payload must be fetched afresh,
	// even though the same tool session and stored findings are reused.
	if err := os.WriteFile(responsePath, []byte(inlineRootPage+"\n"+inlineReplyPage), 0600); err != nil {
		t.Fatal(err)
	}
	payload, err := svc.StartRemote(req, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != "ses_previous" || !strings.Contains(payload.PriorFindings, "Finding "+findingID) {
		t.Fatalf("lost finding/session: %+v", payload)
	}
	// Exercise the same rendering path used by a remote worker. The author
	// content contains quotes, a backslash and a newline; it must stay one arg.
	args, err := RenderCommand(payload.CommandTemplate, ToolArgs{
		ReviewID: req.ID, SessionID: payload.SessionID, RetryNote: "Continue reviewing.", PriorFindings: payload.PriorFindings,
	})
	if err != nil || len(args) != 9 {
		t.Fatalf("inline feedback broke command rendering: args=%v err=%v", args, err)
	}
	prompt := args[len(args)-1]
	for _, want := range []string{
		"author at 2026-10-30T03:55:45Z", "https://github.com/a/b/pull/1#discussion_r101",
		"The server guarantees exactly nq entries. Keep \"topks\" as is.\nPath: C:\\tmp",
		"Unchanged code alone does not mean a finding remains valid", `use "commented"`, "explain why the reply does not resolve it",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "No replies in the fetched thread") {
		t.Fatal("stale no-replies context survived refresh")
	}
	if got := st.ListFindings(req.ID)[0].Status; got != "open" {
		t.Fatalf("a reply must not automatically change finding status: %s", got)
	}
}

func TestInlineFeedbackFailurePreventsExecution(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote", true: "local"}[local], func(t *testing.T) {
			st := refreshStore(t)
			req, _ := newInlineReview(t, st, "ses_previous")
			gh, _ := fakeInlineGH(t, "", true)
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			svc := New(st, &fakeLark{}, Config{Enabled: true, GHCli: gh, Tool: "/missing/review-tool", Repos: map[string]string{"a/b": root}})
			defer svc.Stop()
			if local {
				svc.process(req.ID)
				got, _ := st.GetReview(req.ID)
				run, err := st.LatestRun(req.ID)
				if err != nil || got.Status != "failed" || run.Status != "failed" || !strings.Contains(got.Error, "refresh prior finding inline discussion") {
					t.Fatalf("review=%+v run=%+v err=%v", got, run, err)
				}
			} else {
				if _, err := svc.StartRemote(req, "worker"); err == nil || !strings.Contains(err.Error(), "refresh prior finding inline discussion") {
					t.Fatalf("remote dispatch accepted missing discussion: %v", err)
				}
				got, _ := st.GetReview(req.ID)
				if got.Status != "pending" || got.WorkerID != "" || len(st.ListRuns(req.ID)) != 0 {
					t.Fatalf("failed refresh claimed a review: %+v", got)
				}
			}
		})
	}
}

func TestLocalReviewReceivesInlineRepliesWithoutResultCallback(t *testing.T) {
	st := refreshStore(t)
	req, _ := newInlineReview(t, st, "ses_previous")
	gh, _ := fakeInlineGH(t, inlineRootPage+"\n"+inlineReplyPage, false)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(t.TempDir(), "review-tool")
	if err := os.WriteFile(tool, []byte(`#!/bin/sh
if [ "$1" = --version ]; then
  echo 1.19.0
elif [ "$1 $2" = 'session list' ]; then
  echo '[]'
elif [ "$1" = export ]; then
  printf '{"info":{"id":"%s","title":"saturnus-review-rvw_inline","directory":"%s"}}\n' "$2" "$PWD"
else
  for arg in "$@"; do printf '%s\n' "$arg"; done
fi
`), 0755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, &fakeLark{}, Config{Enabled: true, GHCli: gh, Tool: tool, Repos: map[string]string{"a/b": root}, LogDir: t.TempDir()})
	defer svc.Stop()
	svc.process(req.ID)
	got, err := st.GetReview(req.ID)
	if err != nil || got.Status != "succeeded" || !strings.Contains(got.ResultText, "The server guarantees exactly nq entries.") {
		t.Fatalf("local tool did not receive replies: review=%+v err=%v", got, err)
	}
	if !strings.Contains(got.ResultText, "--session\nses_previous") {
		t.Fatal("local run did not resume the previous session")
	}
}

func TestResetContextKeepsInlineReplyContext(t *testing.T) {
	st := refreshStore(t)
	req, findingID := newInlineReview(t, st, "ses_previous")
	req.Status = "succeeded"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	req, _, err := st.ResetReviewContext(req.ID, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := fakeInlineGH(t, inlineRootPage+"\n"+inlineReplyPage, false)
	svc := New(st, &fakeLark{}, Config{GHCli: gh})
	defer svc.Stop()
	payload, err := svc.JobPayload(req)
	if err != nil || payload.SessionID != "" || payload.ContextID == "" || !strings.Contains(payload.PriorFindings, "Finding "+findingID) || !strings.Contains(payload.PriorFindings, "The server guarantees exactly nq entries.") {
		t.Fatalf("context reset lost feedback: payload=%+v err=%v", payload, err)
	}
}

func TestInlineDiscussionAssociatesRepliesAndOrdersThem(t *testing.T) {
	comments := []PRInlineComment{
		{ID: 103, InReplyToID: 100, User: "reviewer", CreatedAt: "2026-11-03T04:36:19Z", Body: "second reply"},
		{ID: 102, InReplyToID: 100, User: "author", CreatedAt: "2026-11-03T04:36:19Z", Body: "first reply"},
		{ID: 100, CreatedAt: "2026-10-30T02:35:59Z", Body: "original"},
		{ID: 999, InReplyToID: 900, CreatedAt: "2026-11-03T04:36:19Z", Body: "unrelated reply"},
		{ID: 200, CreatedAt: "2026-10-30T02:35:59Z", Body: "no replies here"},
	}
	findings := []store.ReviewFinding{
		{ID: "fnd_a", GithubCommentID: "100"},
		{ID: "fnd_b", GithubCommentID: "200"},
		{ID: "fnd_deleted", GithubCommentID: "300"},
		{ID: "fnd_unposted"},
	}
	got := formatInlineDiscussion("a/b", 1, findings, comments)
	a, b := strings.Index(got, "first reply"), strings.Index(got, "second reply")
	if a < 0 || b < a || strings.Contains(got, "unrelated reply") || strings.Contains(got, "fnd_unposted") {
		t.Fatalf("wrong reply mapping/order: %s", got)
	}
	if !strings.Contains(got, "No replies in the fetched thread") || !strings.Contains(got, "original comment may have been deleted") {
		t.Fatalf("missing empty/deleted thread context: %s", got)
	}
}

func TestPriorFindingContextRejectsOversizeFeedback(t *testing.T) {
	st := refreshStore(t)
	req, _ := newInlineReview(t, st, "")
	comments, err := json.Marshal([]PRInlineComment{{ID: 101, InReplyToID: 100, CreatedAt: "2026-10-30T03:55:45Z", Body: strings.Repeat("x", 64*1024)}})
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := fakeInlineGH(t, string(comments), false)
	svc := New(st, &fakeLark{}, Config{GHCli: gh})
	defer svc.Stop()
	if prior, err := svc.priorFindingsForRun(context.Background(), req); err == nil || prior != "" || !strings.Contains(err.Error(), "refusing to omit feedback") {
		t.Fatalf("oversize feedback silently truncated: bytes=%d err=%v", len(prior), err)
	}
}
