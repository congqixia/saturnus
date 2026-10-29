package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LatestRun also returns finished runs, so a late callback can be checked
// against the most recent execution rather than a previous round.
func (s *Store) LatestRun(reviewID string) (ReviewRun, error) {
	run, err := scanReviewRun(s.db.QueryRow(`SELECT id, review_id, seq, status, head_commit,
 retry_reason, session_id, worker_id, result_text, error, tool, verdict, summary,
 started_at, completed_at, updated_at FROM review_runs WHERE review_id=? ORDER BY seq DESC LIMIT 1`, reviewID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRun{}, ErrNotFound
	}
	return run, err
}

// LatestReviewVerdict reads only the last available conclusion, without
// loading potentially large transcripts from the run history.
func (s *Store) LatestReviewVerdict(reviewID string) (string, error) {
	var verdict string
	err := s.db.QueryRow(`SELECT verdict FROM review_runs
 WHERE review_id=? AND verdict!='' ORDER BY seq DESC LIMIT 1`, reviewID).Scan(&verdict)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return verdict, err
}

// SaveReviewResult commits the result and its findings together. A run's
// verdict is recorded immediately, so completion can require a result from
// this execution rather than accept a verdict left over from a prior run.
func (s *Store) SaveReviewResult(reviewID, runID, verdict, summary string, findings []ReviewFinding) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	now := timeText(time.Now().UTC())
	if runID != "" {
		res, err := tx.ExecContext(ctx, `UPDATE review_runs SET verdict=?,summary=?,updated_at=?
 WHERE id=? AND review_id=? AND status IN ('running','succeeded')
 AND seq=(SELECT MAX(seq) FROM review_runs WHERE review_id=?)`, verdict, summary, now, runID, reviewID, reviewID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrNotPending
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE review_requests SET verdict=?,summary=?,updated_at=? WHERE id=?`, verdict, summary, now, reviewID); err != nil {
		return err
	}
	if err := saveFindingsTx(ctx, tx, reviewID, runID, findings); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteReviewRun publishes the terminal review and run state together.
// Pollers must never observe a succeeded review with its run still running.
// Ownership and run fences also roll back the entire transition when stale.
func (s *Store) CompleteReviewRun(req ReviewRequest, runID string) (bool, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	now := timeText(time.Now().UTC())
	res, err := tx.ExecContext(ctx, `UPDATE review_requests
 SET status=?,head_commit=?,pr_head_commit=?,session_id=?,retry_reason=?,result_text=?,error=?,completed_at=?,updated_at=?
 WHERE id=? AND status='reviewing' AND worker_id=?`, req.Status, req.HeadCommit, req.PrHeadCommit, req.SessionID,
		req.RetryReason, req.ResultText, req.Error, nullableTimeText(req.CompletedAt), now, req.ID, req.WorkerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if runID != "" {
		res, err := tx.ExecContext(ctx, `UPDATE review_runs
 SET status=?,head_commit=?,session_id=?,result_text=?,error=?,verdict=?,summary=?,completed_at=?,updated_at=?
 WHERE id=? AND review_id=? AND status='running' AND worker_id=?`, req.Status, req.HeadCommit, req.SessionID,
			req.ResultText, req.Error, req.Verdict, req.Summary, nullableTimeText(req.CompletedAt), now, runID, req.ID, req.WorkerID)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return false, err
		}
	}
	return true, tx.Commit()
}
