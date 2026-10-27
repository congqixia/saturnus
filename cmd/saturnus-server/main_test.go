package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
	"saturnus/internal/tasks"
)

func TestServeUntilShutdown(t *testing.T) {
	srv := newFakeHTTPServer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(ctx, srv, time.Second)
	}()

	<-srv.started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveUntilShutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveUntilShutdown did not return")
	}

	select {
	case <-srv.shutdownCalled:
	default:
		t.Fatal("Shutdown was not called")
	}
	select {
	case <-srv.closeCalled:
		t.Fatal("Close was called after a successful graceful shutdown")
	default:
	}
}

func TestServeUntilShutdownForcesCloseAfterShutdownFailure(t *testing.T) {
	srv := newFakeHTTPServer()
	srv.shutdownErr = errors.New("shutdown failed")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(ctx, srv, time.Second)
	}()

	<-srv.started
	cancel()

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, srv.shutdownErr) {
			t.Fatalf("serveUntilShutdown() error = %v, want shutdown error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serveUntilShutdown did not return")
	}

	select {
	case <-srv.closeCalled:
	default:
		t.Fatal("Close was not called after Shutdown failed")
	}
}

func TestBotAllowed(t *testing.T) {
	s := &server{botUsers: []string{"ou_admin", "*"}}
	if !s.botAllowed("ou_admin") {
		t.Fatal("expected listed user allowed")
	}
	if !s.botAllowed("ou_anyone") {
		t.Fatal("expected wildcard allow all")
	}
	if s.botAllowed("") {
		t.Fatal("expected empty sender rejected")
	}

	restricted := &server{botUsers: []string{"ou_only"}}
	if !restricted.botAllowed("ou_only") {
		t.Fatal("expected listed user allowed")
	}
	if restricted.botAllowed("ou_other") {
		t.Fatal("expected unlisted user rejected")
	}

	denyAll := &server{}
	if denyAll.botAllowed("ou_anyone") {
		t.Fatal("expected empty allowlist to reject everyone")
	}
}

func TestShouldRespondGroupMentionGating(t *testing.T) {
	s := &server{botOpenID: "ou_bot"}

	p2p := review.MessageContext{ChatType: "p2p"}
	if !s.shouldRespond(p2p) {
		t.Fatal("expected p2p message to be handled")
	}

	unknown := review.MessageContext{}
	if !s.shouldRespond(unknown) {
		t.Fatal("expected message without chat_type to be handled")
	}

	noMention := review.MessageContext{ChatType: "group", Mentions: nil}
	if s.shouldRespond(noMention) {
		t.Fatal("expected group message without bot mention to be ignored")
	}

	atAll := review.MessageContext{ChatType: "group", Mentions: []string{"all"}}
	if s.shouldRespond(atAll) {
		t.Fatal("expected @all to not count as a bot mention")
	}

	atOther := review.MessageContext{ChatType: "group", Mentions: []string{"ou_other"}}
	if s.shouldRespond(atOther) {
		t.Fatal("expected mention of another user to not count")
	}

	atAllAndBot := review.MessageContext{ChatType: "group", Mentions: []string{"all", "ou_bot"}}
	if !s.shouldRespond(atAllAndBot) {
		t.Fatal("expected direct bot mention to count even alongside @all")
	}

	atBot := review.MessageContext{ChatType: "group", Mentions: []string{"ou_bot"}}
	if !s.shouldRespond(atBot) {
		t.Fatal("expected direct bot mention to trigger handling")
	}
}

func TestShouldRespondIgnoresGroupWhenBotUnknown(t *testing.T) {
	s := &server{} // botOpenID empty
	group := review.MessageContext{ChatType: "group", Mentions: []string{"ou_anyone"}}
	if s.shouldRespond(group) {
		t.Fatal("expected group message to be ignored when bot open_id is unknown")
	}
	if !s.shouldRespond(review.MessageContext{ChatType: "p2p"}) {
		t.Fatal("expected p2p to still be handled when bot open_id is unknown")
	}
}

func TestShouldRespondReportModeSilencesGroup(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, botOpenID: "ou_bot"}

	atBot := review.MessageContext{ChatType: "group", ChatID: "oc_report", Mentions: []string{"ou_bot"}}
	if !s.shouldRespond(atBot) {
		t.Fatal("expected direct bot mention to be handled in default (review) mode")
	}

	if _, err := st.SetChatMode("oc_report", store.ChatModeReport); err != nil {
		t.Fatal(err)
	}
	if s.shouldRespond(atBot) {
		t.Fatal("expected report-mode group to ignore even a direct bot mention")
	}
	p2p := review.MessageContext{ChatType: "p2p", ChatID: "oc_report"}
	if !s.shouldRespond(p2p) {
		t.Fatal("expected p2p messages to remain handled regardless of group mode")
	}
}

