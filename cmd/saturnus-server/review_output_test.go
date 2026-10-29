package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
)

func TestReviewListUsesPriorVerdictWithoutChangingCurrentResult(t *testing.T) {
	for _, tc := range []struct{ verdict, short string }{
		{"lgtm", "LGTM"}, {"nit_comments", "NIT"}, {"request_change", "RC"},
	} {
		t.Run(tc.short, func(t *testing.T) {
			st, err := store.Open(t.TempDir() + "/saturnus.db")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			req, err := st.CreateReview(store.ReviewRequest{ID: "rvw_list", ThreadID: thread.ID, Status: "reviewing", Repo: "a/b", PRNumber: 1, HeadCommit: "old", PrHeadCommit: "new"})
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range []store.ReviewRun{
				{ReviewID: req.ID, Status: "succeeded", Verdict: "request_change"},
				{ReviewID: req.ID, Status: "succeeded", Verdict: tc.verdict},
				{ReviewID: req.ID, Status: "failed"},
				{ReviewID: req.ID, Status: "running"},
			} {
				if _, err := st.CreateReviewRun(run); err != nil {
					t.Fatal(err)
				}
			}
			svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
			srv := &server{store: st, reviews: svc}
			want := "reviewing / " + tc.short + " / OUTDATED"
			reply := srv.reviewsReply("")
			tag := reply.table.Rows[0]["status"].(map[string]any)
			if tag["text"] != want || tag["color"] != "orange" || !strings.Contains(reply.text, want) || !strings.Contains(svc.FormatList([]store.ReviewRequest{req}), want) {
				t.Fatalf("last conclusion omitted or changed status color: tag=%v text=%s", tag, reply.text)
			}
			stored, _ := st.GetReview(req.ID)
			if stored.Verdict != "" {
				t.Fatalf("historical verdict was saved onto the active review: %+v", stored)
			}
			// Findings point to the round that last updated them, not the
			// current execution. Details live in a separate bounded reply.
			runs := st.ListRuns(req.ID)
			if err := st.SaveFindings(req.ID, runs[1].ID, []store.ReviewFinding{{Severity: "P1", Status: "addressed", Path: "a|b.go", Line: 7, Message: "first line\nsecond line", Suggestion: "use a channel"}}); err != nil {
				t.Fatal(err)
			}
			described := srv.describeReviewReply(req.ID)
			for _, output := range []string{described.text, described.md.Content} {
				for _, fragment := range []string{"Findings: 1 (P0: 0, P1: 1, P2: 0)", "addressed=1", "/sat findings " + req.ID, "**Runs**:"} {
					if !strings.Contains(output, fragment) {
						t.Fatalf("description omitted summary %q: %s", fragment, output)
					}
				}
				if strings.Contains(output, "first line") || strings.Contains(output, "use a channel") {
					t.Fatalf("finding details leaked into description: %s", output)
				}
			}
			details := srv.handleBotCommand("/sat findings "+req.ID, review.MessageContext{})
			if details.table == nil || len(details.table.Rows) != 1 {
				t.Fatalf("missing native finding table: %+v", details)
			}
			row := details.table.Rows[0]
			if row["status"] != "addressed" || row["run"] != "#2" || row["location"] != "a|b.go:7" || row["fix"] != "use a channel" {
				t.Fatalf("missing finding metadata: %v", row)
			}
		})
	}
}

func TestFindingCommandPaginatesAndKeepsDescribeCompact(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{ID: "rvw_many", ThreadID: thread.ID, Status: "succeeded", Verdict: "request_change", Summary: "Concise conclusion", ResultText: "REVIEW SUMMARY: duplicated finding content"})
	if err != nil {
		t.Fatal(err)
	}
	findings := make([]store.ReviewFinding, 12)
	for i := range findings {
		findings[i] = store.ReviewFinding{Severity: "P1", Title: "finding content", Message: strings.Repeat("中文内容", 2000), Suggestion: strings.Repeat("修改建议", 2000)}
	}
	if err := st.SaveFindings(req.ID, "", findings); err != nil {
		t.Fatal(err)
	}
	srv := &server{store: st, reviews: review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})}
	for _, tc := range []struct {
		command      string
		count, start int
	}{
		{"/sat findings rvw_many", 5, 1}, {"/sat findings rvw_many 2", 5, 6}, {"/sat findings rvw_many 3", 2, 11},
	} {
		reply := srv.handleBotCommand(tc.command, review.MessageContext{})
		if reply.table == nil || len(reply.table.Rows) != tc.count || reply.table.Rows[0]["number"] != tc.start || reply.table.Rows[0]["status"] != "open" || reply.table.Rows[0]["run"] != "-" {
			t.Fatalf("wrong page: %s %+v", tc.command, reply)
		}
		payload, err := json.Marshal(reply.table)
		if err != nil || len(payload) > 15000 || !utf8.ValidString(reply.text) {
			t.Fatalf("oversized/invalid finding reply: %d %v", len(payload), err)
		}
	}
	for _, command := range []string{"/sat findings", "/sat findings rvw_many 0", "/sat findings rvw_many -1", "/sat findings rvw_many xxx", "/sat findings rvw_many 4", "/sat findings missing"} {
		reply := srv.handleBotCommand(command, review.MessageContext{})
		if reply.table != nil || reply.text == "" {
			t.Fatalf("missing validation for %s: %+v", command, reply)
		}
	}
	description := srv.describeReviewReply(req.ID)
	for _, output := range []string{description.text, description.md.Content} {
		if len(output) > 2500 || strings.Contains(output, "finding content") || strings.Contains(output, "中文内容") || !strings.Contains(output, "Findings: 12") || !strings.Contains(output, "Concise conclusion") {
			t.Fatalf("description grew with findings: %s", output)
		}
	}
	if !isReviewRequest("/sat findings rvw_many") || !strings.Contains(helpText(), "/sat findings <review_id> [page]") {
		t.Fatal("finding command missing from group routing/help")
	}
	empty, err := st.CreateReview(store.ReviewRequest{ID: "rvw_empty", ThreadID: thread.ID, Status: "succeeded"})
	if err != nil {
		t.Fatal(err)
	}
	if reply := srv.reviewFindingsReply(empty.ID, 1); reply.table != nil || !strings.Contains(reply.text, "No findings.") {
		t.Fatalf("empty findings reply: %+v", reply)
	}
}
