package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyTaskRefreshMigrationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tasks (
 id TEXT PRIMARY KEY,title TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'open',
 issue_repo TEXT NOT NULL DEFAULT '',issue_number INTEGER NOT NULL DEFAULT 0,
 issue_title TEXT NOT NULL DEFAULT '',issue_state TEXT NOT NULL DEFAULT '',issue_url TEXT NOT NULL DEFAULT '',
 description TEXT NOT NULL DEFAULT '',archived_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	task, err := st.CreateTask(Task{Title: "Task", IssueRepo: "a/b", IssueNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !task.RefreshedAt.IsZero() {
		t.Fatal("legacy task started fresh")
	}
	task.RefreshAttemptedAt = time.Now().UTC()
	task.RefreshError = "timeline failed"
	saved, err := st.SaveTaskRefresh(context.Background(), task, true)
	if err != nil || saved.RefreshError != "timeline failed" || saved.RefreshAttemptedAt.IsZero() || !saved.RefreshedAt.IsZero() {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err = st.GetTask(task.ID)
	if err != nil || saved.RefreshError != "timeline failed" || saved.RefreshAttemptedAt.IsZero() {
		t.Fatalf("reopened=%+v err=%v", saved, err)
	}
}

func TestRefreshAssociationGuardAndCanonicalID(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.CreateTask(Task{Title: "Task"})
	if err != nil {
		t.Fatal(err)
	}
	input := TaskPR{TaskID: task.ID, Repo: "a/b", PRNumber: 7}
	first, err := st.AddTaskPR(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AddTaskPR(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("upsert returned a nonexistent ID: %s / %s", first.ID, second.ID)
	}
	status := "done"
	if _, err := st.PatchTask(task.ID, nil, &status, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateTaskPRContext(context.Background(), first, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("automatic write to done task: %v", err)
	}
	if _, err := st.CloseTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskPRContext(context.Background(), input, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("association write after close: %v", err)
	}
}
