package review

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

func TestServiceSubmitFlow(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"congqixia/saturnus": t.TempDir()},
	})
	if !svc.Enabled() {
		t.Fatal("expected review service enabled")
	}

	req, err := svc.Submit(MessageContext{
		Text:         "please review PR https://github.com/congqixia/saturnus/pull/42",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" || req.Repo != "congqixia/saturnus" || req.PRNumber != 42 {
		t.Fatalf("unexpected review: %#v", req)
	}

	got, err := svc.Get(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID == "" || got.ChatID != "oc_chat" {
		t.Fatalf("thread context missing: %#v", got)
	}

	list := svc.List("pending", false)
	if len(list) != 1 {
		t.Fatalf("expected 1 pending review, got %d", len(list))
	}
	if svc.FormatList(list) == "" || svc.FormatOne(got) == "" {
		t.Fatal("expected non-empty formatted output")
	}
}

func TestServiceSubmitAttributedRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_admin"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	req, err := svc.Submit(MessageContext{
		Text:            "review https://github.com/a/b/pull/9 @zhang",
		ChatID:          "oc_chat",
		MessageID:       "om_1",
		SenderOpenID:    "ou_admin",
		RequesterOpenID: "ou_zhang",
		MentionNames:    map[string]string{"ou_zhang": "张三"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.RequesterOpenID != "ou_zhang" {
		t.Fatalf("requester open_id = %q, want ou_zhang", req.RequesterOpenID)
	}
	if req.RequesterName != "张三" {
		t.Fatalf("requester name = %q, want 张三", req.RequesterName)
	}
}

func TestServiceSubmitWithoutAttributionUsesSender(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_admin"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	req, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/10",
		ChatID:       "oc_chat",
		MessageID:    "om_2",
		SenderOpenID: "ou_admin",
		SenderName:   "Admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.RequesterOpenID != "ou_admin" {
		t.Fatalf("requester open_id = %q, want ou_admin", req.RequesterOpenID)
	}
	if req.RequesterName != "Admin" {
		t.Fatalf("requester name = %q, want Admin", req.RequesterName)
	}
}

func TestServiceSubmitRejectsUnauthorizedAndDisabled(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	disabled := New(st, lark.New(lark.Config{}), Config{Enabled: false})
	if _, err := disabled.Submit(MessageContext{Text: "review https://github.com/a/b/pull/1", SenderOpenID: "ou_me"}); err == nil {
		t.Fatal("expected error for disabled service")
	}

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_allowed"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	if _, err := svc.Submit(MessageContext{Text: "review https://github.com/a/b/pull/1", SenderOpenID: "ou_other"}); err == nil {
		t.Fatal("expected error for unauthorized sender")
	}
	if _, err := svc.Submit(MessageContext{Text: "no pr url here", SenderOpenID: "ou_allowed"}); err == nil {
		t.Fatal("expected error for missing PR URL")
	}
}

func TestServiceDeduplicatesPending(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/5",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	}
	if _, err := svc.Submit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx); err == nil {
		t.Fatal("expected dedupe error for same pending PR")
	}
}

func TestServiceRejectsNonWhitelistedRepo(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"allowed/repo": t.TempDir()},
	})
	if _, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/unknown/repo/pull/9",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
	}); err == nil {
		t.Fatal("expected error for repo outside the whitelist")
	}
	if _, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/allowed/repo/pull/9",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
	}); err != nil {
		t.Fatalf("expected whitelisted repo to be accepted, got %v", err)
	}
}

func TestServiceAllowAllUsers(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"*"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	if _, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/3",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_whoever",
	}); err != nil {
		t.Fatalf("expected wildcard allow-all to accept any sender, got %v", err)
	}
}

func TestServiceProcessRunsToolInRepoDir(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": repoDir},
		Tool:         script,
	})
	req, err := svc.Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/5",
		ChatID:       "oc_chat",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.process(req.ID)

	got, err := svc.Get(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded; error=%q result=%q", got.Status, got.Error, got.ResultText)
	}
	if strings.TrimSpace(got.ResultText) != repoDir {
		t.Fatalf("tool ran in %q, want repo dir %q", strings.TrimSpace(got.ResultText), repoDir)
	}
}

