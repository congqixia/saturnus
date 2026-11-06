package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/repopolicy"
	"saturnus/internal/store"
)

func TestLocalExecutorUsesRepoPolicy(t *testing.T) {
	for _, policy := range []repopolicy.Policy{repopolicy.Mutex, repopolicy.Worktree} {
		t.Run(string(policy), func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "saturnus.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			checkout := t.TempDir()
			if err := os.Mkdir(filepath.Join(checkout, ".git"), 0755); err != nil {
				t.Fatal(err)
			}
			tool := filepath.Join(t.TempDir(), "tool")
			if err := os.WriteFile(tool, []byte("#!/bin/sh\nif [ \"$1\" = session ]; then echo '[]'; else printf '%s\\n' \"$@\"; fi\n"), 0755); err != nil {
				t.Fatal(err)
			}
			svc := New(st, lark.New(lark.Config{}), Config{Enabled: true, Repos: map[string]string{"a/b": checkout}, Tool: tool, GHCli: "true", RepoCollisionPolicy: policy, LogDir: t.TempDir()})
			thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			req, err := st.CreateReview(store.ReviewRequest{ID: "rvw_local", ThreadID: thread.ID, Status: "pending", Repo: "a/b", PRNumber: 1})
			if err != nil {
				t.Fatal(err)
			}
			// Simulate another local job owning this checkout.
			unlock, err := svc.repoGuard.Lock(context.Background(), policy, req.Repo, checkout, "rvw_other")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); svc.process(req.ID) }()
			defer func() {
				svc.cancel()
				if unlock != nil {
					unlock()
				}
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("local executor did not stop")
				}
			}()
			if policy == repopolicy.Mutex {
				select {
				case <-done:
					t.Fatal("local mutex did not wait for the checkout")
				case <-time.After(50 * time.Millisecond):
				}
				fresh, _ := st.GetReview(req.ID)
				if fresh.Status != "pending" || len(st.ListRuns(req.ID)) != 0 {
					t.Fatalf("waiting local job should remain pending: %+v", fresh)
				}
				unlock()
				unlock = nil
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("local execution timed out")
			}
			fresh, _ := st.GetReview(req.ID)
			if fresh.Status != "succeeded" {
				t.Fatalf("local execution failed: %+v", fresh)
			}
			if got := strings.Contains(fresh.ResultText, "git worktree add --detach"); got != (policy == repopolicy.Worktree) {
				t.Fatalf("wrong isolation prompt: %s", fresh.ResultText)
			}
		})
	}
}
