package tasks

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"

	"saturnus/internal/lark"
	"saturnus/internal/store"
)

func TestLarkTaskGroupDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	remote := &fakeTaskLark{}
	svc := New(st, Config{Enabled: true, Lark: remote, LarkDebug: true, LarkTaskGroupID: "group-1"})
	task, err := svc.CreateIndependent("task", "")
	if err != nil {
		t.Fatal(err)
	}
	svc.LogLarkAutoSync(task.ID, "bot")
	if _, err := svc.SyncLark(context.Background(), task.ID, "owner", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "owner", ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"event=auto_sync_decision", "source=\"bot\"", "event=group_resolved", "source=\"configured\"", "event=group_add_start", "event=group_add_complete", "reason=\"already_recorded\"", "group_id=\"group-1\"", task.ID} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in logs: %s", want, logs.String())
		}
	}
	logs.Reset()
	remote2 := &fakeTaskLark{groupErr: errors.New("permission denied")}
	svc2 := New(st, Config{Enabled: true, Lark: remote2, LarkTaskGroupID: "group-2"})
	task2, err := svc2.CreateIndependent("task 2", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.SyncLark(context.Background(), task2.ID, "owner", ""); err == nil {
		t.Fatal("expected group failure")
	}
	for _, want := range []string{"event=failed", "stage=\"add_to_group\"", "group_id=\"group-2\"", "permission denied"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %q in error log: %s", want, logs.String())
		}
	}
	logs.Reset()
	remote3 := &fakeTaskLark{}
	svc3 := New(st, Config{Enabled: true, Lark: remote3, LarkDebug: true})
	task3, err := svc3.CreateIndependent("task 3", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc3.SyncLark(context.Background(), task3.ID, "owner", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "reason=\"no_group_configured\"") {
		t.Fatalf("missing no-group reason: %s", logs.String())
	}
}

type fakeTaskLark struct {
	mu        sync.Mutex
	created   []lark.CreateTaskInput
	task      lark.Task
	groups    []string
	createErr error
	groupErr  error
}

func (f *fakeTaskLark) Enabled() bool { return true }
func (f *fakeTaskLark) CreateTask(in lark.CreateTaskInput) (lark.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
	if f.createErr != nil {
		return lark.Task{}, f.createErr
	}
	f.task = lark.Task{GUID: "guid-1", URL: "https://example.com/task", Summary: in.Summary, Description: in.Description}
	return f.task, nil
}
func (f *fakeTaskLark) GetTaskContext(_ context.Context, _ string) (lark.Task, error) {
	return f.task, nil
}
func (f *fakeTaskLark) UpdateTaskContext(_ context.Context, _ string, in lark.UpdateTaskInput) error {
	if in.Summary != nil {
		f.task.Summary = *in.Summary
	}
	if in.Description != nil {
		f.task.Description = *in.Description
	}
	if in.Completed != nil {
		f.task.Completed = *in.Completed
	}
	return nil
}
func (f *fakeTaskLark) AddTaskToList(_, group string) error {
	f.groups = append(f.groups, group)
	return f.groupErr
}

func TestLarkTaskSyncAndPreserveUserNotes(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	remote := &fakeTaskLark{}
	svc := New(st, Config{Enabled: true, Lark: remote, LarkTaskGroupID: "group-default"})
	task, err := svc.CreateIndependent("Improve task", "original description")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.AddRef(task.ID, "https://example.com/spec", "Spec"); err != nil {
		t.Fatal(err)
	}
	link, err := svc.SyncLark(context.Background(), task.ID, "ou_owner", "group-override")
	if err != nil {
		t.Fatal(err)
	}
	if link.GUID != "guid-1" || link.GroupID != "group-override" || len(remote.created) != 1 || remote.created[0].ClientToken != task.ID || !reflect.DeepEqual(remote.created[0].MemberOpenIDs, []string{"ou_owner"}) {
		t.Fatalf("link=%+v created=%+v", link, remote.created)
	}
	remote.task.Description = "my note\n\n" + remote.task.Description + "\n\nkeep me"
	title := "Updated task"
	status := store.TaskStatusDone
	if _, err = st.PatchTask(task.ID, &title, &status, nil); err != nil {
		t.Fatal(err)
	}
	link, err = svc.SyncLark(context.Background(), task.ID, "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(remote.created) != 1 || len(remote.groups) != 1 || link.GroupID != "group-override" || !remote.task.Completed || remote.task.Summary != title || !strings.Contains(remote.task.Description, "my note") || !strings.Contains(remote.task.Description, "keep me") || !strings.Contains(remote.task.Description, "https://example.com/spec") {
		t.Fatalf("link=%+v task=%+v groups=%v", link, remote.task, remote.groups)
	}
}

