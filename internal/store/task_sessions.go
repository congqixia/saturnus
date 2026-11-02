package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// TaskSession identifies a tool-native session; it need not be registered in sessions.
type TaskSession struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Tool      string    `json:"tool"`
	SessionID string    `json:"session_id"`
	Purpose   string    `json:"purpose"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) BindTaskIssue(id, repo string, number int, url, title, state string) (Task, error) {
	result, err := s.db.Exec(`UPDATE tasks SET issue_repo=?,issue_number=?,issue_url=?,issue_title=?,issue_state=?,updated_at=? WHERE id=? AND status!='archived'`, repo, number, url, title, state, timeText(time.Now().UTC()), id)
	if err != nil {
		return Task{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Task{}, err
	}
	if n == 0 {
		return Task{}, ErrNotFound
	}
	return s.GetTask(id)
}

func (s *Store) AddTaskSession(link TaskSession) (TaskSession, error) {
	link.Tool = strings.ToLower(strings.TrimSpace(link.Tool))
	link.SessionID = strings.TrimSpace(link.SessionID)
	if link.Tool == "" || link.SessionID == "" {
		return TaskSession{}, errors.New("tool and session_id are required")
	}
	link.ID = "tss_" + newID()
	link.CreatedAt = time.Now().UTC()
	result, err := s.db.Exec(`INSERT INTO task_sessions (id,task_id,tool,session_id,purpose,note,created_at) SELECT ?,id,?,?,?,?,? FROM tasks WHERE id=? AND status!='archived'`, link.ID, link.Tool, link.SessionID, link.Purpose, link.Note, timeText(link.CreatedAt), link.TaskID)
	if err != nil {
		return TaskSession{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return TaskSession{}, err
	}
	if n == 0 {
		return TaskSession{}, ErrNotFound
	}
	return link, nil
}

func (s *Store) ListTaskSessions(taskID string) ([]TaskSession, error) {
	rows, err := s.db.QueryContext(context.Background(), `SELECT id,task_id,tool,session_id,purpose,note,created_at FROM task_sessions WHERE task_id=? ORDER BY created_at`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []TaskSession{}
	for rows.Next() {
		var link TaskSession
		var created string
		if err := rows.Scan(&link.ID, &link.TaskID, &link.Tool, &link.SessionID, &link.Purpose, &link.Note, &created); err != nil {
			return nil, err
		}
		link.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func (s *Store) RemoveTaskSession(taskID, id string) error {
	result, err := s.db.Exec(`DELETE FROM task_sessions WHERE id=? AND task_id=? AND EXISTS (SELECT 1 FROM tasks WHERE id=? AND status!='archived')`, id, taskID, taskID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
