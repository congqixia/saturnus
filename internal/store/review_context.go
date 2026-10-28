package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrReviewContextBusy = errors.New("review or reaction is still in progress; wait for it to finish before resetting context")
var ErrReviewContextArchived = errors.New("archived review context cannot be reset")

// ReviewContextID is empty for legacy reviews that have never been reset.
func (s *Store) ReviewContextID(reviewID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT context_id FROM review_contexts WHERE review_id=?`, reviewID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// ResetReviewContext atomically detaches the tool session, rotates its discovery
// namespace and queues a fresh run, preserving review/run/finding history.
func (s *Store) ResetReviewContext(reviewID, actor string) (ReviewRequest, string, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, "", err
	}
	defer rollback(tx)
	req, err := getReviewTx(ctx, tx, reviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, "", ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, "", err
	}
	if req.Status == "pending" || req.Status == "reviewing" {
		return req, "", ErrReviewContextBusy
	}
	if req.Status == "archived" {
		return req, "", ErrReviewContextArchived
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT
 (SELECT COUNT(*) FROM review_reactions WHERE review_id=? AND status IN ('pending','running')) +
 (SELECT COUNT(*) FROM review_runs WHERE review_id=? AND status='running')`, reviewID, reviewID).Scan(&active); err != nil {
		return req, "", err
	}
	if active != 0 {
		return req, "", ErrReviewContextBusy
	}
	contextID := "ctx_" + newID()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_contexts(review_id,context_id,reset_by,reset_at) VALUES(?,?,?,?)
 ON CONFLICT(review_id) DO UPDATE SET context_id=excluded.context_id,reset_by=excluded.reset_by,reset_at=excluded.reset_at`, reviewID, contextID, actor, timeText(now)); err != nil {
		return req, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_requests SET session_id='', status='pending', result_text='', error='',
 verdict='', summary='', completed_at=NULL, comments_posted_at=NULL, retry_reason='review context reset', updated_at=? WHERE id=?`, timeText(now), reviewID); err != nil {
		return req, "", err
	}
	if err := audit(ctx, tx, actor, "review.context_reset", reviewID, mustJSON(map[string]string{"context_id": contextID, "previous_session_id": req.SessionID})); err != nil {
		return req, "", err
	}
	if err := tx.Commit(); err != nil {
		return req, "", err
	}
	req.SessionID, req.ResultText, req.Error, req.Verdict, req.Summary = "", "", "", "", ""
	req.Status, req.RetryReason = "pending", "review context reset"
	req.CompletedAt, req.CommentsPostedAt = time.Time{}, time.Time{}
	req.UpdatedAt = now
	return req, contextID, nil
}

// SetReviewSession records local execution's captured session without replacing
// a concurrently submitted verdict or finding payload.
func (s *Store) SetReviewSession(reviewID, sessionID string) error {
	result, err := s.db.Exec(`UPDATE review_requests SET session_id=?,updated_at=? WHERE id=? AND status='reviewing'`, sessionID, timeText(time.Now().UTC()), reviewID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotPending
	}
	return err
}
