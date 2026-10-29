package main

import (
	"strings"
	"testing"

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
			// current execution. Both card and text must retain that mapping.
			runs := st.ListRuns(req.ID)
			if err := st.SaveFindings(req.ID, runs[1].ID, []store.ReviewFinding{{Severity: "P1", Status: "addressed", Path: "a|b.go", Line: 7, Message: "first line\nsecond line", Suggestion: "use a channel"}}); err != nil {
				t.Fatal(err)
			}
			described := srv.describeReviewReply(req.ID)
			for _, output := range []string{described.text, described.md.Content} {
				for _, fragment := range []string{"| Status | Run (last update) |", "| addressed | #2 |", `a\|b.go:7`, "first line second line", "use a channel", "**Runs**:"} {
					if !strings.Contains(output, fragment) {
						t.Fatalf("description omitted finding metadata %q: %s", fragment, output)
					}
				}
			}
		})
	}
}
