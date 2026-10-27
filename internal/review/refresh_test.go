package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

type refreshLark struct {
	*fakeLark
	updateErr   error
	afterUpdate func()
}

func (f *refreshLark) UpdateTaskContext(ctx context.Context, id string, input lark.UpdateTaskInput) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if err := f.fakeLark.UpdateTaskContext(ctx, id, input); err != nil {
		return err
	}
	if f.afterUpdate != nil {
		f.afterUpdate()
	}
	return nil
}

func refreshStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSyncPRRetriesFeishuCompletionAfterArchive(t *testing.T) {
	st := refreshStore(t)
	newRefreshReview(t, st, "rvw_sync", "succeeded", "fs-task")
	remote := &refreshLark{fakeLark: &fakeLark{enabled: true, task: lark.Task{GUID: "fs-task"}}, updateErr: errors.New("transient remote failure")}
	svc := New(st, remote, Config{Enabled: true})
	info := PRInfo{State: "MERGED", HeadCommit: "new-head"}
	if err := svc.SyncPR(context.Background(), "a/b", 1, info); err == nil {
		t.Fatal("expected completion error")
	}
	first, _ := st.GetReview("rvw_sync")
	if first.Status != "archived" || !first.RefreshedAt.IsZero() {
		t.Fatalf("bad failed checkpoint: %+v", first)
	}
	remote.updateErr = nil
	if err := svc.SyncPR(context.Background(), "a/b", 1, info); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncPR(context.Background(), "a/b", 1, info); err != nil {
		t.Fatal(err)
	}
	if len(remote.taskUpdates) != 1 || !remote.task.Completed {
		t.Fatalf("completion was lost or repeated: %+v", remote)
	}
}

func TestSyncPRRemoteSuccessBeforeLocalConflictDoesNotDuplicateDescription(t *testing.T) {
	st := refreshStore(t)
	newRefreshReview(t, st, "rvw_sync", "succeeded", "fs-task")
	payload := `[{"id":42,"user":"reviewer","state":"COMMENTED","body":"Please fix this","submitted_at":"2026-10-01T00:00:00Z"}]`
	remote := &refreshLark{fakeLark: &fakeLark{enabled: true, task: lark.Task{GUID: "fs-task"}}}
	remote.afterUpdate = func() {
		// Simulate a worker/result write between the external append and local CAS.
		if err := st.SetReviewResult("rvw_sync", "lgtm", "Concurrent result"); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(st, remote, Config{Enabled: true, GHCli: fakeGHRefresh(t, "OPEN", payload)})
	if err := svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "OPEN", HeadCommit: "new-head"}); !errors.Is(err, ErrSyncDeferred) {
		t.Fatalf("expected CAS deferral, got %v", err)
	}
	first, _ := st.GetReview("rvw_sync")
	if !first.TaskSyncedAt.IsZero() || first.Summary != "Concurrent result" {
		t.Fatalf("concurrent result overwritten: %+v", first)
	}
	remote.afterUpdate = nil
	if err := svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "OPEN", HeadCommit: "new-head"}); err != nil {
		t.Fatal(err)
	}
	second, _ := st.GetReview("rvw_sync")
	if len(remote.taskUpdates) != 1 || strings.Count(remote.task.Description, "[github-review:42]") != 1 || second.TaskSyncedAt.IsZero() {
		t.Fatalf("append duplicated or watermark lost: %+v %+v", remote, second)
	}
}

func TestSyncPRDefersRunningAndPreservesReviewedBaseline(t *testing.T) {
	st := refreshStore(t)
	newRefreshReview(t, st, "rvw_sync", "reviewing", "fs-task")
	svc := New(st, &fakeLark{}, Config{Enabled: true})
	if err := svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "MERGED", HeadCommit: "new-head"}); !errors.Is(err, ErrSyncDeferred) {
		t.Fatalf("err=%v", err)
	}
	req, _ := st.GetReview("rvw_sync")
	if req.Status != "reviewing" || !req.RefreshedAt.IsZero() {
		t.Fatalf("running review changed: %+v", req)
	}
	req.Status = "succeeded"
	req.HeadCommit = "reviewed-head"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "OPEN", HeadCommit: "new-head"}); err != nil {
		t.Fatal(err)
	}
	req, _ = st.GetReview("rvw_sync")
	if req.HeadCommit != "reviewed-head" || req.PrHeadCommit != "new-head" || !ReviewOutdated(req) {
		t.Fatalf("baseline lost: %+v", req)
	}
}

