package store

import "testing"

func TestTaskSessionLinks(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task, err := st.CreateTask(Task{Title: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	link, err := st.AddTaskSession(TaskSession{TaskID: task.ID, Tool: "Codex", SessionID: "native-id", Purpose: "analyze"})
	if err != nil {
		t.Fatal(err)
	}
	links, err := st.ListTaskSessions(task.ID)
	if err != nil || len(links) != 1 || links[0].Tool != "codex" || links[0].Purpose != "analyze" {
		t.Fatalf("links=%#v err=%v", links, err)
	}
	if _, err := st.AddTaskSession(TaskSession{TaskID: task.ID, Tool: "codex", SessionID: "native-id"}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := st.RemoveTaskSession("other", link.ID); err == nil {
		t.Fatal("removed another task's link")
	}
	if err := st.RemoveTaskSession(task.ID, link.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddTaskSession(TaskSession{TaskID: task.ID, Tool: "opencode", SessionID: "other"}); err == nil {
		t.Fatal("added to archived task")
	}
}
