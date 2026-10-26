package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/store"
)

func refreshFixture(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	gh, calls, fail := filepath.Join(dir, "gh"), filepath.Join(dir, "calls"), filepath.Join(dir, "fail")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + calls + `"
case "$1" in
issue) printf '%s\n' '{"number":10,"title":"Issue title","state":"OPEN","url":"https://github.com/a/b/issues/10"}' ;;
api) printf '%s\n' '[{"event":"cross-referenced","source":{"issue":{"number":7,"repository_url":"https://api.github.com/repos/a/b","pull_request":{}}}}]' ;;
pr) if [ -f "` + fail + `" ]; then exit 1; fi
 printf '%s\n' '{"number":7,"title":"PR title","state":"OPEN","url":"https://github.com/a/b/pull/7","baseRefName":"main","headRefOid":"new-head"}' ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(gh, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return st, gh, calls, fail
}

func trackedTask(t *testing.T, st *store.Store, status string) store.Task {
	t.Helper()
	task, err := st.CreateTask(store.Task{Title: "Operator title", Status: status, IssueRepo: "a/b", IssueNumber: 10, IssueURL: "https://github.com/a/b/issues/10"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestSweepActiveScopeDeduplicatesPRAndSnapshotsAfterReview(t *testing.T) {
	st, gh, calls, _ := refreshFixture(t)
	open := trackedTask(t, st, "open")
	progress := trackedTask(t, st, "in_progress")
	trackedTask(t, st, "done")
	trackedTask(t, st, "archived")
	thread, err := st.GetOrCreateReviewThread("chat", "msg", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := st.CreateReview(store.ReviewRequest{ThreadID: thread.ID, Repo: "a/b", PRNumber: 7, Status: "succeeded", HeadCommit: "reviewed-head", TaskID: "fs-task"})
	if err != nil {
		t.Fatal(err)
	}
	syncs := 0
	svc := New(st, Config{Enabled: true, GHCli: gh, Repos: map[string]string{"a/b": "unused"}, SyncReview: func(ctx context.Context, repo string, num int, info PRInfo) error {
		syncs++
		if repo != "a/b" || num != 7 || info.HeadCommit != "new-head" {
			t.Fatalf("wrong sync input: %s %d %+v", repo, num, info)
		}
		review.Status = "archived"
		_, err := st.UpdateReview(review)
		return err
	}})
	results, err := svc.Sweep(context.Background(), true)
	if err != nil || len(results) != 2 || syncs != 1 {
		t.Fatalf("results=%+v syncs=%d err=%v", results, syncs, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "pr view") != 1 {
		t.Fatalf("PR fetched repeatedly:\n%s", data)
	}
	for _, id := range []string{open.ID, progress.ID} {
		task, _ := st.GetTask(id)
		prs := st.ListTaskPRs(id)
		if task.RefreshedAt.IsZero() || task.Title != "Operator title" || len(prs) != 1 || prs[0].ReviewStatus != "archived" || prs[0].FsTaskID != "fs-task" {
			t.Fatalf("bad snapshot: %+v %+v", task, prs)
		}
	}
	// Manual sweeps also include done, while preserving archived exclusion.
	manual, err := svc.Sweep(context.Background(), false)
	if err != nil || len(manual) != 3 {
		t.Fatalf("manual=%+v err=%v", manual, err)
	}
}

func TestPartialRefreshPreservesSuccessTimestampsAndRecovers(t *testing.T) {
	st, gh, _, fail := refreshFixture(t)
	task := trackedTask(t, st, "open")
	svc := New(st, Config{Enabled: true, GHCli: gh, Repos: map[string]string{"a/b": "unused"}})
	first, err := svc.RefreshContext(context.Background(), task.ID)
	if err != nil || first.Outcome != "success" || first.Task.RefreshedAt.IsZero() {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := os.WriteFile(fail, []byte("fail"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := svc.RefreshContext(context.Background(), task.ID)
	if err != nil || second.Outcome != "partial" || second.Task.RefreshError == "" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if !second.Task.RefreshedAt.Equal(first.Task.RefreshedAt) || !second.PRs[0].SyncedAt.Equal(first.PRs[0].SyncedAt) || second.PRs[0].PRTitle != "PR title" || second.PRs[0].SyncError == "" {
		t.Fatalf("lost prior success: %+v", second)
	}
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	third, err := svc.RefreshContext(context.Background(), task.ID)
	if err != nil || third.Outcome != "success" || third.Task.RefreshError != "" || third.PRs[0].SyncError != "" {
		t.Fatalf("third=%+v err=%v", third, err)
	}
}

func TestRefreshPreservesConcurrentEditsAndClosure(t *testing.T) {
	for _, closeTask := range []bool{false, true} {
		t.Run(map[bool]string{false: "edit", true: "close"}[closeTask], func(t *testing.T) {
			st, gh, _, _ := refreshFixture(t)
			task := trackedTask(t, st, "open")
			svc := New(st, Config{Enabled: true, GHCli: gh, Repos: map[string]string{"a/b": "unused"}, SyncReview: func(context.Context, string, int, PRInfo) error {
				title, desc := "Edited title", "Edited notes"
				if _, err := st.PatchTask(task.ID, &title, nil, &desc); err != nil {
					return err
				}
				if closeTask {
					_, err := st.CloseTask(task.ID)
					return err
				}
				return nil
			}})
			result, err := svc.RefreshContext(context.Background(), task.ID)
			saved, _ := st.GetTask(task.ID)
			if saved.Title != "Edited title" || saved.Description != "Edited notes" {
				t.Fatalf("operator edit overwritten: %+v", saved)
			}
			if closeTask {
				if !errors.Is(err, store.ErrNotFound) || saved.Status != "archived" || saved.ArchivedAt.IsZero() {
					t.Fatalf("closure overwritten: %+v result=%+v err=%v", saved, result, err)
				}
			} else if err != nil || result.Outcome != "success" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestSchedulerImmediateSerialScansAndDisabled(t *testing.T) {
	st, gh, calls, _ := refreshFixture(t)
	trackedTask(t, st, "open")
	svc := New(st, Config{Enabled: true, GHCli: gh, Repos: map[string]string{"a/b": "unused"}, RefreshInterval: time.Hour})
	waits := 0
	svc.run(context.Background(), func(ctx context.Context, d time.Duration) bool {
		waits++
		if d != time.Hour {
			t.Fatalf("interval=%s", d)
		}
		data, _ := os.ReadFile(calls)
		if strings.Count(string(data), "pr view") != waits {
			t.Fatalf("scan was not finished before waiting: %s", data)
		}
		return waits < 2
	})
	if waits != 2 {
		t.Fatalf("waits=%d", waits)
	}
	New(nil, Config{Enabled: true, RefreshInterval: 0}).run(context.Background(), func(context.Context, time.Duration) bool { t.Fatal("disabled scheduler waited"); return false })
	New(nil, Config{Enabled: false, RefreshInterval: time.Hour}).run(context.Background(), func(context.Context, time.Duration) bool { t.Fatal("disabled module waited"); return false })
}

func TestSchedulerStopCancelsInFlightGitHubAndJoins(t *testing.T) {
	st, gh, _, _ := refreshFixture(t)
	task := trackedTask(t, st, "open")
	ready := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf ready > '"+ready+"'\nexec sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, Config{Enabled: true, GHCli: gh, Repos: map[string]string{"a/b": "unused"}, RefreshInterval: time.Hour})
	stop := svc.Start(context.Background())
	defer stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("GitHub never started")
		case <-ticker.C:
		}
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not cancel and join")
	}
	saved, err := st.GetTask(task.ID)
	if err != nil || saved.RefreshError == "" || !saved.RefreshedAt.IsZero() {
		t.Fatalf("missing cancellation checkpoint: %+v %v", saved, err)
	}
}

func TestRefreshPrioritizesUnattemptedPRs(t *testing.T) {
	st, gh, _, _ := refreshFixture(t)
	task := trackedTask(t, st, "open")
	if _, err := st.AddTaskPR(store.TaskPR{TaskID: task.ID, Repo: "a/b", PRNumber: 1, SyncAttemptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	order := []int{}
	svc := New(st, Config{Enabled: true, GHCli: gh, Repos: map[string]string{"a/b": "unused"}, SyncReview: func(ctx context.Context, repo string, num int, info PRInfo) error {
		order = append(order, num)
		return nil
	}})
	if _, err := svc.RefreshContext(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != 7 || order[1] != 1 {
		t.Fatalf("unattempted PRs did not resume first: %v", order)
	}
}
