package store

import "time"

// ReviewRequesterNotification is a snapshot of a result notification and its delivery outcome.
type ReviewRequesterNotification struct {
	ID              string    `json:"id"`
	ReviewID        string    `json:"review_id"`
	RunID           string    `json:"run_id,omitempty"`
	RequesterOpenID string    `json:"requester_open_id"`
	Message         string    `json:"message"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

func (s *Store) CreateRequesterNotification(n ReviewRequesterNotification) (ReviewRequesterNotification, error) {
	n.ID = newID()
	n.CreatedAt = time.Now().UTC()
	n.Status = "pending"
	_, err := s.db.Exec(`INSERT INTO review_requester_notifications
		(id, review_id, run_id, requester_open_id, message, status, error, created_at)
		VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, '', ?)`, n.ID, n.ReviewID, n.RunID, n.RequesterOpenID, n.Message, n.Status, timeText(n.CreatedAt))
	return n, err
}

func (s *Store) FinishRequesterNotification(id, status, errText string) error {
	_, err := s.db.Exec(`UPDATE review_requester_notifications SET status=?, error=? WHERE id=?`, status, errText, id)
	return err
}

func (s *Store) ListRequesterNotifications(reviewID string) ([]ReviewRequesterNotification, error) {
	rows, err := s.db.Query(`SELECT id, review_id, COALESCE(run_id, ''), requester_open_id, message, status, error, created_at
		FROM review_requester_notifications WHERE review_id=? ORDER BY created_at, id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ReviewRequesterNotification{}
	for rows.Next() {
		var n ReviewRequesterNotification
		var created string
		if err := rows.Scan(&n.ID, &n.ReviewID, &n.RunID, &n.RequesterOpenID, &n.Message, &n.Status, &n.Error, &created); err != nil {
			return nil, err
		}
		n.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, rows.Err()
}
