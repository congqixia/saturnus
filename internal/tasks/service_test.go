package tasks

import (
	"os"
	"path/filepath"
	"testing"

	"saturnus/internal/store"
)

func TestParseRepoMap(t *testing.T) {
	m := ParseRepoMap("a/b=/x/c, c/d=/y/d,,bad")
	if len(m) != 2 {
		t.Fatalf("expected 2 repos, got %#v", m)
	}
	if m["a/b"] != "/x/c" || m["c/d"] != "/y/d" {
		t.Fatalf("unexpected map: %#v", m)
	}
}

func TestFindIssueAndPRRequest(t *testing.T) {
	issue, err := FindIssueRequest("please track https://github.com/acme/foo/issues/42 thanks")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Repo != "acme/foo" || issue.IssueNumber != 42 || issue.IssueURL != "https://github.com/acme/foo/issues/42" {
		t.Fatalf("unexpected issue: %#v", issue)
	}
	if _, err := FindIssueRequest("no url here"); err == nil {
		t.Fatal("expected error for missing issue URL")
	}

	pr, err := FindPRRequest("https://github.com/acme/foo/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Repo != "acme/foo" || pr.PRNumber != 7 {
		t.Fatalf("unexpected pr: %#v", pr)
	}
}

func TestServiceDisabled(t *testing.T) {
	svc := New(nil, Config{Enabled: false})
	if _, err := svc.Create("https://github.com/a/b/issues/1"); err != ErrDisabled {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
	if _, err := svc.AddPR("tsk_x", "https://github.com/a/b/pull/1"); err != ErrDisabled {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}
}

func TestServiceCreateRejectsNonWhitelistedRepo(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, Config{Enabled: true, Repos: map[string]string{"a/b": "/tmp/a"}})
	_, err = svc.Create("https://github.com/other/repo/issues/1")
	if err == nil {
		t.Fatal("expected error for non-whitelisted repo")
	}
}

func TestServiceCreateStoresTask(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, Config{Enabled: true, GHCli: "no-such-gh", Repos: map[string]string{"a/b": "/tmp/a"}})
	task, err := svc.Create("https://github.com/a/b/issues/10")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskStatusOpen || task.IssueNumber != 10 || task.IssueRepo != "a/b" {
		t.Fatalf("unexpected task: %#v", task)
	}
	if task.Title == "" {
		t.Fatal("expected a fallback title")
	}
}

func TestServiceAddDocRequiresExistingFileInWhitelistedCheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs", "plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "plan", "foo.md"), []byte("# foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, Config{Enabled: true, GHCli: "no-such-gh", Repos: map[string]string{"a/b": dir}})
	task, err := svc.Create("https://github.com/a/b/issues/1")
	if err != nil {
		t.Fatal(err)
	}

	// Non-whitelisted repo rejected.
	if _, err := svc.AddDoc(task.ID, "other/repo:docs/plan/foo.md"); err == nil {
		t.Fatal("expected error for non-whitelisted doc repo")
	}
	// Missing file rejected.
	if _, err := svc.AddDoc(task.ID, "a/b:docs/plan/missing.md"); err == nil {
		t.Fatal("expected error for missing doc file")
	}
	// Existing file accepted.
	doc, err := svc.AddDoc(task.ID, "a/b:docs/plan/foo.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Path != "docs/plan/foo.md" || doc.Repo != "a/b" {
		t.Fatalf("unexpected doc: %#v", doc)
	}
	// Deduped on re-add.
	if _, err := svc.AddDoc(task.ID, "a/b:docs/plan/foo.md"); err != nil {
		t.Fatal(err)
	}
	if docs := svc.Docs(task.ID); len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %#v", docs)
	}
}

