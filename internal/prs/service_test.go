package prs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"saturnus/internal/store"
)

type fakeProvider struct {
	mu      sync.Mutex
	numbers []int
	fetches map[int]int
	fetch   func(context.Context, int) (store.PullRequest, []store.PRCheck, error)
}

func (f *fakeProvider) CurrentUser(context.Context) (string, error) { return "me", nil }
func (f *fakeProvider) Discover(_ context.Context, _ string, author string) ([]int, error) {
	if author != "me" {
		return nil, fmt.Errorf("unexpected author %s", author)
	}
	return f.numbers, nil
}
func (f *fakeProvider) Fetch(ctx context.Context, repo string, n int) (store.PullRequest, []store.PRCheck, error) {
	f.mu.Lock()
	if f.fetches == nil {
		f.fetches = map[int]int{}
	}
	f.fetches[n]++
	f.mu.Unlock()
	if f.fetch != nil {
		return f.fetch(ctx, n)
	}
	return store.PullRequest{Repo: repo, Number: n, State: "OPEN", HeadSHA: "head", Title: "PR"}, []store.PRCheck{{Key: "build", Name: "build", Bucket: "pass", HeadSHA: "head"}}, nil
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRefreshBypassAndFailurePreserveCompleteSnapshot(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	provider := &fakeProvider{}
	provider.fetch = func(context.Context, int) (store.PullRequest, []store.PRCheck, error) {
		return store.PullRequest{Repo: "a/b", Number: 1, State: "OPEN", HeadSHA: "head"}, []store.PRCheck{
			{Key: "build", Name: "build", HeadSHA: "head", Bucket: "pass", Status: "completed", Conclusion: "success"},
			{Key: "optional", Name: "optional", HeadSHA: "head", Bucket: "fail", Status: "completed", Conclusion: "failure"},
		}, nil
	}
	cfg := Config{Enabled: true, Repos: []string{"a/b"}, Policy: Policy{Global: []string{"optional"}}}
	svc := New(st, cfg)
	svc.github = provider
	d, err := svc.Refresh(ctx, "https://github.com/A/B/pull/1")
	if err != nil || d.PR.CIState != "PASS" || d.PR.Counts.Bypass != 1 || len(d.Checks) != 2 || d.ChecksStale {
		t.Fatalf("%+v %v", d, err)
	}
	if !d.Checks[1].Bypassed || d.Checks[1].Conclusion != "failure" {
		t.Fatalf("raw failure lost: %+v", d.Checks)
	}
	goodTime, goodID := d.PR.SyncedAt, d.PR.ID
	// Policy changes are reflected from the persisted raw results immediately,
	// without a network request or mutating GitHub's original check outcome.
	plain := New(st, Config{Enabled: true, Repos: []string{"a/b"}})
	noBypass, err := plain.Get(ctx, goodID)
	if err != nil || noBypass.PR.CIState != "FAIL" || noBypass.PR.Counts.Bypass != 0 {
		t.Fatalf("policy not reevaluated: %+v %v", noBypass, err)
	}
	provider.fetch = func(context.Context, int) (store.PullRequest, []store.PRCheck, error) {
		return store.PullRequest{Repo: "a/b", Number: 1, State: "OPEN", HeadSHA: "new-head"}, nil, errors.New("status API unavailable")
	}
	d, err = svc.Refresh(ctx, goodID)
	if err == nil || d.PR.CIState != "UNKNOWN" || !d.ChecksStale || len(d.Checks) != 2 || d.PR.CheckedHeadSHA != "head" || d.PR.HeadSHA != "new-head" || !d.PR.SyncedAt.Equal(goodTime) {
		t.Fatalf("partial fetch overwrote good snapshot: %+v %v", d, err)
	}
	if _, err := svc.Refresh(ctx, "outside/repo#1"); err == nil {
		t.Fatal("whitelist bypassed")
	}
}

func TestSweepMonitorsMoreThanDisplayLimitAndSharesTaskReviewIdentity(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	provider := &fakeProvider{}
	for i := 1; i <= 25; i++ {
		provider.numbers = append(provider.numbers, i)
	}
	thread, err := st.GetOrCreateReviewThread("chat", "message", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := st.CreateReview(store.ReviewRequest{ThreadID: thread.ID, Status: "succeeded", Repo: "a/b", PRNumber: 99})
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(store.Task{Title: "feature", Status: store.TaskStatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AddTaskPR(store.TaskPR{TaskID: task.ID, Repo: "a/b", PRNumber: 99}); err != nil {
		t.Fatal(err)
	}
	svc := New(st, Config{Enabled: true, Repos: []string{"a/b"}})
	svc.github = provider
	r, err := svc.Sweep(ctx)
	if err != nil || len(r.Errors) != 0 || r.Refreshed != 26 {
		t.Fatalf("%+v %v", r, err)
	}
	rows, err := svc.List(ctx, false)
	if err != nil || len(rows) != 26 {
		t.Fatalf("monitor truncated: %d %v", len(rows), err)
	}
	pr, err := st.GetPullRequestByNumber(ctx, "a/b", 99)
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc.Get(ctx, pr.ID)
	if err != nil || d.ReviewID != review.ID || len(d.TaskIDs) != 1 || d.TaskIDs[0] != task.ID {
		t.Fatalf("links missing: %+v %v", d, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for n, calls := range provider.fetches {
		if calls != 1 {
			t.Fatalf("PR %d fetched %d times", n, calls)
		}
	}
}

func TestClosedPRStopsPollingAndDiscoveryCanReopenIt(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	provider := &fakeProvider{fetch: func(context.Context, int) (store.PullRequest, []store.PRCheck, error) {
		return store.PullRequest{Repo: "a/b", Number: 1, State: "MERGED", HeadSHA: "head"}, nil, nil
	}}
	svc := New(st, Config{Enabled: true, Repos: []string{"a/b"}})
	svc.github = provider
	d, err := svc.Refresh(ctx, "a/b#1")
	if err != nil || d.PR.State != "MERGED" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if provider.fetches[1] != 1 {
		t.Fatal("closed PR polled again")
	}
	provider.numbers = []int{1}
	provider.fetch = nil
	if _, err := svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	d, err = svc.Get(ctx, d.PR.ID)
	if err != nil || d.PR.State != "OPEN" || provider.fetches[1] != 2 {
		t.Fatalf("reopen was missed: %+v %v", d, err)
	}
}

func TestPausePersistsThroughDiscoveryAndManualRefresh(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	provider := &fakeProvider{numbers: []int{1}}
	svc := New(st, Config{Enabled: true, Repos: []string{"a/b"}})
	svc.github = provider
	d, err := svc.Refresh(ctx, "a/b#1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SetMonitoring(ctx, d.PR.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if provider.fetches[1] != 1 {
		t.Fatal("discovery resumed a paused PR")
	}
	d, err = svc.Refresh(ctx, d.PR.ID)
	if err != nil || d.PR.Monitoring {
		t.Fatalf("manual refresh changed pause: %+v %v", d, err)
	}
	if _, err = svc.SetMonitoring(ctx, d.PR.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if provider.fetches[1] != 3 {
		t.Fatal(provider.fetches)
	}
}

func TestMonitorShutdownCancelsGitHubAndCheckpointsFailure(t *testing.T) {
	st := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	provider := &fakeProvider{numbers: []int{1}, fetch: func(ctx context.Context, _ int) (store.PullRequest, []store.PRCheck, error) {
		close(started)
		<-ctx.Done()
		return store.PullRequest{}, nil, ctx.Err()
	}}
	svc := New(st, Config{Enabled: true, Context: ctx, Repos: []string{"a/b"}, RefreshInterval: time.Hour})
	svc.github = provider
	stop := svc.Start(ctx)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor did not start")
	}
	finished := make(chan struct{})
	go func() { stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("monitor shutdown blocked")
	}
	pr, err := st.GetPullRequestByNumber(context.Background(), "a/b", 1)
	if err != nil || pr.CIState != "UNKNOWN" || pr.SyncError == "" {
		t.Fatalf("cancellation not recorded: %+v %v", pr, err)
	}
}

func TestParseRefRejectsInvalidOrExternalReferences(t *testing.T) {
	for _, ref := range []string{"a/b#0", "a/../b#1", "https://example.com/a/b/pull/1", "https://github.com/a/b/issues/1", "a/b#1#2"} {
		if _, _, err := ParseRef(ref); err == nil {
			t.Fatalf("accepted %s", ref)
		}
	}
	for _, ref := range []string{"A/B#12", "https://github.com/A/B/pull/12/files?x=1"} {
		repo, n, err := ParseRef(ref)
		if err != nil || repo != "a/b" || n != 12 {
			t.Fatalf("%s -> %s %d %v", ref, repo, n, err)
		}
	}
}
