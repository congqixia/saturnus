package store

import (
	"database/sql"
	"errors"
	"time"
)

// WorkerTokenAlertSentAt persists notification deduplication across restarts.
func (s *Store) WorkerTokenAlertSentAt(workerID, key string) (time.Time, error) {
	var raw string
	err := s.db.QueryRow(`SELECT sent_at FROM worker_token_alerts WHERE worker_id=? AND alert_key=?`, workerID, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func (s *Store) MarkWorkerTokenAlertSent(workerID, key string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO worker_token_alerts(worker_id,alert_key,sent_at) VALUES(?,?,?)
 ON CONFLICT(worker_id,alert_key) DO UPDATE SET sent_at=excluded.sent_at`, workerID, key, timeText(at))
	return err
}