func TestShouldRespondReportReviewCombination(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, botOpenID: "ou_bot"}

	if _, err := st.SetChatMode("oc_mix", "report|review"); err != nil {
		t.Fatal(err)
	}
	base := review.MessageContext{ChatType: "group", ChatID: "oc_mix", Mentions: []string{"ou_bot"}}

	// Review requests pass through in report|review.
	reviewMsg := base
	reviewMsg.Text = "review https://github.com/congqixia/saturnus/pull/1"
	if !s.shouldRespond(reviewMsg) {
		t.Fatal("expected PR link to be handled in report|review")
	}
	reviewCmd := base
	reviewCmd.Text = "/sat review https://github.com/congqixia/saturnus/pull/1"
	if !s.shouldRespond(reviewCmd) {
		t.Fatal("expected /sat review to be handled in report|review")
	}
	retryCmd := base
	retryCmd.Text = "/sat retry rvw_abc"
	if !s.shouldRespond(retryCmd) {
		t.Fatal("expected /sat retry to be handled in report|review")
	}

	// Ordinary @chatter and non-review commands stay silent.
	chatter := base
	chatter.Text = "hello bot"
	if s.shouldRespond(chatter) {
		t.Fatal("expected casual chatter to be ignored in report|review")
	}
	sessionsCmd := base
	sessionsCmd.Text = "/sat sessions"
	if s.shouldRespond(sessionsCmd) {
		t.Fatal("expected non-review /sat command to be ignored in report|review")
	}

	// report|review still requires a direct mention: a PR link without @ is ignored.
	noMention := review.MessageContext{ChatType: "group", ChatID: "oc_mix", Text: "https://github.com/congqixia/saturnus/pull/1"}
	if s.shouldRespond(noMention) {
		t.Fatal("expected PR link without a bot mention to be ignored")
	}

	// Plain report mode (no review flag) ignores even review requests.
	if _, err := st.SetChatMode("oc_report", store.ChatModeReport); err != nil {
		t.Fatal(err)
	}
	reportReview := review.MessageContext{
		ChatType: "group", ChatID: "oc_report", Mentions: []string{"ou_bot"},
		Text: "review https://github.com/congqixia/saturnus/pull/1",
	}
	if s.shouldRespond(reportReview) {
		t.Fatal("expected report-only group to ignore even review requests")
	}
}

func TestGroupModeReplyInGroup(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, lark: lark.New(lark.Config{})} // lark disabled

	msg := review.MessageContext{ChatType: "group", ChatID: "oc_group", SenderOpenID: "ou_admin"}
	reply := s.groupReply([]string{"/sat", "group", "mode", "report"}, "/sat group mode report", msg)
	if !strings.Contains(reply.text, "oc_group") || !strings.Contains(reply.text, "report") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	if got := st.GetChatMode("oc_group"); got != store.ChatModeReport {
		t.Fatalf("mode = %q, want report", got)
	}

	reply = s.groupReply([]string{"/sat", "group", "mode", "review"}, "/sat group mode review", msg)
	if !strings.Contains(reply.text, "review") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	if got := st.GetChatMode("oc_group"); got != store.ChatModeReview {
		t.Fatalf("mode = %q, want review", got)
	}
}

func TestGroupModeReplyExplicitIDOutsideGroup(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, lark: lark.New(lark.Config{})}

	msg := review.MessageContext{ChatType: "p2p", SenderOpenID: "ou_admin"}
	reply := s.groupReply([]string{"/sat", "group", "mode", "report", "oc_remote"}, "/sat group mode report oc_remote", msg)
	if !strings.Contains(reply.text, "oc_remote") || !strings.Contains(reply.text, "report") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	if got := st.GetChatMode("oc_remote"); got != store.ChatModeReport {
		t.Fatalf("mode = %q, want report", got)
	}
}

func TestGroupModeReplyInvalid(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, lark: lark.New(lark.Config{})}

	msg := review.MessageContext{ChatType: "p2p", SenderOpenID: "ou_admin"}
	if reply := s.groupReply([]string{"/sat", "group", "mode", "bogus"}, "/sat group mode bogus", msg); !strings.Contains(reply.text, "Invalid mode") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	// no chat_id and not in a group
	if reply := s.groupReply([]string{"/sat", "group", "mode", "report"}, "/sat group mode report", msg); !strings.Contains(reply.text, "chat_id") {
		t.Fatalf("expected usage error, got: %q", reply.text)
	}
}

func TestGroupModeReplyCombinedFlags(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, lark: lark.New(lark.Config{})}

	msg := review.MessageContext{ChatType: "group", ChatID: "oc_group", SenderOpenID: "ou_admin"}
	reply := s.groupReply([]string{"/sat", "group", "mode", "review|report"}, "/sat group mode review|report", msg)
	if !strings.Contains(reply.text, "report|review") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	if got := st.GetChatMode("oc_group"); got != "report|review" {
		t.Fatalf("mode = %q, want report|review", got)
	}
}

func TestGroupReplyShowsMode(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, lark: lark.New(lark.Config{})}

	inGroup := review.MessageContext{ChatType: "group", ChatID: "oc_g"}
	reply := s.groupReply([]string{"/sat", "group"}, "/sat group", inGroup)
	if !strings.Contains(reply.text, "oc_g") || !strings.Contains(reply.text, "review") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}

	reply = s.groupReply([]string{"/sat", "group", "oc_other"}, "/sat group oc_other", inGroup)
	if !strings.Contains(reply.text, "oc_other") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
}

func TestGroupsReplyRequiresLark(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &server{store: st, lark: lark.New(lark.Config{})}
	if reply := s.groupsReply(); !strings.Contains(reply.text, "lark client disabled") {
		t.Fatalf("expected disabled lark error, got: %q", reply.text)
	}
}

func TestReviewUsageExampleNoSat(t *testing.T) {
	s := &server{}
	msg := s.reviewUsageExample()
	if msg == "" {
		t.Fatal("expected non-empty usage example")
	}
	if strings.HasPrefix(msg, "/sat") || strings.Contains(msg, "/sat ") {
		t.Fatalf("usage example must not present /sat commands: %q", msg)
	}
}