func TestServiceAddPRSnapshotsReviewAndState(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Create a review record to snapshot from.
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_root", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := st.CreateReview(store.ReviewRequest{
		ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/5", Repo: "a/b", PRNumber: 5,
		TaskID: "fs_guid", TaskURL: "https://feishu.task",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := New(st, Config{Enabled: true, GHCli: "no-such-gh", Repos: map[string]string{"a/b": "/tmp/a"}})
	task, err := svc.Create("https://github.com/a/b/issues/2")
	if err != nil {
		t.Fatal(err)
	}
	// PR without a review: review_status none.
	pr, err := svc.AddPR(task.ID, "https://github.com/a/b/pull/1")
	if err != nil {
		t.Fatal(err)
	}
	if pr.ReviewStatus != "none" || pr.ReviewID != "" {
		t.Fatalf("expected no review snapshot, got %#v", pr)
	}
	// PR with a review: status and Feishu task snapshotted.
	pr2, err := svc.AddPR(task.ID, "https://github.com/a/b/pull/5")
	if err != nil {
		t.Fatal(err)
	}
	if pr2.ReviewID != review.ID || pr2.ReviewStatus != "succeeded" || pr2.FsTaskID != "fs_guid" || pr2.FsTaskURL != "https://feishu.task" {
		t.Fatalf("review snapshot mismatch: %#v", pr2)
	}

	// Archived tasks reject new PRs.
	if _, err := svc.Close(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddPR(task.ID, "https://github.com/a/b/pull/3"); err != ErrArchived {
		t.Fatalf("expected ErrArchived, got %v", err)
	}
}

// fakeGHAutoLink writes an executable "gh" stub for the task refresh flow:
// it answers `issue view`, `pr view` and the timeline `api` call so a task
// can auto-discover its linked PR.
func fakeGHAutoLink(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	content := `#!/bin/sh
if [ "$1" = "issue" ] && [ "$3" = "10" ]; then
  printf '%s\n' '{"number":10,"title":"Tracked issue","state":"open","url":"https://github.com/a/b/issues/10"}'
  exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "repos/a/b/issues/10/timeline?per_page=100" ]; then
  printf '%s\n' '[{"event":"cross-referenced","source":{"type":"issue","issue":{"number":7,"repository_url":"https://api.github.com/repos/a/b","pull_request":{"url":"https://api.github.com/repos/a/b/pulls/7"}}}}]'
  exit 0
fi
if [ "$1" = "pr" ] && [ "$3" = "7" ]; then
  printf '%s\n' '{"number":7,"title":"Implement it","state":"OPEN","url":"https://github.com/a/b/pull/7","baseRefName":"main"}'
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 1
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestServiceRefreshAutoLinksLinkedPRs(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, Config{Enabled: true, GHCli: fakeGHAutoLink(t), Repos: map[string]string{"a/b": "/tmp/a"}})
	task, err := svc.Create("https://github.com/a/b/issues/10")
	if err != nil {
		t.Fatal(err)
	}

	_, prs, _, err := svc.Refresh(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].PRNumber != 7 || prs[0].PRTitle != "Implement it" || prs[0].PRState != "OPEN" {
		t.Fatalf("expected auto-linked PR 7, got %#v", prs)
	}

	// A second refresh must not duplicate the already-linked PR.
	_, prs, _, err = svc.Refresh(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].PRNumber != 7 {
		t.Fatalf("expected the linked PR to stay deduped, got %#v", prs)
	}
}

func TestServiceRefreshAutoLinkSkipsExistingPRs(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, Config{Enabled: true, GHCli: fakeGHAutoLink(t), Repos: map[string]string{"a/b": "/tmp/a"}})
	task, err := svc.Create("https://github.com/a/b/issues/10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddPR(task.ID, "https://github.com/a/b/pull/7"); err != nil {
		t.Fatal(err)
	}

	_, prs, _, err := svc.Refresh(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].PRNumber != 7 {
		t.Fatalf("expected the manually linked PR 7, got %#v", prs)
	}
}

func TestServiceRefreshUpdatesSnapshotsAndSuggestsClose(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, Config{Enabled: true, GHCli: "no-such-gh", Repos: map[string]string{"a/b": "/tmp/a"}})
	task, err := svc.Create("https://github.com/a/b/issues/1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddPR(task.ID, "https://github.com/a/b/pull/1"); err != nil {
		t.Fatal(err)
	}
	task.IssueState = "CLOSED"
	if _, err := st.UpdateTask(task); err != nil {
		t.Fatal(err)
	}
	refreshed, prs, suggestion, err := svc.Refresh(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.IssueState != "CLOSED" {
		t.Fatalf("expected CLOSED issue state, got %#v", refreshed)
	}
	if len(prs) != 1 {
		t.Fatalf("expected 1 PR, got %#v", prs)
	}
	if suggestion == "" {
		t.Fatal("expected close suggestion for closed issue")
	}
	if prs[0].SyncedAt.IsZero() {
		t.Fatal("expected synced_at to be set after refresh")
	}
}