func TestLarkTaskRetryPreservesCreateRequest(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	remote := &fakeTaskLark{createErr: errors.New("timeout")}
	svc := New(st, Config{Enabled: true, Lark: remote})
	task, err := svc.CreateIndependent("Old title", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "ou_first", ""); err == nil {
		t.Fatal("expected timeout")
	}
	link, err := st.GetTaskLarkLink(task.ID)
	if err != nil || link.LastError == "" || link.GUID != "" {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	title := "Changed title"
	if _, err := st.PatchTask(task.ID, &title, nil, nil); err != nil {
		t.Fatal(err)
	}
	remote.createErr = nil
	if _, err := svc.SyncLark(context.Background(), task.ID, "ou_other", ""); err != nil {
		t.Fatal(err)
	}
	if len(remote.created) != 2 || !reflect.DeepEqual(remote.created[0], remote.created[1]) || remote.task.Summary != title {
		t.Fatalf("requests=%+v task=%+v", remote.created, remote.task)
	}
	if err := svc.ResetLarkCreation(context.Background(), task.ID); err == nil {
		t.Fatal("reset an existing link")
	}
}

func TestLarkSyncRequiresAssignee(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	remote := &fakeTaskLark{}
	svc := New(st, Config{Enabled: true, Lark: remote})
	task, err := svc.CreateIndependent("Task", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "", ""); err == nil || len(remote.created) != 0 {
		t.Fatalf("unexpected result: %v", err)
	}
}

func TestLarkGroupFailureDoesNotDuplicateTask(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	remote := &fakeTaskLark{groupErr: errors.New("permission denied")}
	svc := New(st, Config{Enabled: true, Lark: remote, LarkTaskGroupID: "group"})
	task, err := svc.CreateIndependent("Task", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "ou_owner", ""); err == nil {
		t.Fatal("expected group error")
	}
	link, err := st.GetTaskLarkLink(task.ID)
	if err != nil || link.GUID == "" || link.LastError == "" {
		t.Fatalf("link=%+v err=%v", link, err)
	}
	remote.groupErr = nil
	if _, err := svc.SyncLark(context.Background(), task.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(remote.created) != 1 || len(remote.groups) != 2 {
		t.Fatalf("created=%d groups=%v", len(remote.created), remote.groups)
	}
}

func TestUncertainLarkCreationRequiresRecovery(t *testing.T) {
	path := t.TempDir() + "/test.db"
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	remote := &fakeTaskLark{createErr: errors.New("timeout")}
	svc := New(st, Config{Enabled: true, Lark: remote})
	task, err := svc.CreateIndependent("Task", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "ou_owner", ""); err == nil {
		t.Fatal("expected timeout")
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_lark_links SET started_at='2020-01-01T00:00:00Z' WHERE task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "ou_owner", ""); err == nil || !strings.Contains(err.Error(), "uncertain") || len(remote.created) != 1 {
		t.Fatalf("unexpected retry: %v", err)
	}
	remote.task = lark.Task{GUID: "existing", URL: "https://example.com/task"}
	if _, err := svc.LinkLark(context.Background(), task.ID, "existing"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncLark(context.Background(), task.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(remote.created) != 1 {
		t.Fatal("created a duplicate task")
	}
}
