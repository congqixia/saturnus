package store

import (
	"database/sql"
	"errors"
	"time"
)

type TaskLarkLink struct {
	TaskID      string `json:"task_id"`
	GUID        string `json:"guid"`
	URL         string `json:"url"`
	GroupID     string `json:"group_id"`
	RequestJSON string `json:"-"`
	StartedAt   string `json:"-"`
	LastError   string `json:"last_error"`
	SyncedAt    string `json:"synced_at"`
}

func (s *Store) GetTaskLarkLink(id string) (TaskLarkLink, error) {
	var link TaskLarkLink
	err := s.db.QueryRow(`SELECT task_id,guid,url,group_id,request_json,started_at,last_error,synced_at FROM task_lark_links WHERE task_id=?`, id).Scan(&link.TaskID, &link.GUID, &link.URL, &link.GroupID, &link.RequestJSON, &link.StartedAt, &link.LastError, &link.SyncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskLarkLink{}, ErrNotFound
	}
	return link, err
}

// ReserveTaskLarkCreation persists the exact request before contacting Lark.
func (s *Store) ReserveTaskLarkCreation(id, request string) error {
	_, err := s.db.Exec(`INSERT INTO task_lark_links(task_id,request_json,started_at) VALUES(?,?,?) ON CONFLICT(task_id) DO NOTHING`, id, request, timeText(time.Now().UTC()))
	return err
}

func (s *Store) SetTaskLarkRemote(id, guid, url string) error {
	result, err := s.db.Exec(`UPDATE task_lark_links SET guid=?,url=?,last_error='' WHERE task_id=? AND (guid='' OR guid=?)`, guid, url, id, guid)
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

func (s *Store) SetTaskLarkGroup(id, group string) error {
	_, err := s.db.Exec(`UPDATE task_lark_links SET group_id=? WHERE task_id=?`, group, id)
	return err
}

func (s *Store) SetTaskLarkSync(id, errText string, success bool) error {
	var synced any
	if success {
		synced = timeText(time.Now().UTC())
	}
	_, err := s.db.Exec(`UPDATE task_lark_links SET last_error=?,synced_at=COALESCE(?,synced_at) WHERE task_id=?`, errText, synced, id)
	return err
}

// ResetTaskLarkCreation is safe only after confirming no remote task exists.
func (s *Store) ResetTaskLarkCreation(id string) error {
	result, err := s.db.Exec(`DELETE FROM task_lark_links WHERE task_id=? AND guid=''`, id)
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