func TestFormatReviewMarkdown(t *testing.T) {
	req := store.ReviewRequest{
		ID:            "rvw_01",
		Status:        "failed",
		PRURL:         "https://github.com/owner/repo/pull/42",
		Repo:          "owner/repo",
		PRNumber:      42,
		Title:         "Fix * bug",
		RequesterName: "张三",
		CreatedAt:     time.Date(2026, 9, 17, 10, 29, 0, 0, time.UTC),
		Error:         "timeout [repo#1]",
		ResultText:    "found an issue\n- line 1",
	}
	md := formatReviewMarkdown(req, "opencode", nil)
	for _, want := range []string{
		"**Status**: `failed`",
		"[owner/repo#42](https://github.com/owner/repo/pull/42)",
		"**Title**: Fix \\* bug",
		"**Requester**: 张三",
		"**Error**: timeout \\[repo#1\\]",
		"found an issue",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestPRsReplyListsOpenPRsWithBaseBranch(t *testing.T) {
	gh := filepath.Join(t.TempDir(), "gh")
	content := `#!/bin/sh
printf '%s\n' '[{"number":3,"title":"Fix","headRefName":"fix/x","baseRefName":"release","isDraft":false,"url":"https://github.com/a/b/pull/3","updatedAt":"2026-09-24T10:00:00Z"}]'
`
	if err := os.WriteFile(gh, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, &fakeLarkName{}, review.Config{
		Enabled: true,
		Repos:   map[string]string{"a/b": t.TempDir()},
		GHCli:   gh,
	})
	s := &server{store: st, reviews: svc}
	reply := s.prsReply()
	if reply.table == nil || len(reply.table.Rows) != 1 {
		t.Fatalf("expected single-row table, got %#v", reply.table)
	}
	row := reply.table.Rows[0]
	if row["repo"] != "a/b#3" || row["base"] != "release" || row["head"] != "fix/x" {
		t.Fatalf("unexpected row: %#v", row)
	}
	if !strings.Contains(reply.text, "base=release") {
		t.Fatalf("plain text missing base branch: %q", reply.text)
	}
}

func TestPRsReplyNoWhitelistedRepos(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, &fakeLarkName{}, review.Config{Enabled: true})
	s := &server{store: st, reviews: svc}
	if reply := s.prsReply(); !strings.Contains(reply.text, "No repos whitelisted") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
}

func TestHelpCoversAllCommands(t *testing.T) {
	text := helpText()
	for _, c := range satCommandList {
		if !strings.Contains(text, c.command) {
			t.Fatalf("help text missing command %q", c.command)
		}
	}
	md := helpMarkdown()
	if !strings.Contains(md, "**/sat help**") {
		t.Fatalf("help markdown missing bold command:\n%s", md)
	}
	if len(satCommandList) != len(helpMarkdownLines(md)) {
		t.Fatalf("markdown line count mismatch")
	}
}

func helpMarkdownLines(md string) []string {
	return strings.Split(strings.TrimRight(md, "\n"), "\n")
}

func TestTruncateText(t *testing.T) {
	if got := truncateText("abc", 5); got != "abc" {
		t.Fatalf("expected no truncation, got %q", got)
	}
	got := truncateText("abcdef", 4)
	if !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "...[truncated]") {
		t.Fatalf("unexpected truncation: %q", got)
	}
}

func TestTailText(t *testing.T) {
	if got := tailText("abc", 5); got != "abc" {
		t.Fatalf("expected no truncation, got %q", got)
	}
	got := tailText("0123456789", 4)
	if !strings.HasSuffix(got, "6789") || !strings.Contains(got, "[earlier output omitted]") {
		t.Fatalf("unexpected tail: %q", got)
	}
}

func TestReviewCompleted(t *testing.T) {
	req := store.ReviewRequest{}
	for _, status := range []string{"succeeded", "failed"} {
		req.Status = status
		if !reviewCompleted(req) {
			t.Fatalf("expected status %q to be completed", status)
		}
	}
	for _, status := range []string{"pending", "reviewing", ""} {
		req.Status = status
		if reviewCompleted(req) {
			t.Fatalf("expected status %q to be not completed", status)
		}
	}
	req.Status = "pending"
	req.CompletedAt = time.Now()
	if !reviewCompleted(req) {
		t.Fatal("expected CompletedAt to mark the review completed")
	}
}

