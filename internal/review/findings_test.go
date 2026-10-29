package review

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"saturnus/internal/store"
)

func TestFindingPageBoundsMessagesAndPreservesRoundAndStatus(t *testing.T) {
	findings := make([]store.ReviewFinding, 12)
	for i := range findings {
		findings[i] = store.ReviewFinding{ID: "finding", RunID: "old-run", Severity: "P1", Status: "addressed", Path: "file.go", Line: 12,
			Title: strings.Repeat("标题", 400), Message: strings.Repeat("内容", 4000), Suggestion: strings.Repeat("修复", 4000)}
	}
	req := store.ReviewRequest{ID: "rvw_large", Repo: "a/b", PRNumber: 1}
	runs := []store.ReviewRun{{ID: "old-run", Seq: 2}}
	for _, page := range []int{1, 2, 3} {
		text, err := FormatFindingPage(req, findings, runs, page)
		if err != nil || len(text) > 9000 || !utf8.ValidString(text) {
			t.Fatalf("invalid or oversized message: page=%d size=%d err=%v", page, len(text), err)
		}
		for _, want := range []string{"Previous findings", "addressed=12", "Run (last update): #2", "Status: addressed", "file.go:12"} {
			if !strings.Contains(text, want) {
				t.Fatalf("missing %q: %s", want, text)
			}
		}
		if (page < 3) != strings.Contains(text, "Next:") {
			t.Fatal("incorrect next-page command")
		}
	}
	for _, page := range []int{-1, 0, 4, int(^uint(0) >> 1)} {
		if _, _, err := FindingPage(findings, page); err == nil {
			t.Fatalf("accepted page %d", page)
		}
	}
	text, err := FormatFindingPage(req, nil, nil, 1)
	if err != nil || !strings.Contains(text, "Findings: 0") || !strings.Contains(text, "No findings.") {
		t.Fatalf("empty result: %s %v", text, err)
	}
}

type firstResultDeliveryFails struct{ *fakeLark }

func (f firstResultDeliveryFails) SendTextToUser(id, text string) error {
	_ = f.fakeLark.SendTextToUser(id, text)
	if len(f.sent) == 1 {
		return errors.New("delivery failed")
	}
	return nil
}

func TestNotifyReviewerSeparatesSummaryAndFindings(t *testing.T) {
	for _, target := range []string{"user", "chat", "first-delivery-fails", "no-findings"} {
		t.Run(target, func(t *testing.T) {
			st := refreshStore(t)
			req := newReviewingReview(t, st, "rvw_split")
			cfg := Config{ReviewerOpenID: "reviewer", ReplyInThread: true}
			if target == "chat" {
				cfg.ReviewerOpenID, cfg.ReviewerChatID = "", "review-chat"
			}
			fake := &fakeLark{enabled: true}
			svc := New(st, fake, cfg)
			if target == "first-delivery-fails" {
				svc.lark = firstResultDeliveryFails{fake}
			}
			if target != "no-findings" {
				if err := st.SaveFindings(req.ID, "", []store.ReviewFinding{{Severity: "P0", Title: "blocking bug", Message: "full finding detail", Path: "a.go", Line: 4}}); err != nil {
					t.Fatal(err)
				}
			}
			req.Verdict, req.Status, req.Summary, req.MessageID = "request_change", "succeeded", "Review summary", "message"
			if target == "no-findings" {
				req.Summary = strings.Repeat("评审概要", 1000)
			}
			req.ResultText = "REVIEW SUMMARY: full finding detail"
			svc.notifyReviewer(req)
			messages := fake.sent
			if target == "chat" {
				messages = fake.chatSent
			}
			if len(messages) != 2 || len(fake.replies) != 2 {
				t.Fatalf("expected two independent messages and replies: %v %v", messages, fake.replies)
			}
			if !utf8.ValidString(messages[0]) || len(messages[0]) > 3000 {
				t.Fatalf("invalid or oversized summary: %d bytes", len(messages[0]))
			}
			if !strings.Contains(messages[0], "Verdict: Request change") || strings.Contains(messages[0], "blocking bug") || strings.Contains(messages[0], "full finding detail") {
				t.Fatalf("summary contains finding details: %s", messages[0])
			}
			if target == "no-findings" {
				if !strings.Contains(messages[1], "No findings.") {
					t.Fatal(messages[1])
				}
			} else if !strings.Contains(messages[1], "full finding detail") || !strings.Contains(messages[1], req.ID) {
				t.Fatal(messages[1])
			}
		})
	}
}
