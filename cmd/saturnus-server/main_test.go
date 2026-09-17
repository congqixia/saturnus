package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