func TestFormatReviewMarkdownCompletedShowsFullSummary(t *testing.T) {
	req := store.ReviewRequest{
		ID:         "rvw_c",
		Status:     "succeeded",
		PRURL:      "https://github.com/a/b/pull/1",
		Repo:       "a/b",
		PRNumber:   1,
		ResultText: "line1\nline2\n...\nREVIEW SUMMARY:\nPR summary: fix the race\nIssues:\n- shared err\nReview suggestions: LGTM after fix",
	}
	md := formatReviewMarkdown(req, "opencode", nil)
	for _, want := range []string{"PR summary: fix the race", "shared err", "Review suggestions: LGTM after fix"} {
		if !strings.Contains(md, want) {
			t.Fatalf("completed markdown missing summary %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "line1") {
		t.Fatalf("expected the output head to be omitted, got:\n%s", md)
	}
}

func TestFormatReviewMarkdownRunningShowsTail(t *testing.T) {
	head := strings.Repeat("x", 3000)
	req := store.ReviewRequest{
		ID:         "rvw_r",
		Status:     "reviewing",
		PRURL:      "https://github.com/a/b/pull/1",
		Repo:       "a/b",
		PRNumber:   1,
		ResultText: head + "\nlatest progress here",
	}
	md := formatReviewMarkdown(req, "opencode", nil)
	if !strings.Contains(md, "[earlier output omitted]") {
		t.Fatalf("expected tail marker for running review:\n%s", md)
	}
	if !strings.Contains(md, "latest progress here") {
		t.Fatalf("expected tail content for running review:\n%s", md)
	}
	if strings.Contains(md, strings.Repeat("x", 3000)) {
		t.Fatal("expected the long output head to be omitted")
	}
}

func TestRequesterDisplayName(t *testing.T) {
	if got := requesterDisplayName(store.ReviewRequest{RequesterName: "张三", RequesterOpenID: "ou_x"}); got != "张三" {
		t.Fatalf("expected stored name, got %q", got)
	}
	if got := requesterDisplayName(store.ReviewRequest{RequesterOpenID: "ou_x"}); got != "ou_x" {
		t.Fatalf("expected open_id fallback, got %q", got)
	}
}

// fakeLarkName is a minimal lark client whose GetUserName returns a fixed name,
// so name resolution can be exercised without the real contact API.
type fakeLarkName struct {
	name string
}

func (f *fakeLarkName) GetTaskContext(ctx context.Context, id string) (lark.Task, error) {
	return f.GetTask(id)
}
func (f *fakeLarkName) UpdateTaskContext(ctx context.Context, id string, input lark.UpdateTaskInput) error {
	return f.UpdateTask(id, input)
}
func (f *fakeLarkName) Enabled() bool                      { return true }
func (f *fakeLarkName) GetUserName(string) (string, error) { return f.name, nil }
func (f *fakeLarkName) SendText(_, _, _ string) error      { return nil }
func (f *fakeLarkName) SendTextToUser(_, _ string) error   { return nil }
func (f *fakeLarkName) CreateTask(lark.CreateTaskInput) (lark.Task, error) {
	return lark.Task{}, nil
}
func (f *fakeLarkName) GetTask(string) (lark.Task, error)             { return lark.Task{}, nil }
func (f *fakeLarkName) UpdateTask(string, lark.UpdateTaskInput) error { return nil }
func (f *fakeLarkName) ReplyMessage(_, _ string) error                { return nil }
func (f *fakeLarkName) EnsureTaskList(string) (lark.TaskList, error)  { return lark.TaskList{}, nil }
func (f *fakeLarkName) AddTaskToList(_, _ string) error               { return nil }

func newTestReviewWithName(t *testing.T, name string) (botReply, *server) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID:              "rvw_test",
		ThreadID:        thread.ID,
		Status:          "pending",
		PRURL:           "https://github.com/a/b/pull/1",
		Repo:            "a/b",
		PRNumber:        1,
		RequesterOpenID: "ou_requester",
	}); err != nil {
		t.Fatal(err)
	}
	svc := review.New(st, &fakeLarkName{name: name}, review.Config{Enabled: true})
	return (&server{store: st, reviews: svc}).reviewsReply(""), nil
}

func TestReviewsReplyResolvesRequesterName(t *testing.T) {
	reply, _ := newTestReviewWithName(t, "张三")
	if reply.table == nil || len(reply.table.Rows) != 1 {
		t.Fatalf("expected a single-row table card, got %#v", reply.table)
	}
	if got := reply.table.Rows[0]["requester"]; got != "张三" {
		t.Fatalf("expected resolved requester name, got %v", got)
	}
}

func TestReviewsReplyFallsBackToOpenIDWhenNameUnresolvable(t *testing.T) {
	reply, _ := newTestReviewWithName(t, "")
	if got := reply.table.Rows[0]["requester"]; got != "ou_requester" {
		t.Fatalf("expected open_id fallback when name unresolvable, got %v", got)
	}
}

func TestThreadsReplyListsAllLinkedThreads(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	t1, err := st.GetOrCreateReviewThread("oc_alice", "om_a", "topic-a", []string{"ou_alice"})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := st.GetOrCreateReviewThread("oc_bob", "om_b", "topic-b", []string{"ou_bob", "ou_alice"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_th", ThreadID: t1.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkReviewThread(req.ID, t1.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkReviewThread(req.ID, t2.ID); err != nil {
		t.Fatal(err)
	}

	srv := &server{store: st, reviews: review.New(st, &fakeLarkName{}, review.Config{Enabled: true})}
	reply := srv.threadsReply("rvw_th")
	if reply.table == nil || len(reply.table.Rows) != 2 {
		t.Fatalf("expected a 2-row table card, got %#v", reply.table)
	}
	seenChats := map[string]bool{}
	for _, row := range reply.table.Rows {
		seenChats[row["chat"].(string)] = true
	}
	if !seenChats["oc_alice"] || !seenChats["oc_bob"] {
		t.Fatalf("thread card missing chats: %#v", seenChats)
	}
	if !strings.Contains(reply.text, "oc_alice") || !strings.Contains(reply.text, "topic-b") {
		t.Fatalf("plain-text fallback missing thread info: %s", reply.text)
	}
}

func TestThreadsReplyReviewNotFound(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := &server{store: st, reviews: review.New(st, &fakeLarkName{}, review.Config{Enabled: true})}
	reply := srv.threadsReply("rvw_missing")
	if reply.table != nil || !strings.Contains(reply.text, "not found") {
		t.Fatalf("expected not-found reply, got %#v", reply)
	}
}

func TestReviewsReplyArchivedMode(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_active", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_requester",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_arch", ThreadID: thread.ID, Status: "archived",
		PRURL: "https://github.com/a/b/pull/2", Repo: "a/b", PRNumber: 2,
		RequesterOpenID: "ou_requester",
	}); err != nil {
		t.Fatal(err)
	}
	srv := &server{store: st, reviews: review.New(st, &fakeLarkName{}, review.Config{Enabled: true})}

	// Default list hides archived reviews.
	def := srv.reviewsReply("")
	if len(def.table.Rows) != 1 || def.table.Rows[0]["id"] != "rvw_active" {
		t.Fatalf("default list should hide archived, got %#v", def.table.Rows)
	}
	// Explicit archived mode shows only archived.
	arch := srv.reviewsReply("archived")
	if len(arch.table.Rows) != 1 || arch.table.Rows[0]["id"] != "rvw_arch" {
		t.Fatalf("archived list wrong, got %#v", arch.table.Rows)
	}
	// All mode shows everything.
	all := srv.reviewsReply("all")
	if len(all.table.Rows) != 2 {
		t.Fatalf("all list wrong, got %#v", all.table.Rows)
	}
}

