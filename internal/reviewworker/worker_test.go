package reviewworker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/ghinfo"
	"saturnus/internal/repopolicy"
	"saturnus/internal/review"
	"saturnus/internal/workerproto"
)

func TestHeartbeatRefreshesGitHubMetadataAndPreservesExpiryOnFailure(t *testing.T) {
	expiry := time.Now().UTC().Add(6 * 24 * time.Hour)
	initial := &ghinfo.Info{Version: "2.86.0", TokenExpiresAt: &expiry, ExpiryStatus: "known", ExpirySource: "github_response_header"}
	w := New(Config{GH: true, GitHub: initial})
	if got := w.heartbeat().GitHub; got == nil || !got.TokenExpiresAt.Equal(expiry) {
		t.Fatalf("initial metadata missing: %+v", got)
	}
	failure := &ghinfo.Info{Version: "2.86.0", ExpiryStatus: "check_failed"}
	w.updateGitHub(failure)
	if got := w.heartbeat().GitHub; got.ExpiryStatus != "check_failed" || !got.TokenExpiresAt.Equal(expiry) {
		t.Fatalf("probe failure erased known expiry: %+v", got)
	}
	renewed := expiry.Add(30 * 24 * time.Hour)
	w.updateGitHub(&ghinfo.Info{Version: "2.87.0", TokenExpiresAt: &renewed, ExpiryStatus: "known"})
	if got := w.heartbeat().GitHub; got.Version != "2.87.0" || !got.TokenExpiresAt.Equal(renewed) {
		t.Fatalf("renewal was not reported: %+v", got)
	}
	if !initial.TokenExpiresAt.Equal(expiry) {
		t.Fatal("new probe mutated an old heartbeat snapshot")
	}
}

func TestWorkerRepoCollisions(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		policy                           repopolicy.Policy
		reaction, cancelFirst, failFirst bool
	}{
		{name: "default mutex"},
		{name: "mutex cancel", policy: repopolicy.Mutex, cancelFirst: true},
		{name: "mutex failure and action", policy: repopolicy.Mutex, reaction: true, failFirst: true},
		{name: "worktree reviews", policy: repopolicy.Worktree},
		{name: "worktree action", policy: repopolicy.Worktree, reaction: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("REVIEW_TEST_STATE", state)
			checkout, other := t.TempDir(), t.TempDir()
			for _, dir := range []string{checkout, other} {
				if err := os.Mkdir(filepath.Join(dir, ".git"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			tool := filepath.Join(t.TempDir(), "tool")
			script := `#!/bin/sh
if [ "$1" = session ]; then echo '[]'; exit 0; fi
if [ "$1" = --version ]; then echo 1.19.0; exit 0; fi
if [ "$1" = export ]; then
  printf '{"info":{"id":"ses_two","title":"saturnus-review-two","directory":"%s"}}\n' "$PWD"
  exit 0
fi
previous=
for arg in "$@"; do
  if [ "$previous" = --title ]; then id=${arg#saturnus-review-}; fi
  previous=$arg
done
printf '%s\n' "$@" > "$REVIEW_TEST_STATE/$id.args"
touch "$REVIEW_TEST_STATE/$id.started"
while [ ! -f "$REVIEW_TEST_STATE/$id.release" ]; do sleep 0.01; done
if [ -f "$REVIEW_TEST_STATE/$id.fail" ]; then exit 7; fi
`
			if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			w := New(Config{Tool: tool, Repos: map[string]string{"a/b": checkout, "c/d": other}, MaxConcurrent: 3, RepoCollisionPolicy: tc.policy, LogDir: t.TempDir()})
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() { cancel(); waitWorker(t, func() bool { return w.heartbeat().InFlight == 0 }) })
			job := func(id, repo string) workerproto.JobPayload {
				return workerproto.JobPayload{ReviewID: id, Repo: repo, CommandTemplate: review.DefaultCommandTemplate, Timeout: 5 * time.Second}
			}
			started := func(id string) bool { _, err := os.Stat(filepath.Join(state, id+".started")); return err == nil }
			release := func(id string) {
				if err := os.WriteFile(filepath.Join(state, id+".release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			w.dispatch(ctx, job("one", "a/b"))
			waitWorker(t, func() bool { return started("one") })
			second := job("two", "a/b")
			if tc.reaction {
				second.Kind, second.ReactionID, second.SessionID, second.Instruction = workerproto.JobReaction, "act_two", "ses_two", `Do the requested action with "quotes"`
			}
			w.dispatch(ctx, second)
			w.dispatch(ctx, job("three", "c/d"))
			waitWorker(t, func() bool { return started("three") })
			if tc.policy == repopolicy.Worktree {
				waitWorker(t, func() bool { return started("two") })
				for _, id := range []string{"one", "two"} {
					args, err := os.ReadFile(filepath.Join(state, id+".args"))
					if err != nil || !strings.Contains(string(args), "git worktree add --detach") || !strings.Contains(string(args), filepath.Join(".saturnus-worktrees", filepath.Base(checkout), id)) {
						t.Fatalf("isolation missing: %s %v", args, err)
					}
				}
			} else {
				deadline := time.Now().Add(80 * time.Millisecond)
				for time.Now().Before(deadline) {
					if started("two") {
						t.Fatal("same repo tools ran concurrently")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if tc.failFirst {
				if err := os.WriteFile(filepath.Join(state, "one.fail"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.cancelFirst {
				w.cancel("one")
			} else {
				release("one")
			}
			waitWorker(t, func() bool { return started("two") })
			release("two")
			release("three")
			waitWorker(t, func() bool { return w.heartbeat().InFlight == 0 })
		})
	}
}

func waitWorker(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("worker condition timed out")
}

func TestDuplicateClaimsPreserveRunningJobAndCancellation(t *testing.T) {
	dir := t.TempDir()
	w := New(Config{Repos: map[string]string{"a/b": dir}, MaxConcurrent: 3})
	unlock, err := w.repoGuard.Lock(context.Background(), repopolicy.Mutex, "a/b", dir, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := workerproto.JobPayload{ReviewID: "rvw_one", RunID: "run_one", Repo: "a/b"}
	w.dispatch(ctx, p)
	w.dispatch(ctx, p) // exact redelivery is idempotent
	p.RunID = "run_new"
	w.dispatch(ctx, p) // another run must be rejected, not replace the old job
	w.mu.Lock()
	running := w.inFlight[p.ReviewID]
	count := len(w.inFlight)
	w.mu.Unlock()
	if count != 1 || len(w.sem) != 1 || running.runID != "run_one" {
		t.Fatalf("duplicate claim replaced running job: count=%d job=%+v", count, running)
	}
	w.cancel(p.ReviewID)
	waitWorker(t, func() bool { return w.heartbeat().InFlight == 0 })
}