func TestLoadGuide(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".saturnus-review.md"), []byte("in-repo guide: no compile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "guide.txt")
	if err := os.WriteFile(explicit, []byte("explicit guide: read only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherRepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(otherRepo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc := New(nil, lark.New(lark.Config{}), Config{})
	got, err := svc.loadGuide("a/b", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "in-repo guide: no compile" {
		t.Fatalf("in-repo discovery got %q", got)
	}

	svc.cfg.Guides = map[string]string{"a/b": explicit}
	got, err = svc.loadGuide("a/b", repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "explicit guide: read only" {
		t.Fatalf("explicit guide got %q", got)
	}

	got, err = svc.loadGuide("other/repo", otherRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected empty guide for checkout without guide file, got %q", got)
	}
}

func TestServiceSubmitRerunsTerminatedReview(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/9",
		ChatID:       "oc_c",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	}
	req, err := svc.Submit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	req.ResultText = "done"
	req.CompletedAt = time.Now()
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	req2, err := svc.Submit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if req2.ID != req.ID {
		t.Fatalf("expected same review id for re-run, got %q want %q", req2.ID, req.ID)
	}
	if req2.Status != "pending" {
		t.Fatalf("expected reset to pending, got %q", req2.Status)
	}
	if req2.ResultText != "" {
		t.Fatalf("expected cleared result, got %q", req2.ResultText)
	}
}

func TestServiceSubmitRejectsActiveDuplicate(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/5",
		ChatID:       "oc_c",
		SenderOpenID: "ou_me",
		MessageID:    "om_1",
	}
	if _, err := svc.Submit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx); err != ErrAlreadyPending {
		t.Fatalf("expected ErrAlreadyPending for active duplicate, got %v", err)
	}
}

func TestIsReviewer(t *testing.T) {
	svc := New(nil, lark.New(lark.Config{}), Config{
		AllowedUsers:   []string{"ou_a"},
		ReviewerOpenID: "ou_r",
	})
	if !svc.isReviewer("ou_r") {
		t.Fatal("expected configured reviewer")
	}
	if !svc.isReviewer("ou_a") {
		t.Fatal("expected allowed user to be treated as reviewer")
	}
	if svc.isReviewer("ou_x") {
		t.Fatal("expected unrelated user rejected")
	}
	if svc.isReviewer("") {
		t.Fatal("expected empty sender rejected")
	}
}

func TestCaptureSessionID(t *testing.T) {
	repoDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "tool.sh")
	content := `#!/bin/sh
cat <<'EOF'
[{"id":"ses_new","title":"saturnus-review-rvw_x","directory":"` + repoDir + `","updated":2000},{"id":"ses_old","title":"other","directory":"` + repoDir + `","updated":1000},{"id":"ses_otherdir","title":"saturnus-review-rvw_x","directory":"/elsewhere","updated":3000}]
EOF
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(nil, lark.New(lark.Config{}), Config{Tool: script})

	if got := svc.captureSessionID(repoDir, "rvw_x"); got != "ses_new" {
		t.Fatalf("title match = %q, want ses_new", got)
	}
	if got := svc.captureSessionID(repoDir, "rvw_z"); got != "ses_new" {
		t.Fatalf("fallback by dir recency = %q, want ses_new", got)
	}
}

func TestSubmitPublicBypassesUserAllowlist(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_allowed"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	ctx := MessageContext{
		Text:         "review https://github.com/a/b/pull/4",
		ChatID:       "oc_c",
		SenderOpenID: "ou_stranger",
	}
	if _, err := svc.Submit(ctx); err == nil {
		t.Fatal("expected Submit to reject user outside review allowlist")
	}
	if _, err := svc.SubmitPublic(ctx); err != nil {
		t.Fatalf("expected SubmitPublic to accept any sender, got %v", err)
	}
}

func TestTaskMembers(t *testing.T) {
	got := taskMembers("ou_r", "ou_q")
	if len(got) != 2 || got[0] != "ou_r" || got[1] != "ou_q" {
		t.Fatalf("taskMembers = %#v", got)
	}
	got = taskMembers("ou_same", "ou_same")
	if len(got) != 1 || got[0] != "ou_same" {
		t.Fatalf("expected dedupe, got %#v", got)
	}
	got = taskMembers("", "")
	if len(got) != 0 {
		t.Fatalf("expected empty members, got %#v", got)
	}
}

func TestFormatResultStructuredSummary(t *testing.T) {
	svc := New(nil, lark.New(lark.Config{}), Config{})
	req := store.ReviewRequest{
		ID:       "rvw_x",
		Status:   "succeeded",
		PRURL:    "https://github.com/a/b/pull/1",
		Repo:     "a/b",
		PRNumber: 1,
		ResultText: "investigating...\n" +
			"REVIEW SUMMARY:\n" +
			"PR summary: fix the MultiSave race\n" +
			"Issues:\n" +
			"- etcd_restore.go:40 shared err is racy\n" +
			"Review suggestions: LGTM after fix\n",
	}
	got := svc.FormatResult(req)
	for _, want := range []string{"PR summary:", "Issues:", "Review suggestions:", "fix the MultiSave race", "shared err is racy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatResult missing %q:\n%s", want, got)
		}
	}
}

func TestFormatResultFallsBackToRawSummary(t *testing.T) {
	svc := New(nil, lark.New(lark.Config{}), Config{})
	req := store.ReviewRequest{
		ID:         "rvw_y",
		Status:     "succeeded",
		PRURL:      "https://github.com/a/b/pull/2",
		Repo:       "a/b",
		PRNumber:   2,
		ResultText: "REVIEW SUMMARY: unformatted verdict",
	}
	got := svc.FormatResult(req)
	if !strings.Contains(got, "unformatted verdict") {
		t.Fatalf("FormatResult missing raw summary:\n%s", got)
	}
}

type fakeLark struct {
	enabled      bool
	task         lark.Task
	sent         []string
	chatSent     []string
	taskUpdates  []string
	replies      []string
	tasklistAdds []string
	userName     string
	userCalls    int
}

func (f *fakeLark) Enabled() bool { return f.enabled }
func (f *fakeLark) GetUserName(string) (string, error) {
	f.userCalls++
	return f.userName, nil
}
func (f *fakeLark) SendText(_, receiveID, text string) error {
	f.chatSent = append(f.chatSent, receiveID+": "+text)
	return nil
}
func (f *fakeLark) SendTextToUser(openID, text string) error {
	f.sent = append(f.sent, openID+": "+text)
	return nil
}
func (f *fakeLark) CreateTask(lark.CreateTaskInput) (lark.Task, error) {
	return f.task, nil
}
func (f *fakeLark) GetTask(taskGUID string) (lark.Task, error) {
	return f.task, nil
}
func (f *fakeLark) UpdateTask(taskGUID string, input lark.UpdateTaskInput) error {
	desc := ""
	if input.Description != nil {
		desc = *input.Description
	}
	completed := false
	if input.Completed != nil {
		completed = *input.Completed
	}
	f.taskUpdates = append(f.taskUpdates, fmt.Sprintf("%s|completed=%v|%s", taskGUID, completed, desc))
	return nil
}
func (f *fakeLark) ReplyMessage(id, text string) error {
	f.replies = append(f.replies, id+": "+text)
	return nil
}

func (f *fakeLark) EnsureTaskList(name string) (lark.TaskList, error) {
	return lark.TaskList{GUID: "tkl_group", Name: name}, nil
}

func (f *fakeLark) AddTaskToList(taskGUID, taskListGUID string) error {
	f.tasklistAdds = append(f.tasklistAdds, taskGUID+"->"+taskListGUID)
	return nil
}

func TestCreateTaskFilesUnderTaskGroup(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "t_1", URL: "https://task"}}
	svc := New(st, fake, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"},
		Repos:      map[string]string{"a/b": t.TempDir()},
		CreateTask: true, TaskGroup: "SaturnusReview",
	})
	if _, err := svc.Submit(MessageContext{
		Text: "review https://github.com/a/b/pull/1", ChatID: "oc_c", MessageID: "om_1", SenderOpenID: "ou_me",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.tasklistAdds) != 1 || !strings.HasPrefix(fake.tasklistAdds[0], "t_1->tkl_group") {
		t.Fatalf("tasklist adds = %v, want [t_1->tkl_group]", fake.tasklistAdds)
	}
}

func TestCreateTaskFilesUnderPinnedGroup(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "t_2", URL: "https://task"}}
	svc := New(st, fake, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"},
		Repos:      map[string]string{"a/b": t.TempDir()},
		CreateTask: true, TaskGroupID: "tkl_pinned",
	})
	if _, err := svc.Submit(MessageContext{
		Text: "review https://github.com/a/b/pull/2", ChatID: "oc_c", MessageID: "om_2", SenderOpenID: "ou_me",
	}); err != nil {
		t.Fatal(err)
	}
	if len(fake.tasklistAdds) != 1 || !strings.HasPrefix(fake.tasklistAdds[0], "t_2->tkl_pinned") {
		t.Fatalf("tasklist adds = %v, want [t_2->tkl_pinned]", fake.tasklistAdds)
	}
}

func TestMaybeCreateTaskNotifiesRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fake := &fakeLark{enabled: true, task: lark.Task{
		GUID: "t_123",
		URL:  "https://applink.feishu.cn/client/todo/detail?guid=t_123",
	}}
	svc := &Service{
		store: st,
		lark:  fake,
		cfg:   Config{CreateTask: true, ReviewerOpenID: "ou_r"},
	}
	req := store.ReviewRequest{
		ID:              "rvw_x",
		Status:          "pending",
		RequesterOpenID: "ou_q",
		Repo:            "a/b",
		PRNumber:        4,
		PRURL:           "https://github.com/a/b/pull/4",
	}
	svc.maybeCreateTask(&req)
	if req.TaskID != "t_123" || req.TaskURL != fake.task.URL {
		t.Fatalf("task not stored: %#v", req)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("requester not notified: %#v", fake.sent)
	}
	for _, want := range []string{"ou_q", "t_123", fake.task.URL} {
		if !strings.Contains(fake.sent[0], want) {
			t.Fatalf("requester message missing %q: %s", want, fake.sent[0])
		}
	}
}

func TestMaybeCreateTaskSkipsRequesterNotificationWhenDisabled(t *testing.T) {
	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "t_1", URL: "https://applink.feishu.cn/client/todo/detail?guid=t_1"}}
	svc := &Service{lark: fake, cfg: Config{CreateTask: false}}
	svc.maybeCreateTask(&store.ReviewRequest{ID: "rvw_x", RequesterOpenID: "ou_q"})
	if len(fake.sent) != 0 {
		t.Fatalf("expected no notification when task creation disabled, got %#v", fake.sent)
	}
}

func TestMaybeCreateTaskDeduplicatesAcrossReruns(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "t_123", URL: "https://applink.feishu.cn/client/todo/detail?guid=t_123"}}
	svc := &Service{
		store: st,
		lark:  fake,
		cfg:   Config{CreateTask: true},
	}
	req := store.ReviewRequest{
		ID:              "rvw_x",
		Status:          "pending",
		RequesterOpenID: "ou_q",
		Repo:            "a/b",
		PRNumber:        4,
		PRURL:           "https://github.com/a/b/pull/4",
	}

	svc.maybeCreateTask(&req)
	if req.TaskID != "t_123" || len(fake.sent) != 1 {
		t.Fatalf("expected first task creation, got task=%#v sent=%#v", req, fake.sent)
	}

	req.Status = "pending"
	svc.maybeCreateTask(&req)
	if req.TaskID != "t_123" {
		t.Fatalf("task id changed across rerun: %q", req.TaskID)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("expected no duplicate task/notification on rerun, got %#v", fake.sent)
	}
}

func TestResolveRequesterNameSuccess(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID:              "rvw_n",
		ThreadID:        thread.ID,
		Status:          "pending",
		PRURL:           "https://github.com/a/b/pull/1",
		Repo:            "a/b",
		PRNumber:        1,
		RequesterOpenID: "ou_x",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := &Service{store: st, lark: &fakeLark{enabled: true, userName: "张三"}}
	if err := svc.ResolveRequesterName(&req); err != nil {
		t.Fatalf("resolve requester name: %v", err)
	}
	if req.RequesterName != "张三" {
		t.Fatalf("expected resolved name, got %q", req.RequesterName)
	}

	got, err := st.GetReview("rvw_n")
	if err != nil {
		t.Fatal(err)
	}
	if got.RequesterName != "张三" {
		t.Fatalf("resolved name not persisted, got %q", got.RequesterName)
	}
}

func TestResolveRequesterNameErrorsDoNotPanic(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID:              "rvw_e",
		ThreadID:        thread.ID,
		Status:          "pending",
		PRURL:           "https://github.com/a/b/pull/1",
		Repo:            "a/b",
		PRNumber:        1,
		RequesterOpenID: "ou_x",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Contact API returns an empty name: resolution fails, name stays empty.
	svc := &Service{store: st, lark: &fakeLark{enabled: true}}
	if err := svc.ResolveRequesterName(&req); err == nil {
		t.Fatal("expected error when contact API returns empty name")
	}
	if req.RequesterName != "" {
		t.Fatalf("expected empty name, got %q", req.RequesterName)
	}

	// Disabled lark client: resolution is skipped with an error, no panic.
	disabled := &Service{store: st, lark: &fakeLark{enabled: false}}
	if err := disabled.ResolveRequesterName(&req); err == nil {
		t.Fatal("expected error when lark client disabled")
	}

	// Already-resolved names are left untouched.
	req.RequesterName = "张三"
	if err := svc.ResolveRequesterName(&req); err != nil {
		t.Fatalf("expected nil for already-resolved name, got %v", err)
	}
}

func TestResolveRequesterNameBacksOffAfterFailure(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID:              "rvw_b",
		ThreadID:        thread.ID,
		Status:          "pending",
		PRURL:           "https://github.com/a/b/pull/1",
		Repo:            "a/b",
		PRNumber:        1,
		RequesterOpenID: "ou_x",
	})
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeLark{enabled: true}
	svc := &Service{store: st, lark: fake}

	if err := svc.ResolveRequesterName(&req); err == nil {
		t.Fatal("expected first attempt to fail with empty contact name")
	}
	if fake.userCalls != 1 {
		t.Fatalf("expected 1 contact API call, got %d", fake.userCalls)
	}

	// A retry for the same open_id within the backoff window is skipped
	// silently: no error, no extra API call, name stays empty.
	if err := svc.ResolveRequesterName(&req); err != nil {
		t.Fatalf("expected backed-off retry to return nil, got %v", err)
	}
	if fake.userCalls != 1 {
		t.Fatalf("expected no second contact API call, got %d", fake.userCalls)
	}
	if req.RequesterName != "" {
		t.Fatalf("expected name to stay empty, got %q", req.RequesterName)
	}
}

// fakeGHCli writes an executable "gh" stub that always reports the given head
// commit, so head-change checks can be exercised without a real GitHub CLI.
func fakeGHCli(t *testing.T, headCommit string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	content := "#!/bin/sh\nprintf '%s\\n' '{\"title\":\"T\",\"baseRefName\":\"main\",\"headRefOid\":\"" + headCommit + "\"}'\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// fakeGHRefresh writes an executable "gh" stub answering "pr view" with the
// given PR state and "api .../reviews" with the given reviews JSON.
func fakeGHRefresh(t *testing.T, state, reviewsJSON string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	content := "#!/bin/sh\ncase \"$1\" in\npr) printf '%s\\n' '{\"title\":\"T\",\"baseRefName\":\"main\",\"headRefOid\":\"abc123\",\"state\":\"" + state + "\"}' ;;\napi) printf '%s\\n' '" + reviewsJSON + "' ;;\n*) printf '%s\\n' '[]' ;;\nesac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func newReviewForRetry(t *testing.T, st *store.Store, id, status, head, session, errText string) {
	t.Helper()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID:              id,
		ThreadID:        thread.ID,
		Status:          status,
		PRURL:           "https://github.com/a/b/pull/1",
		Repo:            "a/b",
		PRNumber:        1,
		RequesterOpenID: "ou_me",
		SessionID:       session,
		HeadCommit:      head,
		Error:           errText,
	}); err != nil {
		t.Fatal(err)
	}
}

func retryService(t *testing.T, st *store.Store, gh string) *Service {
	return &Service{
		store: st,
		lark:  &fakeLark{enabled: true},
		cfg: Config{
			Enabled:      true,
			AllowedUsers: []string{"ou_me"},
			Repos:        map[string]string{"a/b": t.TempDir()},
			GHCli:        gh,
		},
	}
}

func TestSetRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_req", "succeeded", "abc123", "ses_1", "")

	svc := retryService(t, st, "")
	req, err := svc.SetRequester("rvw_req", "ou_new", "New User")
	if err != nil {
		t.Fatal(err)
	}
	if req.RequesterOpenID != "ou_new" || req.RequesterName != "New User" {
		t.Fatalf("requester = %s (%s), want ou_new (New User)", req.RequesterOpenID, req.RequesterName)
	}
	// Persisted.
	got, err := svc.Get("rvw_req")
	if err != nil {
		t.Fatal(err)
	}
	if got.RequesterOpenID != "ou_new" || got.RequesterName != "New User" {
		t.Fatalf("persisted requester = %s (%s)", got.RequesterOpenID, got.RequesterName)
	}

	if _, err := svc.SetRequester("rvw_missing", "ou_x", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := svc.SetRequester("rvw_req", "", ""); err == nil {
		t.Fatal("expected error for empty open_id")
	}
}

func TestSetRequesterRejectsArchived(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_arch", "archived", "abc123", "ses_1", "")
	svc := retryService(t, st, "")
	if _, err := svc.SetRequester("rvw_arch", "ou_new", "New"); !errors.Is(err, ErrArchived) {
		t.Fatalf("expected ErrArchived, got %v", err)
	}
}

func TestRetryFailedAlwaysReruns(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_f", "failed", "", "ses_prev", "boom")

	req, err := retryService(t, st, "").Retry("rvw_f", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" {
		t.Fatalf("expected pending, got %s", req.Status)
	}
	if req.SessionID != "ses_prev" {
		t.Fatalf("expected previous session kept for continuation, got %q", req.SessionID)
	}
	if !strings.Contains(req.RetryReason, "retry after failure") {
		t.Fatalf("unexpected retry_reason: %q", req.RetryReason)
	}
}

func TestRetrySucceededUnchangedHeadRefuses(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_s", "succeeded", "abc123", "ses_1", "")

	_, err = retryService(t, st, fakeGHCli(t, "abc123")).Retry("rvw_s", MessageContext{SenderOpenID: "ou_me"})
	if !errors.Is(err, ErrReviewUnchanged) {
		t.Fatalf("expected ErrReviewUnchanged, got %v", err)
	}
}

func TestRetrySucceededHeadChangedReruns(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_h", "succeeded", "abc123", "ses_1", "")

	req, err := retryService(t, st, fakeGHCli(t, "def456")).Retry("rvw_h", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" {
		t.Fatalf("expected pending, got %s", req.Status)
	}
	if req.SessionID != "ses_1" {
		t.Fatalf("expected previous session kept, got %q", req.SessionID)
	}
	if !strings.Contains(req.RetryReason, "head changed") {
		t.Fatalf("unexpected retry_reason: %q", req.RetryReason)
	}
}

func TestRetryOnReplyHeadChangedRerunsAndNotifies(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_reply", "succeeded", "abc123", "ses_1", "")
	fk := &fakeLark{enabled: true, userName: "Me"}
	svc := &Service{
		store: st,
		lark:  fk,
		cfg: Config{
			Enabled:        true,
			AllowedUsers:   []string{"ou_me"},
			Repos:          map[string]string{"a/b": t.TempDir()},
			GHCli:          fakeGHCli(t, "def456"),
			ReviewerOpenID: "ou_reviewer",
		},
	}
	req, retried, err := svc.RetryOnReply(MessageContext{
		ChatID:        "oc_c",
		RootMessageID: "om_1",
		MessageID:     "om_reply",
		SenderOpenID:  "ou_me",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !retried {
		t.Fatal("expected retry when PR head moved")
	}
	if req.Status != "pending" {
		t.Fatalf("expected pending, got %s", req.Status)
	}
	if !strings.Contains(req.RetryReason, "reply in review thread") {
		t.Fatalf("unexpected retry_reason: %q", req.RetryReason)
	}
	// Reviewer notified via DM.
	foundReviewer := false
	for _, s := range fk.sent {
		if strings.Contains(s, "ou_reviewer") && strings.Contains(s, req.ID) {
			foundReviewer = true
		}
	}
	if !foundReviewer {
		t.Fatalf("reviewer not notified: %v", fk.sent)
	}
	// Requester acknowledged in-thread.
	if len(fk.replies) == 0 {
		t.Fatal("expected in-thread reply to the requester")
	}
}

func TestRetryOnReplyHeadUnchangedNoRerun(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_nochange", "succeeded", "abc123", "ses_1", "")
	fk := &fakeLark{enabled: true}
	svc := &Service{
		store: st,
		lark:  fk,
		cfg: Config{
			Enabled:        true,
			AllowedUsers:   []string{"ou_me"},
			Repos:          map[string]string{"a/b": t.TempDir()},
			GHCli:          fakeGHCli(t, "abc123"),
			ReviewerOpenID: "ou_reviewer",
		},
	}
	_, retried, err := svc.RetryOnReply(MessageContext{
		ChatID:        "oc_c",
		RootMessageID: "om_1",
		MessageID:     "om_reply",
		SenderOpenID:  "ou_me",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried {
		t.Fatal("expected no retry when PR head unchanged")
	}
	if len(fk.sent) != 0 || len(fk.replies) != 0 {
		t.Fatalf("expected no notifications when head unchanged, sent=%v replies=%v", fk.sent, fk.replies)
	}
}

func TestRetryOnReplyNoReviewInThread(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := retryService(t, st, "")
	if _, _, err := svc.RetryOnReply(MessageContext{ChatID: "oc_other", RootMessageID: "om_none"}); err == nil {
		t.Fatal("expected error when no review is rooted at the reply thread")
	}
}

func TestSubmitReuseSucceededUnchangedHeadRefused(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_u", "succeeded", "abc123", "ses_1", "")

	_, err = retryService(t, st, fakeGHCli(t, "abc123")).Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/1",
		ChatID:       "oc_c",
		SenderOpenID: "ou_me",
		MessageID:    "om_2",
	})
	if !errors.Is(err, ErrReviewUnchanged) {
		t.Fatalf("expected ErrReviewUnchanged, got %v", err)
	}
}

func TestSubmitReuseFailedReruns(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_f2", "failed", "abc123", "ses_1", "boom")

	req, err := retryService(t, st, fakeGHCli(t, "abc123")).Submit(MessageContext{
		Text:         "review https://github.com/a/b/pull/1",
		ChatID:       "oc_c",
		SenderOpenID: "ou_me",
		MessageID:    "om_2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != "pending" {
		t.Fatalf("expected pending, got %s", req.Status)
	}
	if req.SessionID != "ses_1" {
		t.Fatalf("expected previous session kept, got %q", req.SessionID)
	}
	if !strings.Contains(req.RetryReason, "re-submitted after failure") {
		t.Fatalf("unexpected retry_reason: %q", req.RetryReason)
	}
}

func TestRetryPreservesOriginalRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_alice", "om_alice", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_orig", ThreadID: thread.ID, Status: "failed",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_alice", RequesterName: "Alice",
		SessionID: "ses_1", Error: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_alice", "ou_bob"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})

	// Reviewer Bob retries from a different thread: the original requester must
	// be preserved so requester replies/task notifications still target Alice.
	retried, err := svc.Retry("rvw_orig", MessageContext{
		SenderOpenID: "ou_bob", SenderName: "Bob", ChatID: "oc_bob", MessageID: "om_bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.RequesterOpenID != "ou_alice" || retried.RequesterName != "Alice" {
		t.Fatalf("requester overwritten by retry: open=%q name=%q", retried.RequesterOpenID, retried.RequesterName)
	}
	// The latest request context is still recorded on the review.
	if retried.ChatID != "oc_bob" || retried.MessageID != "om_bob" {
		t.Fatalf("request context not updated: chat=%q msg=%q", retried.ChatID, retried.MessageID)
	}
	if retried.ThreadID == thread.ID {
		t.Fatal("expected thread to follow the new request context")
	}
}

func TestSubmitRerunPreservesOriginalRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_alice", "om_alice", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_sub", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_alice", RequesterName: "Alice",
		SessionID: "ses_1", HeadCommit: "abc123",
	}); err != nil {
		t.Fatal(err)
	}

	// Head moved to def456, so re-submission triggers a rerun by Bob.
	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_alice", "ou_bob"},
		Repos:        map[string]string{"a/b": t.TempDir()},
		GHCli:        fakeGHCli(t, "def456"),
	})
	rerun, err := svc.Submit(MessageContext{
		Text: "https://github.com/a/b/pull/1", ChatID: "oc_bob",
		SenderOpenID: "ou_bob", SenderName: "Bob", MessageID: "om_bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rerun.ID != "rvw_sub" {
		t.Fatalf("expected same review, got %q", rerun.ID)
	}
	if rerun.RequesterOpenID != "ou_alice" || rerun.RequesterName != "Alice" {
		t.Fatalf("requester overwritten by re-submission: open=%q name=%q", rerun.RequesterOpenID, rerun.RequesterName)
	}
}

func TestRetryAdoptsSenderForPlaceholderRequester(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Review created via the HTTP API without a requester -> "api" sentinel.
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_api", ThreadID: thread.ID, Status: "failed",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "api", Error: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	retried, err := svc.Retry("rvw_api", MessageContext{SenderOpenID: "ou_me", SenderName: "Me"})
	if err != nil {
		t.Fatal(err)
	}
	if retried.RequesterOpenID != "ou_me" || retried.RequesterName != "Me" {
		t.Fatalf("placeholder requester not replaced: open=%q name=%q", retried.RequesterOpenID, retried.RequesterName)
	}
}

func TestIsPlaceholderRequester(t *testing.T) {
	for _, openID := range []string{"", "api"} {
		if !isPlaceholderRequester(openID) {
			t.Fatalf("expected %q to be a placeholder requester", openID)
		}
	}
	for _, openID := range []string{"ou_alice", "api_user"} {
		if isPlaceholderRequester(openID) {
			t.Fatalf("expected %q to be a real requester", openID)
		}
	}
}

func TestFinishRecordsRunOutcome(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_r", ThreadID: thread.ID, Status: "reviewing",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateReviewRun(store.ReviewRun{
		ReviewID: req.ID, Status: "running", Tool: "opencode", RetryReason: "retry after failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Seq != 1 {
		t.Fatalf("expected seq 1, got %d", run.Seq)
	}

	svc := &Service{store: st, lark: &fakeLark{enabled: true}, cfg: Config{}}
	req.HeadCommit = "abc1234"
	req.SessionID = "ses_1"
	svc.finish(&req, "succeeded", "review ok", "")

	runs := st.ListRuns(req.ID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	got := runs[0]
	if got.Status != "succeeded" || got.HeadCommit != "abc1234" || got.SessionID != "ses_1" || got.ResultText != "review ok" || got.CompletedAt.IsZero() {
		t.Fatalf("run outcome not recorded: %#v", got)
	}
	if _, err := st.CurrentRun(req.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no in-flight run after finish, got err=%v", err)
	}
}

func TestParseRepoMap(t *testing.T) {
	m := ParseRepoMap(" a/b=/srv/a , c/d = /srv/c ,malformed,e/f=/srv/e")
	want := map[string]string{"a/b": "/srv/a", "c/d": "/srv/c", "e/f": "/srv/e"}
	if len(m) != len(want) {
		t.Fatalf("got %d entries, want %d: %#v", len(m), len(want), m)
	}
	for repo, dir := range want {
		if m[repo] != dir {
			t.Fatalf("repo %s -> %q, want %q", repo, m[repo], dir)
		}
	}
}

// reactionToolScript writes a tool stub that answers "session list" with an
// empty session list (so session capture finds nothing) and otherwise echoes
// its argv one-per-line (so tests can assert the resume args and instruction).
func reactionToolScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "tool.sh")
	content := "#!/bin/sh\ncase \"$1 $2\" in\n\"session list\") printf '%s\\n' '[]' ;;\n*) printf '%s\\n' \"$@\" ;;\nesac\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestActRejectsInvalid(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:        true,
		AllowedUsers:   []string{"ou_me"},
		ReviewerOpenID: "ou_r",
		Repos:          map[string]string{"a/b": repoDir},
		Tool:           reactionToolScript(t),
	})

	// Disabled service.
	disabled := New(st, lark.New(lark.Config{}), Config{Enabled: false})
	if _, err := disabled.Act(MessageContext{SenderOpenID: "ou_r"}, "rvw_x", "post comments"); err == nil {
		t.Fatal("expected error for disabled service")
	}
	// Non-reviewer sender.
	if _, err := svc.Act(MessageContext{SenderOpenID: "ou_x"}, "rvw_x", "post comments"); err == nil {
		t.Fatal("expected error for non-reviewer sender")
	}
	// Empty instruction.
	if _, err := svc.Act(MessageContext{SenderOpenID: "ou_me"}, "rvw_x", "   "); err == nil {
		t.Fatal("expected error for empty instruction")
	}
	// Missing review.
	if _, err := svc.Act(MessageContext{SenderOpenID: "ou_me"}, "rvw_missing", "post comments"); err == nil {
		t.Fatal("expected error for missing review")
	}

	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Review still in progress.
	inProgress, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_ip", ThreadID: thread.ID, Status: "reviewing",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Act(MessageContext{SenderOpenID: "ou_me"}, inProgress.ID, "post comments"); err == nil {
		t.Fatal("expected error for in-progress review")
	}
	// Review without a tool session.
	noSession, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_ns", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Act(MessageContext{SenderOpenID: "ou_me"}, noSession.ID, "post comments"); err == nil {
		t.Fatal("expected error for review without a tool session")
	}
}

func TestProcessReactionRunsToolResumingSession(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:        true,
		AllowedUsers:   []string{"ou_me"},
		ReviewerOpenID: "ou_r",
		Repos:          map[string]string{"a/b": repoDir},
		Tool:           reactionToolScript(t),
	})
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_a", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		SessionID: "ses_9",
	})
	if err != nil {
		t.Fatal(err)
	}

	reac, err := svc.Act(MessageContext{
		SenderOpenID: "ou_me",
		SenderName:   "reviewer",
		ChatID:       "oc_c",
		MessageID:    "om_9",
	}, req.ID, "用inline comment 回复 issue 1,3")
	if err != nil {
		t.Fatal(err)
	}
	if reac.SessionID != "ses_9" {
		t.Fatalf("expected review session kept, got %q", reac.SessionID)
	}

	svc.processReaction(reac.ID)

	got, err := st.GetReaction(reac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded; error=%q result=%q", got.Status, got.Error, got.ResultText)
	}
	for _, want := range []string{"run", "--session", "ses_9", "--title", "saturnus-review-rvw_a", "用inline comment 回复 issue 1,3"} {
		if !strings.Contains(got.ResultText, want) {
			t.Fatalf("tool output missing %q: %q", want, got.ResultText)
		}
	}
}

func TestProcessReactionFallsBackToCapturedSession(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "tool.sh")
	content := `#!/bin/sh
case "$1 $2" in
"session list") printf '%s\n' '[{"id":"ses_new","title":"saturnus-review-rvw_cap","directory":"` + repoDir + `","updated":2000}]' ;;
*) printf '%s\n' "$@" ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, lark.New(lark.Config{}), Config{
		Enabled:        true,
		AllowedUsers:   []string{"ou_me"},
		ReviewerOpenID: "ou_r",
		Repos:          map[string]string{"a/b": repoDir},
		Tool:           script,
	})
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_cap", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	reac, err := svc.Act(MessageContext{SenderOpenID: "ou_me", ChatID: "oc_c"}, req.ID, "give concrete code for issue 2")
	if err != nil {
		t.Fatal(err)
	}
	if reac.SessionID != "ses_new" {
		t.Fatalf("expected captured session, got %q", reac.SessionID)
	}

	svc.processReaction(reac.ID)
	got, err := st.GetReaction(reac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || !strings.Contains(got.ResultText, "ses_new") {
		t.Fatalf("reaction did not resume captured session: status=%q result=%q error=%q", got.Status, got.ResultText, got.Error)
	}
}

