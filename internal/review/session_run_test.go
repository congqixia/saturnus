package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

// This fake tool has one shared session namespace. A session-list lookup would
// return misleading data; callers must use their own stdout and explicit export.
func sessionTestTool(t *testing.T) (string, string) {
	t.Helper()
	state := t.TempDir()
	tool := filepath.Join(state, "opencode")
	script := `#!/bin/sh
state=$(dirname "$0")
if [ "$1" = --version ]; then
  if [ -f "$state/version" ]; then cat "$state/version"; else echo 1.19.0; fi
  exit
fi
if [ "$1 $2" = 'session export' ]; then echo v2 > "$state/export-command"; cat "$state/export-$3.json"; exit; fi
if [ "$1" = export ]; then echo v1 > "$state/export-command"; cat "$state/export-$2.json"; exit; fi
if [ "$1" = session ]; then touch "$state/scanned"; echo '[]'; exit; fi
title=
previous=
for arg in "$@"; do
  if [ "$previous" = --title ]; then title=$arg; fi
  previous=$arg
done
touch "$state/$title.started"
while [ -f "$state/hold" ]; do sleep 0.01; done
if [ -f "$state/$title.stderr" ]; then cat "$state/$title.stderr" >&2; fi
cat "$state/$title.events"
`
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return tool, state
}