func TestSyncPRAndRetrySerializeArchiveTransition(t *testing.T) {
	st := refreshStore(t)
	newRefreshReview(t, st, "rvw_sync", "succeeded", "fs-task")
	entered, release := make(chan struct{}), make(chan struct{})
	remote := &refreshLark{fakeLark: &fakeLark{enabled: true, task: lark.Task{GUID: "fs-task"}}, afterUpdate: func() { close(entered); <-release }}
	svc := New(st, remote, Config{Enabled: true, AllowedUsers: []string{"ou_me"}, GHCli: fakeGHRefresh(t, "OPEN", "[]")})
	synced := make(chan error, 1)
	go func() { synced <- svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "MERGED"}) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("sync never reached remote write")
	}
	retried := make(chan error, 1)
	go func() { _, err := svc.Retry("rvw_sync", MessageContext{SenderOpenID: "ou_me"}); retried <- err }()
	close(release)
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-retried:
		if !errors.Is(err, ErrArchived) {
			t.Fatalf("retry ignored archive: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry blocked")
	}
}

func TestFetchPRReviewsPaginationAndMalformedLaterPage(t *testing.T) {
	page := `[{"id":1,"user":"a","state":"COMMENTED","body":"a","submitted_at":"2026-10-01T00:00:00Z"}]`
	reviews, err := FetchPRReviews(context.Background(), fakeGHRefresh(t, "OPEN", page+"\n"+page), "a/b", 1)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews=%+v err=%v", reviews, err)
	}
	if _, err := FetchPRReviews(context.Background(), fakeGHRefresh(t, "OPEN", page+"\ninvalid"), "a/b", 1); err == nil {
		t.Fatal("later-page failure hidden")
	}
}

// TestSyncPRRotatesFullTaskDescription verifies the fix for the "task
// description is full" deadlock: reviews are persisted durably in the store and
// the description is rebuilt as a bounded, rotating summary instead of failing
// forever.
func TestSyncPRRotatesFullTaskDescription(t *testing.T) {
	st := refreshStore(t)
	newRefreshReview(t, st, "rvw_full", "succeeded", "fs-task")

	// Pre-populate enough large comments that a description holding all of
	// them would exceed the Feishu limit.
	var stored []store.ReviewComment
	desc := "PR review request rvw_full\nhttps://github.com/a/b/pull/1"
	bigBody := strings.Repeat("z", 600)
	for i := 1; i <= 6; i++ {
		submitted := time.Date(2026, 10, i, 12, 0, 0, 0, time.UTC)
		key := fmt.Sprintf("[github-review:%d]", i)
		stored = append(stored, store.ReviewComment{
			Key: key, GithubReviewID: int64(i), User: "reviewer", State: "COMMENTED", Body: bigBody, SubmittedAt: submitted,
		})
		desc += reviewEntryText(store.ReviewComment{Key: key, User: "reviewer", State: "COMMENTED", SubmittedAt: submitted, Body: bigBody})
	}
	if utf8.RuneCountInString(desc) <= lark.TaskDescriptionMaxRunes {
		t.Fatal("test setup must overflow the description limit")
	}
	if _, err := st.SaveReviewComments(context.Background(), "rvw_full", stored); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "fs-task", Description: desc}}
	payload := `[{"id":7,"user":"reviewer","state":"COMMENTED","body":"new comment","submitted_at":"2026-10-07T12:00:00Z"}]`
	svc := New(st, fake, Config{Enabled: true, GHCli: fakeGHRefresh(t, "OPEN", payload)})
	if err := svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "OPEN", HeadCommit: "new-head"}); err != nil {
		t.Fatal(err)
	}

	// All reviews are durably stored; the newest one is present even though the
	// oldest entries rotated out of the description.
	comments, err := st.ListReviewComments(context.Background(), "rvw_full")
	if err != nil || len(comments) != 7 {
		t.Fatalf("comments=%d err=%v", len(comments), err)
	}
	req, _ := st.GetReview("rvw_full")
	if req.TaskSyncedAt.IsZero() {
		t.Fatalf("watermark not advanced: %+v", req)
	}
	if !(utf8.RuneCountInString(fake.task.Description) <= lark.TaskDescriptionMaxRunes) {
		t.Fatalf("description still over the limit: %d runes", utf8.RuneCountInString(fake.task.Description))
	}
	if !strings.Contains(fake.task.Description, "[github-review:7]") || !strings.Contains(fake.task.Description, "new comment") {
		t.Fatalf("newest comment missing from description: %q", fake.task.Description)
	}
	if strings.Contains(fake.task.Description, "[github-review:1]") {
		t.Fatalf("oldest comment not rotated out: %q", fake.task.Description)
	}

	// A second sync with nothing new must not duplicate or error.
	if err := svc.SyncPR(context.Background(), "a/b", 1, PRInfo{State: "OPEN", HeadCommit: "new-head"}); err != nil {
		t.Fatal(err)
	}
	comments, err = st.ListReviewComments(context.Background(), "rvw_full")
	if err != nil || len(comments) != 7 {
		t.Fatalf("comments after resync=%d err=%v", len(comments), err)
	}
}