func TestExtractLarkMessageParsesMentionsAndChatType(t *testing.T) {
	payload := map[string]any{
		"header": map[string]any{"event_type": "im.message.receive_v1"},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_id": map[string]any{"open_id": "ou_user"},
			},
			"message": map[string]any{
				"chat_id":      "oc_123",
				"chat_type":    "group",
				"message_id":   "om_123",
				"message_type": "text",
				"mentions": []any{
					map[string]any{"id": "all", "name": "所有人"},
					map[string]any{"id": "ou_bot", "name": "saturnus"},
					map[string]any{"id": "ou_zhang", "name": "张三"},
				},
				"content": `{"text":"/sat sessions"}`,
			},
		},
	}
	msg := extractLarkMessage(payload)
	if msg.ChatType != "group" {
		t.Fatalf("chat_type = %q, want group", msg.ChatType)
	}
	if msg.Text != "/sat sessions" {
		t.Fatalf("text = %q, want /sat sessions", msg.Text)
	}
	if len(msg.Mentions) != 3 || msg.Mentions[0] != "all" || msg.Mentions[1] != "ou_bot" {
		t.Fatalf("mentions = %#v, want [all ou_bot ou_zhang]", msg.Mentions)
	}
	if msg.MentionNames["ou_zhang"] != "张三" || msg.MentionNames["all"] != "所有人" {
		t.Fatalf("mention names = %#v", msg.MentionNames)
	}
}

func TestAttributedRequester(t *testing.T) {
	s := &server{botOpenID: "ou_bot"}
	msg := review.MessageContext{
		Mentions:     []string{"all", "ou_bot", "ou_zhang"},
		MentionNames: map[string]string{"ou_zhang": "张三"},
	}
	if got := s.attributedRequester(msg); got != "ou_zhang" {
		t.Fatalf("attributed = %q, want ou_zhang", got)
	}
	onlyBot := review.MessageContext{Mentions: []string{"ou_bot"}}
	if got := s.attributedRequester(onlyBot); got != "" {
		t.Fatalf("attributed = %q, want empty when only the bot is mentioned", got)
	}
	none := review.MessageContext{}
	if got := s.attributedRequester(none); got != "" {
		t.Fatalf("attributed = %q, want empty without mentions", got)
	}
	atAll := review.MessageContext{Mentions: []string{"all"}}
	if got := s.attributedRequester(atAll); got != "" {
		t.Fatalf("attributed = %q, want empty for @all", got)
	}
}

func TestReviewStatusCellOutdatedKeepsColor(t *testing.T) {
	outdated := store.ReviewRequest{Status: "succeeded", HeadCommit: "abc123", PrHeadCommit: "def456"}
	cell := reviewStatusCell(outdated)
	tag := cell.(map[string]any)
	if tag["text"] != "succeeded / OUTDATED" {
		t.Fatalf("text = %v, want 'succeeded / OUTDATED'", tag["text"])
	}
	if tag["color"] != "green" {
		t.Fatalf("color = %v, want green (status color preserved)", tag["color"])
	}
	current := store.ReviewRequest{Status: "failed", HeadCommit: "abc123", PrHeadCommit: "abc123"}
	cell = reviewStatusCell(current)
	tag = cell.(map[string]any)
	if tag["text"] != "failed" || tag["color"] != "red" {
		t.Fatalf("cell = %#v, want failed/red", tag)
	}
}

func TestMaybeReviewKeepsSenderAsRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_sender"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	s := &server{store: st, lark: lark.New(lark.Config{}), reviews: svc, botOpenID: "ou_bot"}

	// A natural-language request: PR link + @the-person-asked-to-approve + @bot.
	msg := review.MessageContext{
		Text:         "fix: batch clustering by XuanYang-cn · Pull Request #1 · https://github.com/a/b/pull/1 这个PR 请@Reviewer 帮忙approve一下，THX！@Bot",
		ChatID:       "oc_c",
		MessageID:    "om_1",
		SenderOpenID: "ou_sender",
		SenderName:   "Sender",
		Mentions:     []string{"ou_bot", "ou_reviewer"},
		MentionNames: map[string]string{"ou_reviewer": "Reviewer"},
	}
	if reply := s.maybeReview(msg); !strings.Contains(reply, "Review started") {
		t.Fatalf("unexpected reply: %q", reply)
	}
	req, err := st.GetReviewByPR("a/b", 1)
	if err != nil {
		t.Fatal(err)
	}
	if req.RequesterOpenID != "ou_sender" {
		t.Fatalf("requester = %q, want ou_sender (mentions must not be reinterpreted as requester)", req.RequesterOpenID)
	}
	if req.RequesterName != "Sender" {
		t.Fatalf("requester name = %q, want Sender", req.RequesterName)
	}
}

