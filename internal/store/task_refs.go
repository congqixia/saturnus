package store

import (
	"time"
)

type TaskRef struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) AddTaskRef(ref TaskRef) (TaskRef, error) {
	ref.ID = "trf_" + newID()
	ref.CreatedAt = time.Now().UTC()
	result, err := s.db.Exec(`INSERT INTO task_refs (id,task_id,url,title,created_at) SELECT ?,id,?,?,? FROM tasks WHERE id=? AND status!='archived'`, ref.ID, ref.URL, ref.Title, timeText(ref.CreatedAt), ref.TaskID)
	if err != nil {
		return TaskRef{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return TaskRef{}, err
	}
	if n == 0 {
		return TaskRef{}, ErrNotFound
	}
	return ref, nil
}

func (s *Store) ListTaskRefs(taskID string) ([]TaskRef, error) {
	rows, err := s.db.Query(`SELECT id,task_id,url,title,created_at FROM task_refs WHERE task_id=? ORDER BY created_at,id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []TaskRef{}
	for rows.Next() {
		var ref TaskRef
		var created string
		if err := rows.Scan(&ref.ID, &ref.TaskID, &ref.URL, &ref.Title, &created); err != nil {
			return nil, err
		}
		ref.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (s *Store) RemoveTaskRef(taskID, refID string) error {
	result, err := s.db.Exec(`DELETE FROM task_refs WHERE id=? AND task_id=? AND EXISTS (SELECT 1 FROM tasks WHERE id=? AND status!='archived')`, refID, taskID, taskID)
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