func TestProcessReactionNotifiesReviewerChat(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLark{enabled: true}
	svc := New(st, fake, Config{
		Enabled:        true,
		AllowedUsers:   []string{"ou_me"},
		ReviewerOpenID: "ou_r",
		Repos:          map[string]string{"a/b": repoDir},
		Tool:           reactionToolScript(t),
	})
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_ntf", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		SessionID: "ses_9",
	})
	if err != nil {
		t.Fatal(err)
	}
	reac, err := svc.Act(MessageContext{SenderOpenID: "ou_me", ChatID: "oc_chat"}, req.ID, "post inline comments")
	if err != nil {
		t.Fatal(err)
	}

	svc.processReaction(reac.ID)

	if len(fake.chatSent) != 1 {
		t.Fatalf("expected 1 chat notification, got %#v", fake.chatSent)
	}
	for _, want := range []string{reac.ID, "succeeded", "rvw_ntf", "post inline comments", "run"} {
		if !strings.Contains(fake.chatSent[0], want) {
			t.Fatalf("reaction notification missing %q: %s", want, fake.chatSent[0])
		}
	}
}

func TestProcessReactionFailsWithoutSession(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_me"},
		Repos:        map[string]string{"a/b": repoDir},
		Tool:         reactionToolScript(t),
	})
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_no", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reac, err := st.CreateReaction(store.ReviewReaction{
		ReviewID: req.ID, Instruction: "act", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.processReaction(reac.ID)

	got, err := st.GetReaction(reac.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" || !strings.Contains(got.Error, "session") {
		t.Fatalf("expected failure without session, got status=%q error=%q", got.Status, got.Error)
	}
}