func writeSessionFixture(t *testing.T, state, id, reviewID, contextID, dir, parent string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"info": sessionMetadata{ID: id, Title: SessionTitle(reviewID, contextID), Directory: dir, ParentID: parent}, "messages": []any{}})
	if err := os.WriteFile(filepath.Join(state, "export-"+id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func sessionEvent(kind, id, text string) string {
	data, _ := json.Marshal(map[string]any{"type": kind, "sessionID": id, "part": map[string]string{"sessionID": id, "text": text}})
	return string(data) + "\n"
}

func sessionRunArgs(identity SessionIdentity) []string {
	args := []string{"run", "--auto", "--format", "json", "--title", SessionTitle(identity.ReviewID, identity.ContextID)}
	if identity.SessionID != "" {
		args = append(args, "--session", identity.SessionID)
	}
	return append(args, "Review this PR")
}

func TestConcurrentRunsBindOnlyTheirOwnRootSession(t *testing.T) {
	for _, sameCheckout := range []bool{false, true} {
		t.Run(map[bool]string{false: "different repos", true: "shared checkout"}[sameCheckout], func(t *testing.T) {
			tool, state := sessionTestTool(t)
			if err := os.WriteFile(filepath.Join(state, "version"), []byte("2.2.5\n"), 0600); err != nil {
				t.Fatal(err)
			}
			dirs := []string{t.TempDir(), t.TempDir()}
			if sameCheckout {
				dirs[1] = dirs[0]
			}
			identities := []SessionIdentity{{ReviewID: "rvw_a"}, {ReviewID: "rvw_b"}}
			ids := []string{"ses_a", "ses_b"}
			for i, identity := range identities {
				writeSessionFixture(t, state, ids[i], identity.ReviewID, "", dirs[i], "")
				writeV2SessionFixture(t, state, ids[i], identity.ReviewID, "", dirs[i])
				text := "REVIEW SUMMARY:\nPR summary: " + identity.ReviewID + "\nIssues: None\nReview suggestions: LGTM"
				events := sessionEvent("step_start", ids[i], "") + sessionEvent("text", ids[i], text)
				if err := os.WriteFile(filepath.Join(state, SessionTitle(identity.ReviewID, "")+".events"), []byte(events), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(state, "hold"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type outcome struct {
				result RunResult
				err    error
			}
			results := []chan outcome{make(chan outcome, 1), make(chan outcome, 1)}
			for i := range identities {
				go func(i int) {
					r, e := RunWithSession(ctx, tool, sessionRunArgs(identities[i]), dirs[i], identities[i], nil)
					results[i] <- outcome{r, e}
				}(i)
			}
			// Both processes must start before either is released: the fix must
			// preserve concurrency instead of hiding the bug with a global lock.
			for _, identity := range identities {
				for {
					if _, err := os.Stat(filepath.Join(state, SessionTitle(identity.ReviewID, "")+".started")); err == nil {
						break
					}
					if ctx.Err() != nil {
						t.Fatal("review processes did not run concurrently")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if err := os.Remove(filepath.Join(state, "hold")); err != nil {
				t.Fatal(err)
			}
			for i, ch := range results {
				out := <-ch
				if out.err != nil || out.result.SessionID != ids[i] || !strings.Contains(out.result.Output, "PR summary: "+identities[i].ReviewID) || strings.Contains(out.result.Output, identities[1-i].ReviewID) {
					t.Fatalf("wrong binding: %+v", out)
				}
			}
			if _, err := os.Stat(filepath.Join(state, "scanned")); !os.IsNotExist(err) {
				t.Fatal("run scanned the global session list")
			}
		})
	}
}

// Fixture shape and command path match the pinned v2.2.5 SessionTransfer.Data
// and packages/cli/src/commands/handlers/session/export.ts implementation.
func writeV2SessionFixture(t *testing.T, state, id, reviewID, contextID, dir string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"info": map[string]any{"id": id, "title": SessionTitle(reviewID, contextID), "parentID": nil, "location": map[string]string{"directory": dir}}, "messages": []any{}})
	if err := os.WriteFile(filepath.Join(state, "export-"+id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeV225BindingAndResume(t *testing.T) {
	tool, state := sessionTestTool(t)
	if err := os.WriteFile(filepath.Join(state, "version"), []byte("2.2.5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	identity := SessionIdentity{ReviewID: "rvw_v2", ContextID: "ctx_current"}
	writeV2SessionFixture(t, state, "ses_v2", identity.ReviewID, identity.ContextID, dir)
	events := sessionEvent("step_start", "ses_v2", "") + sessionEvent("text", "ses_v2", "REVIEW SUMMARY:\nPR summary: v2 review\nIssues: None\nReview suggestions: LGTM")
	if err := os.WriteFile(filepath.Join(state, SessionTitle(identity.ReviewID, identity.ContextID)+".events"), []byte(events), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
	if err != nil || result.SessionID != "ses_v2" || !strings.Contains(result.Output, "v2 review") {
		t.Fatalf("v2 capture failed: %+v %v", result, err)
	}
	identity.SessionID = result.SessionID
	result, err = RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
	if err != nil || result.SessionID != "ses_v2" {
		t.Fatalf("v2 resume failed: %+v %v", result, err)
	}
	used, _ := os.ReadFile(filepath.Join(state, "export-command"))
	if strings.TrimSpace(string(used)) != "v2" {
		t.Fatalf("wrong export command: %s", used)
	}
	// Metadata, rather than the CLI working directory, governs V2 resumes:
	// resolveSessionTarget explicitly adopts the selected session's location.
	writeV2SessionFixture(t, state, "ses_v2", identity.ReviewID, identity.ContextID, t.TempDir())
	if err := os.Remove(filepath.Join(state, SessionTitle(identity.ReviewID, identity.ContextID)+".started")); err != nil {
		t.Fatal(err)
	}
	_, err = RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("v2 resumed another repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, SessionTitle(identity.ReviewID, identity.ContextID)+".started")); !os.IsNotExist(err) {
		t.Fatal("foreign V2 session was started")
	}
}

func TestOpenCodeV225ErrorsAreNotSuccessfulReviews(t *testing.T) {
	for _, id := range []string{"", "ses_v2"} {
		t.Run("session="+id, func(t *testing.T) {
			tool, state := sessionTestTool(t)
			if err := os.WriteFile(filepath.Join(state, "version"), []byte("2.2.5"), 0600); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			identity := SessionIdentity{ReviewID: "rvw_v2"}
			writeV2SessionFixture(t, state, "ses_v2", identity.ReviewID, "", dir)
			event, _ := json.Marshal(map[string]any{"type": "error", "sessionID": id, "error": map[string]string{"type": "unknown", "message": "provider unavailable"}})
			if err := os.WriteFile(filepath.Join(state, SessionTitle(identity.ReviewID, "")+".events"), event, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
			if err == nil || !strings.Contains(err.Error(), "provider unavailable") || result.SessionID != id {
				t.Fatalf("v2 error lost: %+v %v", result, err)
			}
		})
	}
}

func TestResumeRejectsForeignOrUnverifiableSessionBeforeStarting(t *testing.T) {
	for _, failure := range []string{"repo", "review", "context", "child", "missing", "wrong ID"} {
		t.Run(failure, func(t *testing.T) {
			tool, state := sessionTestTool(t)
			dir := t.TempDir()
			identity := SessionIdentity{ReviewID: "rvw_a", ContextID: "ctx_current", SessionID: "ses_bad"}
			repo, reviewID, contextID, parent := dir, identity.ReviewID, identity.ContextID, ""
			switch failure {
			case "repo":
				repo = t.TempDir()
			case "review":
				reviewID = "rvw_b"
			case "context":
				contextID = "ctx_old"
			case "child":
				parent = "ses_parent"
			}
			if failure != "missing" {
				writeSessionFixture(t, state, identity.SessionID, reviewID, contextID, repo, parent)
			}
			if failure == "wrong ID" {
				if err := os.WriteFile(filepath.Join(state, "export-ses_bad.json"), []byte(`{"info":{"id":"ses_other","title":"saturnus-review-rvw_a-ctx_current","directory":"`+dir+`"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
			if err == nil || result.SessionID != "" || !strings.Contains(err.Error(), "reset the review context") {
				t.Fatalf("unsafe resume accepted: %+v %v", result, err)
			}
			if _, err := os.Stat(filepath.Join(state, SessionTitle(identity.ReviewID, identity.ContextID)+".started")); !os.IsNotExist(err) {
				t.Fatal("foreign session was resumed before validation")
			}
		})
	}
}

func TestSessionEventsIgnoreStderrAndNestedIDsAndSurviveTruncation(t *testing.T) {
	tool, state := sessionTestTool(t)
	dir := t.TempDir()
	identity := SessionIdentity{ReviewID: "rvw_a"}
	writeSessionFixture(t, state, "ses_a", identity.ReviewID, "", dir, "")
	spoof := sessionEvent("step_start", "ses_foreign", "")
	events := strings.Repeat("log", maxResultBytes/3+1) + "\n" + sessionEvent("step_start", "ses_a", "") + sessionEvent("text", "ses_a", spoof)
	base := filepath.Join(state, SessionTitle(identity.ReviewID, ""))
	if err := os.WriteFile(base+".events", []byte(events), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+".stderr", []byte(spoof), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
	if err != nil || result.SessionID != "ses_a" || !strings.Contains(result.Output, "[truncated]") {
		t.Fatalf("root binding lost/spoofed: id=%s err=%v", result.SessionID, err)
	}
	// A stream without a root ID must never borrow one from stderr or a list.
	if err := os.WriteFile(base+".events", []byte("review complete\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = RunWithSession(context.Background(), tool, sessionRunArgs(identity), dir, identity, nil)
	if err != nil || result.SessionID != "" {
		t.Fatalf("guessed a session: %+v %v", result, err)
	}
}

func TestSessionStreamRejectsChangedRootAndDecodesChunkedText(t *testing.T) {
	for _, change := range []bool{false, true} {
		var text strings.Builder
		ctx, cancel := context.WithCancel(context.Background())
		stream := &sessionStream{expected: "ses_a", write: func(s string) { text.WriteString(s) }, cancel: cancel}
		events := sessionEvent("step_start", "ses_a", "") + sessionEvent("text", "ses_a", "REVIEW SUMMARY:\nhello 世界")
		if change {
			events += sessionEvent("text", "ses_b", "must not be returned")
		}
		for i := range []byte(events) {
			_, _ = stream.Write([]byte{events[i]})
		}
		stream.finish()
		if !strings.Contains(text.String(), "hello 世界") || strings.Contains(text.String(), "must not be returned") || (stream.err != nil) != change || (ctx.Err() != nil) != change {
			t.Fatalf("stream=%+v text=%s", stream, text.String())
		}
		cancel()
	}
}

func TestUnsafeSessionArgumentsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--continue", "prompt"}, {"run", "--continue=true", "prompt"}, {"run", "-c", "prompt"}, {"run", "--fork", "prompt"},
		{"run", "--session", "ses_other", "prompt"}, {"run", "--session=ses_other", "prompt"}, {"run", "-sses_other", "prompt"},
		{"run", "--session", "ses_a", "--session", "ses_a", "prompt"}, {"run", "--attach", "http://other-host", "prompt"},
	} {
		if _, err := checkSessionArgs(args, t.TempDir(), "ses_a"); err == nil {
			t.Fatalf("accepted unsafe args: %v", args)
		}
	}
}

func TestLocalReviewAndActionRejectSavedForeignSession(t *testing.T) {
	for _, action := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "action"}[action], func(t *testing.T) {
			st := refreshStore(t)
			tool, state := sessionTestTool(t)
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			status := "pending"
			if action {
				status = "succeeded"
			}
			newReviewForRetry(t, st, "rvw_a", status, "head", "ses_foreign", "")
			writeSessionFixture(t, state, "ses_foreign", "rvw_b", "", t.TempDir(), "")
			svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, Tool: tool, GHCli: fakeGHCli(t, "head"), Repos: map[string]string{"a/b": dir}, LogDir: t.TempDir()})
			if action {
				reac, err := st.CreateReaction(store.ReviewReaction{ReviewID: "rvw_a", Status: "pending", SessionID: "ses_foreign", Instruction: "act"})
				if err != nil {
					t.Fatal(err)
				}
				svc.processReaction(reac.ID)
				got, _ := st.GetReaction(reac.ID)
				if got.Status != "failed" || !strings.Contains(got.Error, "does not belong") {
					t.Fatalf("action resumed foreign session: %+v", got)
				}
			} else {
				svc.process("rvw_a")
				got, _ := st.GetReview("rvw_a")
				if got.Status != "failed" || !strings.Contains(got.Error, "does not belong") {
					t.Fatalf("review resumed foreign session: %+v", got)
				}
			}
			if _, err := os.Stat(filepath.Join(state, SessionTitle("rvw_a", "")+".started")); !os.IsNotExist(err) {
				t.Fatal("foreign session reached review tool")
			}
		})
	}
}