func TestRequesterReplyByMention(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_admin"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	s := &server{store: st, lark: lark.New(lark.Config{}), reviews: svc, botOpenID: "ou_bot"}

	req, err := svc.Submit(review.MessageContext{
		Text: "review https://github.com/a/b/pull/1", ChatID: "oc_c", MessageID: "om_1",
		SenderOpenID: "ou_admin", SenderName: "Admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.RequesterOpenID != "ou_admin" {
		t.Fatalf("initial requester = %q, want ou_admin", req.RequesterOpenID)
	}

	msg := review.MessageContext{
		ChatID: "oc_c", SenderOpenID: "ou_admin",
		Mentions:     []string{"ou_bot", "ou_zhang"},
		MentionNames: map[string]string{"ou_zhang": "张三"},
	}
	reply := s.requesterReply([]string{"/sat", "requester", req.ID}, msg)
	if !strings.Contains(reply.text, req.ID) || !strings.Contains(reply.text, "张三") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	got, err := svc.Get(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequesterOpenID != "ou_zhang" || got.RequesterName != "张三" {
		t.Fatalf("requester = %s (%s), want ou_zhang (张三)", got.RequesterOpenID, got.RequesterName)
	}
}

func TestRequesterReplyByOpenID(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_admin"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	s := &server{store: st, lark: lark.New(lark.Config{}), reviews: svc, botOpenID: "ou_bot"}

	req, err := svc.Submit(review.MessageContext{
		Text: "review https://github.com/a/b/pull/2", ChatID: "oc_c", MessageID: "om_2",
		SenderOpenID: "ou_admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := review.MessageContext{ChatType: "p2p", SenderOpenID: "ou_admin"}
	reply := s.requesterReply([]string{"/sat", "requester", req.ID, "ou_alice"}, msg)
	if !strings.Contains(reply.text, "ou_alice") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
	got, err := svc.Get(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequesterOpenID != "ou_alice" {
		t.Fatalf("requester = %q, want ou_alice", got.RequesterOpenID)
	}
}

func TestRequesterReplyNoUser(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true, Repos: map[string]string{"a/b": t.TempDir()}})
	s := &server{store: st, lark: lark.New(lark.Config{}), reviews: svc, botOpenID: "ou_bot"}
	msg := review.MessageContext{ChatType: "p2p", SenderOpenID: "ou_admin"}
	if reply := s.requesterReply([]string{"/sat", "requester", "rvw_x"}, msg); !strings.Contains(reply.text, "No user specified") {
		t.Fatalf("unexpected reply: %q", reply.text)
	}
}

func TestHandleReviewThreadReplyGating(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	s := &server{store: st, lark: lark.New(lark.Config{}), reviews: svc}

	// Only group thread replies are eligible.
	if s.handleReviewThreadReply(review.MessageContext{ChatType: "p2p"}) {
		t.Fatal("expected p2p messages to be left to normal handling")
	}
	// A message that is its own thread root is not a reply.
	if s.handleReviewThreadReply(review.MessageContext{ChatType: "group", ChatID: "oc_c", MessageID: "om_x", RootMessageID: "om_x"}) {
		t.Fatal("expected a non-reply group message to fall through")
	}
	// A reply containing a PR URL is left to the normal review path.
	if s.handleReviewThreadReply(review.MessageContext{
		ChatType: "group", ChatID: "oc_c", MessageID: "om_y", RootMessageID: "om_root",
		Text: "review https://github.com/a/b/pull/1",
	}) {
		t.Fatal("expected a PR-URL reply to fall through to maybeReview")
	}
	// A thread reply with no active review rooted there falls through.
	if s.handleReviewThreadReply(review.MessageContext{
		ChatType: "group", ChatID: "oc_c", MessageID: "om_y", RootMessageID: "om_unknown",
	}) {
		t.Fatal("expected a reply in an unknown thread to fall through")
	}
}

func TestSatCommandTokenMatch(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{text: "/sat whoami", want: "/sat whoami"},
		{text: "please /sat sessions", want: "/sat sessions"},
		{text: "/sat", want: "/sat"},
		{text: "review https://github.com/congqixia/saturnus/pull/1", want: ""},
		{text: "https://github.com/congqixia/saturnus/pull/1", want: ""},
		{text: "hello bot", want: ""},
	}
	for _, tc := range cases {
		if got := satCommand(tc.text); got != tc.want {
			t.Fatalf("satCommand(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestAckMessagesIncludeCommand(t *testing.T) {
	if got := ackText("/sat refresh all"); !strings.Contains(got, "/sat refresh all") {
		t.Fatalf("ack text should echo the command, got %q", got)
	}
	if got := ackDoneText("/sat refresh all"); !strings.Contains(got, "/sat refresh all") {
		t.Fatalf("done text should echo the command, got %q", got)
	}
}

func newTasksTestServer(t *testing.T, repos map[string]string) (*server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	svc := tasks.New(st, tasks.Config{Enabled: true, GHCli: "no-such-gh", Repos: repos})
	return &server{store: st, tasks: svc}, st
}

func TestTasksReplyListsAndFilters(t *testing.T) {
	srv, st := newTasksTestServer(t, map[string]string{"a/b": "/tmp/a"})
	task, err := srv.tasks.Create("https://github.com/a/b/issues/1")
	if err != nil {
		t.Fatal(err)
	}
	reply := srv.tasksReply("")
	if reply.table == nil || len(reply.table.Rows) != 1 {
		t.Fatalf("expected a single-row task table, got %#v", reply.table)
	}
	if got := reply.table.Rows[0]["issue"]; got != "a/b#1" {
		t.Fatalf("expected issue ref, got %v", got)
	}
	if srv.tasksReply("archived").table != nil {
		t.Fatal("expected no table for archived mode before closing any task")
	}
	if _, err := st.CloseTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if reply := srv.tasksReply(""); reply.table != nil {
		t.Fatal("expected archived task hidden from default listing")
	}
	if reply := srv.tasksReply("archived"); reply.table == nil {
		t.Fatal("expected archived task listed in archived mode")
	}
}

func TestTaskCommandCreateAndDetail(t *testing.T) {
	srv, _ := newTasksTestServer(t, map[string]string{"a/b": "/tmp/a"})
	reply := srv.taskCommand([]string{"/sat", "task", "create", "https://github.com/a/b/issues/9"}, "/sat task create https://github.com/a/b/issues/9")
	if !strings.HasPrefix(reply.text, "Task created tsk_") {
		t.Fatalf("unexpected create reply: %q", reply.text)
	}
	id := strings.Fields(reply.text)[2]
	detail := srv.taskCommand([]string{"/sat", "task", id}, "/sat task "+id)
	if !strings.Contains(detail.text, "a/b#9") {
		t.Fatalf("unexpected detail reply: %q", detail.text)
	}
	if detail.md == nil || !strings.Contains(detail.md.Content, "a/b#9") {
		t.Fatalf("expected a markdown detail card with the issue, got %#v", detail.md)
	}
	missing := srv.taskCommand([]string{"/sat", "task", "tsk_missing"}, "/sat task tsk_missing")
	if !strings.Contains(missing.text, "Task not found") {
		t.Fatalf("expected not-found reply, got %q", missing.text)
	}
}

func TestTaskCommandAddDocAndPR(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "x.md"), []byte("# x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, st := newTasksTestServer(t, map[string]string{"a/b": dir})
	task, err := srv.tasks.Create("https://github.com/a/b/issues/1")
	if err != nil {
		t.Fatal(err)
	}

	docReply := srv.taskCommand([]string{"/sat", "task", "add-doc", task.ID, "a/b:docs/x.md"}, "/sat task add-doc "+task.ID+" a/b:docs/x.md")
	if !strings.HasPrefix(docReply.text, "Doc added tdc_") {
		t.Fatalf("unexpected add-doc reply: %q", docReply.text)
	}

	prReply := srv.taskCommand([]string{"/sat", "task", "add-pr", task.ID, "https://github.com/a/b/pull/1"}, "/sat task add-pr "+task.ID+" https://github.com/a/b/pull/1")
	if !strings.HasPrefix(prReply.text, "PR added tpr_") || !strings.Contains(prReply.text, "review=none") {
		t.Fatalf("unexpected add-pr reply: %q", prReply.text)
	}

	detail := srv.taskDetailReply(task)
	if !strings.Contains(detail.text, "docs:") || !strings.Contains(detail.text, "a/b:docs/x.md") || !strings.Contains(detail.text, "a/b#1") {
		t.Fatalf("detail missing docs/prs:\n%s", detail.text)
	}
	if detail.md == nil {
		t.Fatal("expected a markdown detail card")
	}
	for _, want := range []string{"**Status**", "**Issue**", "**Docs**", "a/b:docs/x.md", "**PRs**", "a/b#1"} {
		if !strings.Contains(detail.md.Content, want) {
			t.Fatalf("detail card missing %q:\n%s", want, detail.md.Content)
		}
	}

	// Archived tasks reject new PRs.
	if _, err := st.CloseTask(task.ID); err != nil {
		t.Fatal(err)
	}
	rejected := srv.taskCommand([]string{"/sat", "task", "add-pr", task.ID, "https://github.com/a/b/pull/2"}, "/sat task add-pr "+task.ID+" https://github.com/a/b/pull/2")
	if !strings.Contains(rejected.text, "archived") {
		t.Fatalf("expected archived rejection, got %q", rejected.text)
	}
}

func TestTaskRefreshReply(t *testing.T) {
	srv, _ := newTasksTestServer(t, map[string]string{"a/b": "/tmp/a"})
	task, err := srv.tasks.Create("https://github.com/a/b/issues/1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.tasks.AddPR(task.ID, "https://github.com/a/b/pull/1"); err != nil {
		t.Fatal(err)
	}
	reply := srv.taskRefreshReply(task.ID)
	if !strings.Contains(reply.text, "Refreshed") || !strings.Contains(reply.text, "prs=1") {
		t.Fatalf("unexpected refresh reply: %q", reply.text)
	}
	if reply.md == nil {
		t.Fatal("expected a markdown refresh card")
	}
	for _, want := range []string{"**Status**", "**Issue**", "**PRs**", "a/b#1"} {
		if !strings.Contains(reply.md.Content, want) {
			t.Fatalf("refresh card missing %q:\n%s", want, reply.md.Content)
		}
	}
	all := srv.taskRefreshReply("all")
	if !strings.Contains(all.text, "Refreshed") {
		t.Fatalf("unexpected refresh-all reply: %q", all.text)
	}
}

type fakeHTTPServer struct {
	started        chan struct{}
	stopped        chan struct{}
	shutdownCalled chan struct{}
	closeCalled    chan struct{}
	shutdownErr    error
	stopOnce       sync.Once
}

func newFakeHTTPServer() *fakeHTTPServer {
	return &fakeHTTPServer{
		started:        make(chan struct{}),
		stopped:        make(chan struct{}),
		shutdownCalled: make(chan struct{}),
		closeCalled:    make(chan struct{}),
	}
}

func (s *fakeHTTPServer) ListenAndServe() error {
	close(s.started)
	<-s.stopped
	return http.ErrServerClosed
}

func (s *fakeHTTPServer) Shutdown(context.Context) error {
	close(s.shutdownCalled)
	if s.shutdownErr != nil {
		return s.shutdownErr
	}
	s.stopOnce.Do(func() { close(s.stopped) })
	return nil
}

func (s *fakeHTTPServer) Close() error {
	close(s.closeCalled)
	s.stopOnce.Do(func() { close(s.stopped) })
	return nil
}

func newReviewingReviewForResult(t *testing.T, st *store.Store, id string) {
	t.Helper()
	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: id, ThreadID: thread.ID, Status: "reviewing",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitReviewResultHandler(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReviewForResult(t, st, "rvw_http")

	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:     true,
		ResultToken: "s3cret",
	})
	s := &server{store: st, reviews: svc}

	body := `{"verdict":"request_change","summary":"Fixes the restore race.","findings":[{"severity":"P0","path":"internal/etcd_restore.go","line":40,"title":"data race","message":"shared err var","suggestion":"use a channel"}]}`
	req, err := http.NewRequest("POST", "/api/reviews/rvw_http/result", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.SetPathValue("review_id", "rvw_http")
	req.Header.Set("Authorization", "Bearer s3cret")
	rr := httptest.NewRecorder()
	s.submitReviewResult(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	got, err := svc.Get("rvw_http")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "request_change" || got.Summary != "Fixes the restore race." {
		t.Fatalf("verdict/summary not stored: %#v", got)
	}
	if findings := svc.ListFindings("rvw_http"); len(findings) != 1 || findings[0].Severity != "P0" {
		t.Fatalf("findings not stored: %#v", findings)
	}
}

func TestSubmitReviewResultHandlerAuthAndValidation(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReviewForResult(t, st, "rvw_auth")

	svc := review.New(st, lark.New(lark.Config{}), review.Config{
		Enabled:     true,
		ResultToken: "s3cret",
	})
	s := &server{store: st, reviews: svc}

	good := `{"verdict":"lgtm","summary":"ok","findings":[]}`
	post := func(auth, body string) *httptest.ResponseRecorder {
		req, err := http.NewRequest("POST", "/api/reviews/rvw_auth/result", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		req.SetPathValue("review_id", "rvw_auth")
		rr := httptest.NewRecorder()
		s.submitReviewResult(rr, req)
		return rr
	}

	if rr := post("", good); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("Bearer wrong", good); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("Bearer s3cret", `{"verdict":"approve","summary":"ok"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid verdict, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("Bearer s3cret", good); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid submission, got %d: %s", rr.Code, rr.Body.String())
	}
	// A duplicate while the review is still running is an idempotent replace.
	if rr := post("Bearer s3cret", good); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for in-progress duplicate submission, got %d: %s", rr.Code, rr.Body.String())
	}
	// Once the run has finished with a recorded result, a late duplicate is
	// rejected so it cannot clobber the outcome.
	req, err := st.GetReview("rvw_auth")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	if rr := post("Bearer s3cret", good); rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate submission after completion, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSubmitReviewResultHandlerRejectsNotRunning(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("oc_chat", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_pending", ThreadID: thread.ID, Status: "pending",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	}); err != nil {
		t.Fatal(err)
	}

	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
	s := &server{store: st, reviews: svc}
	req, err := http.NewRequest("POST", "/api/reviews/rvw_pending/result", strings.NewReader(`{"verdict":"lgtm","summary":"ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.SetPathValue("review_id", "rvw_pending")
	rr := httptest.NewRecorder()
	s.submitReviewResult(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for non-running review, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPostReviewCommentsHandler(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReviewForResult(t, st, "rvw_posthttp")

	svc := review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})
	if err := svc.IngestResult("rvw_posthttp", "lgtm", "ok", []review.Finding{
		{Severity: "P2", Path: "a.go", Line: 2, Title: "nit", Message: "reorder"},
	}); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get("rvw_posthttp")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	// No gh stub configured -> posting surfaces the gh failure, still a
	// non-2xx but not a crash.
	s := &server{store: st, reviews: svc}
	httpReq, err := http.NewRequest("POST", "/api/reviews/rvw_posthttp/post-comments", nil)
	if err != nil {
		t.Fatal(err)
	}
	httpReq.SetPathValue("review_id", "rvw_posthttp")
	rr := httptest.NewRecorder()
	s.postReviewComments(rr, httpReq)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without gh, got %d: %s", rr.Code, rr.Body.String())
	}

	// Missing review -> 404.
	httpReq2, _ := http.NewRequest("POST", "/api/reviews/rvw_nope/post-comments", nil)
	httpReq2.SetPathValue("review_id", "rvw_nope")
	rr2 := httptest.NewRecorder()
	s.postReviewComments(rr2, httpReq2)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing review, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

func TestPostBotCommandRejectsInvalid(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := &server{store: st, reviews: review.New(st, lark.New(lark.Config{}), review.Config{Enabled: true})}

	reply := srv.handleBotCommand("/sat post", review.MessageContext{SenderOpenID: "ou_x"})
	if !strings.Contains(reply.text, "Usage: /sat post") {
		t.Fatalf("expected usage reply, got %q", reply.text)
	}
	reply = srv.handleBotCommand("/sat post rvw_missing", review.MessageContext{SenderOpenID: "ou_x"})
	if !strings.Contains(reply.text, "Post comments failed") {
		t.Fatalf("expected failure reply, got %q", reply.text)
	}
}

func TestTasksRefreshConfig(t *testing.T) {
	t.Setenv("SATURNUS_TASKS_REFRESH_INTERVAL", "")
	t.Setenv("SATURNUS_TASKS_REFRESH_TIMEOUT", "")
	cfg, err := tasksConfig(nil)
	if err != nil || cfg.RefreshInterval != 5*time.Minute || cfg.RefreshTimeout != time.Minute {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	t.Setenv("SATURNUS_TASKS_REFRESH_INTERVAL", "0")
	cfg, err = tasksConfig(nil)
	if err != nil || cfg.RefreshInterval != 0 {
		t.Fatalf("cannot disable: %+v %v", cfg, err)
	}
	for _, value := range []string{"-1s", "invalid"} {
		t.Setenv("SATURNUS_TASKS_REFRESH_INTERVAL", value)
		if _, err := tasksConfig(nil); err == nil {
			t.Fatalf("accepted interval %q", value)
		}
	}
	t.Setenv("SATURNUS_TASKS_REFRESH_INTERVAL", "1m")
	for _, value := range []string{"0", "-1s", "invalid"} {
		t.Setenv("SATURNUS_TASKS_REFRESH_TIMEOUT", value)
		if _, err := tasksConfig(nil); err == nil {
			t.Fatalf("accepted timeout %q", value)
		}
	}
}
