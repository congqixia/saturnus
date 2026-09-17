package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/review"
	"saturnus/internal/store"
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
	md := formatReviewMarkdown(req, "opencode")
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
	md := formatReviewMarkdown(req, "opencode")
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
	md := formatReviewMarkdown(req, "opencode")
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

func (f *fakeLarkName) Enabled() bool                      { return true }
func (f *fakeLarkName) GetUserName(string) (string, error) { return f.name, nil }
func (f *fakeLarkName) SendText(_, _, _ string) error      { return nil }
func (f *fakeLarkName) SendTextToUser(_, _ string) error   { return nil }
func (f *fakeLarkName) CreateTask(lark.CreateTaskInput) (lark.Task, error) {
	return lark.Task{}, nil
}
func (f *fakeLarkName) ReplyMessage(_, _ string) error { return nil }

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
	return (&server{store: st, reviews: svc}).reviewsReply(), nil
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
