package review

import (
	"context"
	"errors"

	"saturnus/internal/reviewlog"
	"saturnus/internal/store"
)

// ContextID is the current tool-session namespace, not an authentication token.
func (s *Service) ContextID(reviewID string) (string, error) {
	if s.store == nil {
		return "", nil
	}
	return s.store.ReviewContextID(reviewID)
}

// ResetContext bypasses the unchanged-head retry check and creates a new tool
// context. Active reviews/reactions cannot be reset beneath a running process.
func (s *Service) ResetContext(id string, msg MessageContext) (store.ReviewRequest, error) {
	if !s.cfg.Enabled {
		return store.ReviewRequest{}, errors.New("PR review is disabled")
	}
	if !s.isReviewer(msg.SenderOpenID) {
		return store.ReviewRequest{}, errors.New("sender is not the reviewer")
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	unlock, err := s.syncLocks.Lock(ctx, id)
	if err != nil {
		return store.ReviewRequest{}, err
	}
	defer unlock()
	req, contextID, err := s.store.ResetReviewContext(id, msg.SenderOpenID)
	if errors.Is(err, store.ErrReviewContextArchived) {
		return req, ErrArchived
	}
	if err != nil {
		return req, err
	}
	reviewlog.Event{Name: "context_reset", Component: "server", Kind: "review", ReviewID: id, ContextID: contextID, WorkerID: req.WorkerID, Repo: req.Repo, PRNumber: req.PRNumber, Status: "pending"}.Log()
	s.enqueue("review", id)
	return req, nil
}

// requireContextWorker protects both review runs and subsequent reactions when
// an existing worker has been downgraded or replaced by a legacy process.
func (s *Service) requireContextWorker(contextID, workerID string) error {
	if contextID == "" {
		return nil
	}
	agent, err := s.store.GetWorker(workerID)
	if err != nil {
		return err
	}
	caps, err := agent.CapabilitiesDecoded()
	if err != nil {
		return err
	}
	if !caps.ContextReset {
		return errors.New("worker does not support context reset; upgrade saturnus-worker")
	}
	return nil
}