func TestFormatReaction(t *testing.T) {
	svc := New(nil, lark.New(lark.Config{}), Config{})
	reac := store.ReviewReaction{
		ID: "rxn_x", Status: "succeeded", Instruction: "give concrete code",
		ResultText: "```go\nvar x = 1\n```\n",
	}
	req := store.ReviewRequest{
		ID: "rvw_a", PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	}
	got := svc.FormatReaction(reac, req)
	for _, want := range []string{"rxn_x", "succeeded", "rvw_a", "give concrete code", "```go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatReaction missing %q:\n%s", want, got)
		}
	}
}

func TestListThreadsTracksHistoryAcrossRetries(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	thread, err := st.GetOrCreateReviewThread("oc_alice", "om_1", "topic-1", []string{"ou_alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_th", ThreadID: thread.ID, Status: "failed",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_alice", SessionID: "ses_1", Error: "boom",
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled:      true,
		AllowedUsers: []string{"ou_alice", "ou_bob"},
		Repos:        map[string]string{"a/b": t.TempDir()},
	})
	// Reviewer Bob continues the review from a different chat: a new thread is
	// linked while the original thread stays in the history.
	retried, err := svc.Retry("rvw_th", MessageContext{
		SenderOpenID: "ou_bob", SenderName: "Bob", ChatID: "oc_bob", RootMessageID: "om_2", Topic: "topic-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.ThreadID == thread.ID {
		t.Fatal("expected thread pointer to move to the new request context")
	}

	threads := svc.ListThreads("rvw_th")
	if len(threads) != 2 {
		t.Fatalf("expected 2 threads in history, got %#v", threads)
	}
	seen := map[string]bool{}
	for _, t := range threads {
		seen[t.ID] = true
	}
	if !seen[thread.ID] || !seen[retried.ThreadID] {
		t.Fatalf("thread history missing entries: %#v", seen)
	}
}

func TestListThreadsFallsBackToCurrentThread(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Legacy review: ThreadID set but no links recorded.
	thread, err := st.GetOrCreateReviewThread("oc_alice", "om_1", "topic-1", []string{"ou_alice", "ou_bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: "rvw_legacy", ThreadID: thread.ID, Status: "succeeded",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_alice",
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, &fakeLark{enabled: true}, Config{})
	threads := svc.ListThreads("rvw_legacy")
	if len(threads) != 1 || threads[0].ID != thread.ID {
		t.Fatalf("expected current thread fallback, got %#v", threads)
	}
	if threads[0].ChatID != "oc_alice" || len(threads[0].Participants) != 2 {
		t.Fatalf("unexpected thread: %#v", threads[0])
	}
}

// newRefreshReview creates a review with the given status and optional task.
func newRefreshReview(t *testing.T, st *store.Store, id, status, taskID string) {
	t.Helper()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReview(store.ReviewRequest{
		ID: id, ThreadID: thread.ID, Status: status,
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
		RequesterOpenID: "ou_me", TaskID: taskID, TaskURL: "https://task.example/t/" + taskID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewOutdatedAndFormatting(t *testing.T) {
	outdated := store.ReviewRequest{ID: "rvw_o", Status: "succeeded", HeadCommit: "abc123", PrHeadCommit: "def456"}
	if !ReviewOutdated(outdated) {
		t.Fatal("expected outdated when PR head moved past the reviewed head")
	}
	current := store.ReviewRequest{ID: "rvw_c", Status: "succeeded", HeadCommit: "abc123", PrHeadCommit: "abc123"}
	if ReviewOutdated(current) {
		t.Fatal("expected not outdated when heads match")
	}
	noBaseline := store.ReviewRequest{ID: "rvw_n", Status: "pending"}
	if ReviewOutdated(noBaseline) {
		t.Fatal("expected not outdated without head baselines")
	}

	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := retryService(t, st, "")
	if !strings.Contains(svc.FormatOne(outdated), "OUTDATED") {
		t.Fatalf("FormatOne missing OUTDATED: %q", svc.FormatOne(outdated))
	}
	if !strings.Contains(svc.FormatOne(outdated), "pr_head=def456 (outdated)") {
		t.Fatalf("FormatOne missing pr_head detail: %q", svc.FormatOne(outdated))
	}
	if strings.Contains(svc.FormatOne(current), "OUTDATED") {
		t.Fatalf("FormatOne wrongly flags current review: %q", svc.FormatOne(current))
	}
	if !strings.Contains(svc.FormatList([]store.ReviewRequest{outdated}), "OUTDATED") {
		t.Fatalf("FormatList missing OUTDATED: %q", svc.FormatList([]store.ReviewRequest{outdated}))
	}
}

func TestRefreshRecordsPRHeadCommit(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewForRetry(t, st, "rvw_ref", "succeeded", "oldhead", "ses_1", "")

	svc := retryService(t, st, fakeGHRefresh(t, "OPEN", "[]"))
	req, res, err := svc.Refresh("rvw_ref", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if res.PRState != "OPEN" {
		t.Fatalf("pr state = %q, want OPEN", res.PRState)
	}
	if req.PrHeadCommit != "abc123" {
		t.Fatalf("pr_head_commit = %q, want abc123 (from gh)", req.PrHeadCommit)
	}
	if req.HeadCommit != "oldhead" {
		t.Fatalf("head_commit = %q, want oldhead (last reviewed head unchanged)", req.HeadCommit)
	}
	if !ReviewOutdated(req) {
		t.Fatal("expected the review to be flagged OUTDATED after refresh found a newer head")
	}
}

func TestRefreshArchivesMergedPRAndCompletesTask(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newRefreshReview(t, st, "rvw_ref", "succeeded", "t_1")

	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "t_1"}}
	svc := New(st, fake, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"}, GHCli: fakeGHRefresh(t, "MERGED", "[]"),
	})
	got, res, err := svc.Refresh("rvw_ref", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "archived" || got.ArchivedAt.IsZero() {
		t.Fatalf("expected archived review, got status=%q archived_at=%v", got.Status, got.ArchivedAt)
	}
	if !res.Archived || !res.TaskCompleted {
		t.Fatalf("expected archive + task completion, got %#v", res)
	}
	if len(fake.taskUpdates) != 1 || !strings.Contains(fake.taskUpdates[0], "completed=true") {
		t.Fatalf("expected task completion update, got %#v", fake.taskUpdates)
	}

	// Refreshing again is idempotent and does not re-complete the task (the
	// task API rejects completing an already-completed task).
	got2, res2, err := svc.Refresh("rvw_ref", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != "archived" || res2.Archived {
		t.Fatalf("expected idempotent archive, got status=%q res=%#v", got2.Status, res2)
	}
	if len(fake.taskUpdates) != 1 {
		t.Fatalf("expected no second task completion, got %#v", fake.taskUpdates)
	}
}

func TestRefreshRestoresReopenedPR(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newRefreshReview(t, st, "rvw_restore", "archived", "")
	req, err := st.GetReview("rvw_restore")
	if err != nil {
		t.Fatal(err)
	}
	req.ArchivedAt = time.Now().UTC()
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"}, GHCli: fakeGHRefresh(t, "OPEN", "[]"),
	})
	got, res, err := svc.Refresh("rvw_restore", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || !got.ArchivedAt.IsZero() {
		t.Fatalf("expected restored review, got status=%q archived_at=%v", got.Status, got.ArchivedAt)
	}
	if !res.Unarchived {
		t.Fatalf("expected unarchive result, got %#v", res)
	}
}

func TestRefreshSyncsNewReviewsToTask(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newRefreshReview(t, st, "rvw_sync", "succeeded", "t_1")

	reviews := `[{"user":"reviewer1","state":"APPROVED","body":"looks good","submitted_at":"2026-09-01T10:00:00Z"},{"user":"reviewer2","state":"COMMENTED","body":"see line 42","submitted_at":"2026-09-02T10:00:00Z"}]`
	fake := &fakeLark{enabled: true, task: lark.Task{GUID: "t_1", Description: "PR review request rvw_sync"}}
	svc := New(st, fake, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"}, GHCli: fakeGHRefresh(t, "OPEN", reviews),
	})
	_, res, err := svc.Refresh("rvw_sync", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TaskUpdated || res.NewReviews != 2 {
		t.Fatalf("expected 2 synced reviews, got %#v", res)
	}
	if len(fake.taskUpdates) != 1 {
		t.Fatalf("expected 1 task update, got %#v", fake.taskUpdates)
	}
	for _, want := range []string{"[review] reviewer1 (APPROVED)", "[review] reviewer2 (COMMENTED)", "looks good", "see line 42", "PR review request rvw_sync"} {
		if !strings.Contains(fake.taskUpdates[0], want) {
			t.Fatalf("task description missing %q: %s", want, fake.taskUpdates[0])
		}
	}

	// Second refresh must not re-sync the same reviews.
	_, res2, err := svc.Refresh("rvw_sync", MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.TaskUpdated || res2.NewReviews != 0 {
		t.Fatalf("expected no duplicate sync, got %#v", res2)
	}
	if len(fake.taskUpdates) != 1 {
		t.Fatalf("expected no second task update, got %#v", fake.taskUpdates)
	}
}

func TestRefreshAllSkipsArchived(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newRefreshReview(t, st, "rvw_active", "succeeded", "")
	newRefreshReview(t, st, "rvw_archived", "archived", "")

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"}, GHCli: fakeGHRefresh(t, "OPEN", "[]"),
	})
	reqs, results, err := svc.RefreshAll(MessageContext{SenderOpenID: "ou_me"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 1 || reqs[0].ID != "rvw_active" {
		t.Fatalf("expected only active review refreshed, got %#v", reqs)
	}
	if len(results) != 1 || results[0].PRState != "OPEN" {
		t.Fatalf("unexpected results: %#v", results)
	}
}

func TestRetryAndSubmitRejectArchived(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newRefreshReview(t, st, "rvw_arch", "archived", "")

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled: true, AllowedUsers: []string{"ou_me"},
		Repos: map[string]string{"a/b": t.TempDir()},
		GHCli: fakeGHRefresh(t, "OPEN", "[]"),
	})
	ctx := MessageContext{SenderOpenID: "ou_me"}
	if _, err := svc.Retry("rvw_arch", ctx); !errors.Is(err, ErrArchived) {
		t.Fatalf("expected ErrArchived from retry, got %v", err)
	}
	if _, err := svc.Submit(MessageContext{
		Text: "https://github.com/a/b/pull/1", SenderOpenID: "ou_me",
	}); !errors.Is(err, ErrArchived) {
		t.Fatalf("expected ErrArchived from submit, got %v", err)
	}
}

func TestFormatRefreshResult(t *testing.T) {
	svc := New(nil, &fakeLark{enabled: true}, Config{})
	req := store.ReviewRequest{
		ID: "rvw_x", PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	}
	res := RefreshResult{PRState: "MERGED", Archived: true, TaskCompleted: true}
	got := svc.FormatRefreshResult(req, res)
	for _, want := range []string{"rvw_x", "MERGED", "archived", "task completed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatRefreshResult missing %q:\n%s", want, got)
		}
	}

	sweep := svc.FormatRefreshAll([]store.ReviewRequest{req}, []RefreshResult{res})
	if !strings.Contains(sweep, "rvw_x") || !strings.Contains(sweep, "archived") {
		t.Fatalf("FormatRefreshAll missing info:\n%s", sweep)
	}
}

func newReviewingReview(t *testing.T, st *store.Store, id string) store.ReviewRequest {
	t.Helper()
	thread, err := st.GetOrCreateReviewThread("oc_c", "om_1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := st.CreateReview(store.ReviewRequest{
		ID: id, ThreadID: thread.ID, Status: "reviewing",
		PRURL: "https://github.com/a/b/pull/1", Repo: "a/b", PRNumber: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateReviewRun(store.ReviewRun{ReviewID: req.ID, Status: "running", Tool: "opencode"}); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestIngestResultPersistsStructuredResult(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_ingest")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true})
	if err := svc.IngestResult("rvw_ingest", "request_change", "Fixes the restore race.", []Finding{
		{Severity: "p0", Path: "internal/etcd_restore.go", Line: 40, Title: "data race", Message: "shared err var", Suggestion: "use a channel"},
		{Severity: "P2", Path: "internal/foo.go", Line: 5, Title: "nit", Message: "unused import", Suggestion: "remove it"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get("rvw_ingest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "request_change" || got.Summary != "Fixes the restore race." {
		t.Fatalf("verdict/summary not stored: %#v", got)
	}
	findings := svc.ListFindings("rvw_ingest")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %#v", findings)
	}
	if findings[0].Severity != "P0" || findings[0].Path != "internal/etcd_restore.go" || findings[0].Line != 40 {
		t.Fatalf("finding #0 mismatch: %#v", findings[0])
	}
	if findings[0].RunID == "" {
		t.Fatalf("finding not tied to the running run: %#v", findings[0])
	}
	if findings[1].Severity != "P2" || findings[1].Suggestion != "remove it" {
		t.Fatalf("finding #1 mismatch: %#v", findings[1])
	}
}

func TestIngestResultPersistsFindingsAcrossRounds(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_repl")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true})
	if err := svc.IngestResult("rvw_repl", "lgtm", "all good", []Finding{
		{Severity: "P0", Path: "a.go", Line: 1, Message: "old finding"},
	}); err != nil {
		t.Fatal(err)
	}
	first := svc.ListFindings("rvw_repl")
	if len(first) != 1 {
		t.Fatalf("expected 1 finding after first ingest, got %d", len(first))
	}
	firstID := first[0].ID

	// A later round updates the first finding's status by id and adds a new one;
	// the old finding must persist (not be replaced away).
	if err := svc.IngestResult("rvw_repl", "nit_comments", "mostly good", []Finding{
		{ID: firstID, Status: "addressed"},
		{Severity: "P2", Path: "b.go", Line: 5, Message: "new finding"},
	}); err != nil {
		t.Fatal(err)
	}
	findings := svc.ListFindings("rvw_repl")
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings (old + new), got %d: %#v", len(findings), findings)
	}
	if findings[0].ID != firstID || findings[0].Status != "addressed" {
		t.Fatalf("first finding status not updated: %#v", findings[0])
	}
	if findings[1].ID == firstID || findings[1].Status != "open" {
		t.Fatalf("new finding should be open: %#v", findings[1])
	}
}

func TestIngestResultValidation(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_val")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true})
	badVerdict := func() error { return svc.IngestResult("rvw_val", "approve", "summary", nil) }
	if err := badVerdict(); err == nil || !strings.Contains(err.Error(), "invalid verdict") {
		t.Fatalf("expected invalid-verdict error, got %v", err)
	}
	emptySummary := func() error { return svc.IngestResult("rvw_val", "lgtm", "  ", nil) }
	if err := emptySummary(); err == nil || !strings.Contains(err.Error(), "summary is required") {
		t.Fatalf("expected empty-summary error, got %v", err)
	}
	badSeverity := func() error {
		return svc.IngestResult("rvw_val", "lgtm", "summary", []Finding{{Severity: "P5", Path: "a.go", Message: "m"}})
	}
	if err := badSeverity(); err == nil || !strings.Contains(err.Error(), "invalid severity") {
		t.Fatalf("expected invalid-severity error, got %v", err)
	}
	emptyPath := func() error {
		return svc.IngestResult("rvw_val", "lgtm", "summary", []Finding{{Severity: "P0", Message: "m"}})
	}
	if err := emptyPath(); err == nil || !strings.Contains(err.Error(), "empty path") {
		t.Fatalf("expected empty-path error, got %v", err)
	}
	if _, err := svc.Get("rvw_val"); err != nil {
		t.Fatal(err)
	}
}

func TestIngestResultRejectsWrongStateAndDisabled(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_state")

	disabled := New(st, &fakeLark{enabled: true}, Config{Enabled: false})
	if err := disabled.IngestResult("rvw_state", "lgtm", "s", nil); err == nil {
		t.Fatal("expected disabled error")
	}

	// Mark the review finished with a verdict already recorded: a second
	// submission is rejected so a late duplicate cannot clobber the result.
	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true})
	if err := svc.IngestResult("rvw_state", "lgtm", "s", nil); err != nil {
		t.Fatal(err)
	}
	req, err := st.GetReview("rvw_state")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}
	if err := svc.IngestResult("rvw_state", "lgtm", "s", nil); err == nil || !strings.Contains(err.Error(), "already has a structured result") {
		t.Fatalf("expected duplicate-result rejection, got %v", err)
	}

	// Missing review -> ErrNotFound.
	if err := svc.IngestResult("rvw_missing", "lgtm", "s", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFormatFindingsAndStructuredFormatResult(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_fmt")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true})
	if err := svc.IngestResult("rvw_fmt", "request_change", "Fixes the restore race.", []Finding{
		{Severity: "P0", Path: "internal/etcd_restore.go", Line: 40, Title: "data race", Message: "shared err var", Suggestion: "use a channel"},
	}); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get("rvw_fmt")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	got := svc.FormatResult(req)
	for _, want := range []string{"Verdict: Request change", "Fixes the restore race.", "Findings:", "[P0] data race (internal/etcd_restore.go:40)", "Fix: use a channel"} {
		if !strings.Contains(got, want) {
			t.Fatalf("FormatResult missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "REVIEW SUMMARY:") {
		t.Fatalf("FormatResult should prefer the structured result, not the text summary:\n%s", got)
	}

	// FormatFindings with an empty slice renders nothing.
	if rendered := FormatFindings(nil); rendered != "" {
		t.Fatalf("expected empty findings render, got %q", rendered)
	}
}

func TestPostPRInlineComments(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_post")

	gh := (&fakeGHPost{}).ghScript(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("FGH_LOG", log)

	svc := New(st, &fakeLark{enabled: true}, Config{
		Enabled: true, GHCli: gh,
		PostInlineComments:    true,
		PostInlineMinSeverity: "P1",
	})
	if err := svc.IngestResult("rvw_post", "request_change", "Fixes the restore race.", []Finding{
		{Severity: "P0", Path: "internal/etcd_restore.go", Line: 40, Title: "data race", Message: "shared err var", Suggestion: "use a channel"},
		{Severity: "P1", Path: "internal/foo.go", Line: 5, Title: "lost update", Message: "update can be lost"},
		{Severity: "P2", Path: "internal/nit.go", Line: 2, Title: "style", Message: "reorder imports"},
		{Severity: "P1", Path: "internal/outofdiff.go", Line: 0, Title: "no line", Message: "needs a line"},
	}); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get("rvw_post")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	req.HeadCommit = "abc1234"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	posted, err := svc.PostPRInlineComments(req)
	if err != nil {
		t.Fatal(err)
	}
	if posted != 2 {
		t.Fatalf("expected 2 inline comments (P0+P1), got %d", posted)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 gh calls (2 comments + 1 review), got %d: %#v", len(lines), lines)
	}
	var comments int
	var reviewLine string
	for _, line := range lines {
		parts := strings.SplitN(line, " ", 3)
		if strings.HasSuffix(parts[1], "/comments") {
			comments++
			if !strings.Contains(parts[2], `"line":40`) && !strings.Contains(parts[2], `"line":5`) {
				t.Fatalf("unexpected inline comment payload: %q", parts[2])
			}
			if strings.Contains(parts[2], "nit.go") {
				t.Fatalf("P2 finding should be skipped: %q", parts[2])
			}
			if strings.Contains(parts[2], "outofdiff") {
				t.Fatalf("finding without line should be skipped: %q", parts[2])
			}
		}
		if strings.HasSuffix(parts[1], "/reviews") {
			reviewLine = parts[2]
		}
	}
	if comments != 2 {
		t.Fatalf("expected 2 inline comments, got %d", comments)
	}
	if !strings.Contains(reviewLine, `"event":"REQUEST_CHANGES"`) {
		t.Fatalf("verdict not mapped to review event: %q", reviewLine)
	}
	if !strings.Contains(reviewLine, "Fixes the restore race.") {
		t.Fatalf("summary not carried into the review: %q", reviewLine)
	}
}

func TestPostPRInlineCommentsGating(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_gate")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true, GHCli: "gh"})
	if err := svc.IngestResult("rvw_gate", "lgtm", "all good", nil); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get("rvw_gate")
	if err != nil {
		t.Fatal(err)
	}

	// No verdict -> no-op (regardless of the PostInlineComments flag).
	req.Verdict = ""
	if posted, err := svc.PostPRInlineComments(req); err != nil || posted != 0 {
		t.Fatalf("expected no-op without verdict, got posted=%d err=%v", posted, err)
	}

	// Verdict set but no gh cli -> failure surfaced.
	req.Verdict = "lgtm"
	if posted, err := svc.PostPRInlineComments(req); err == nil {
		t.Fatalf("expected gh failure surfaced (no gh stub), got posted=%d err=%v", posted, err)
	}
}

func TestPostCommentsManualTrigger(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_manual")

	gh := (&fakeGHPost{}).ghScript(t)
	log := filepath.Join(t.TempDir(), "gh.log")
	t.Setenv("FGH_LOG", log)

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true, GHCli: gh, PostInlineComments: true})
	if err := svc.IngestResult("rvw_manual", "nit_comments", "mostly fine", []Finding{
		{Severity: "P2", Path: "a.go", Line: 3, Title: "nit", Message: "reorder imports"},
	}); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get("rvw_manual")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	// Trigger the manual publish path (feature was "off" for the auto path, but
	// PostComments works regardless of PostInlineComments flag since it is an
	// explicit reviewer action).
	posted, err := svc.PostComments("rvw_manual")
	if err != nil {
		t.Fatal(err)
	}
	if posted != 1 {
		t.Fatalf("expected 1 inline comment, got %d", posted)
	}

	got, err := svc.Get("rvw_manual")
	if err != nil {
		t.Fatal(err)
	}
	if got.CommentsPostedAt.IsZero() {
		t.Fatal("comments_posted_at not recorded after successful publish")
	}

	// Re-triggering is refused so no duplicates are posted.
	if _, err := svc.PostComments("rvw_manual"); err == nil || !strings.Contains(err.Error(), "already posted") {
		t.Fatalf("expected already-posted refusal, got %v", err)
	}
}

func TestPostCommentsRejectsInvalid(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_manual2")
	newRefreshReview(t, st, "rvw_arch", "archived", "")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true, GHCli: "gh"})
	// No verdict yet.
	if _, err := svc.PostComments("rvw_manual2"); err == nil || !strings.Contains(err.Error(), "no structured verdict") {
		t.Fatalf("expected no-verdict error, got %v", err)
	}
	// Archived review.
	if _, err := svc.PostComments("rvw_arch"); !errors.Is(err, ErrArchived) {
		t.Fatalf("expected ErrArchived, got %v", err)
	}
	// Missing review.
	if _, err := svc.PostComments("rvw_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// Disabled service.
	disabled := New(st, &fakeLark{enabled: true}, Config{Enabled: false})
	if _, err := disabled.PostComments("rvw_manual2"); err == nil {
		t.Fatal("expected disabled error")
	}
}

// fakeGHPostResolve writes a combined "gh" stub handling inline comment POSTs,
// the verdict review POST and the two GraphQL calls of ResolveInlineComment.
// The inline comment and review POSTs read stdin and answer with the given id.
func fakeGHPostResolve(t *testing.T, commentID string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "gh")
	lookup := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":"T_1","isResolved":false,"comments":{"nodes":[{"databaseId":` + commentID + `}]}}]}}}}}`
	content := `#!/bin/sh
case "$*" in
  *pulls/*/comments*) cat >/dev/null; printf '%s\n' '{"id":` + commentID + `}' ;;
  *pulls/*/reviews*) cat >/dev/null; printf '%s\n' '{"id":99}' ;;
  *reviewThreads*) printf '%s\n' '` + lookup + `' ;;
  *resolveReviewThread*) printf '%s\n' '{"data":{"resolveReviewThread":{"thread":{"id":"T_1","isResolved":true}}}' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestPostPRInlineCommentsRecordsIDsAndResolvesAddressed(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/saturnus.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	newReviewingReview(t, st, "rvw_resolve")

	svc := New(st, &fakeLark{enabled: true}, Config{Enabled: true, GHCli: fakeGHPostResolve(t, "123456"), PostInlineComments: true})
	if err := svc.IngestResult("rvw_resolve", "request_change", "needs work", []Finding{
		{Severity: "P0", Path: "a.go", Line: 1, Message: "blocking bug"},
	}); err != nil {
		t.Fatal(err)
	}
	firstID := svc.ListFindings("rvw_resolve")[0].ID
	// Pretend round 1 already posted this finding's inline comment.
	if err := st.SetFindingCommentID(firstID, "123456"); err != nil {
		t.Fatal(err)
	}

	// Round 2: the old finding is addressed, a new one is introduced.
	if err := svc.IngestResult("rvw_resolve", "nit_comments", "addressed, one nit left", []Finding{
		{ID: firstID, Status: "addressed"},
		{Severity: "P2", Path: "b.go", Line: 5, Message: "new nit"},
	}); err != nil {
		t.Fatal(err)
	}
	req, err := svc.Get("rvw_resolve")
	if err != nil {
		t.Fatal(err)
	}
	req.Status = "succeeded"
	req.HeadCommit = "abc1234"
	if _, err := st.UpdateReview(req); err != nil {
		t.Fatal(err)
	}

	posted, err := svc.PostPRInlineComments(req)
	if err != nil {
		t.Fatal(err)
	}
	// One new inline comment (b.go); the addressed one is resolved, not re-posted.
	if posted != 1 {
		t.Fatalf("expected 1 new inline comment, got %d", posted)
	}

	findings := svc.ListFindings("rvw_resolve")
	var oldF, newF *store.ReviewFinding
	for i := range findings {
		if findings[i].ID == firstID {
			oldF = &findings[i]
		} else {
			newF = &findings[i]
		}
	}
	if oldF == nil || oldF.Status != "resolved" || oldF.GithubCommentID != "123456" {
		t.Fatalf("addressed finding should be resolved with its comment id kept: %#v", oldF)
	}
	if newF == nil || newF.GithubCommentID == "" {
		t.Fatalf("new finding should have its comment id recorded: %#v", newF)
	}
}
