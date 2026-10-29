package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"saturnus/internal/reviewlog"
	"saturnus/internal/store"
	"saturnus/internal/workerproto"
)

// StartRemote atomically assigns a pending review to a remote worker, records
// the run, and builds the dispatch payload. The caller sends the payload to the
// worker over its control channel.
func (s *Service) StartRemote(req store.ReviewRequest, workerID string) (workerproto.JobPayload, error) {
	if workerID == "" {
		return workerproto.JobPayload{}, errors.New("empty worker id")
	}
	unlock, err := s.syncLocks.Lock(context.Background(), req.ID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	defer unlock()
	req, err = s.store.GetReview(req.ID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	contextID, err := s.ContextID(req.ID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	if err := s.requireContextWorker(contextID, workerID); err != nil {
		return workerproto.JobPayload{}, err
	}
	// Validate guides and build the job before claiming it. Configuration
	// errors must not strand a reviewing row or create a requeue loop.
	payload, err := s.JobPayload(req)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	assigned, err := s.store.AssignReview(req.ID, workerID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	run, err := s.store.CreateReviewRun(store.ReviewRun{
		ReviewID:    assigned.ID,
		Status:      "running",
		RetryReason: assigned.RetryReason,
		Tool:        s.cfg.Tool,
		WorkerID:    workerID,
	})
	if err != nil {
		_ = s.store.RequeueReview(req.ID, workerID)
		return workerproto.JobPayload{}, fmt.Errorf("create remote review run: %w", err)
	}
	payload.RunID = run.ID
	payload.ResultAPIURL = s.resultCallbackURL(req.ID, run.ID)
	return payload, nil
}

// StartRemoteReaction atomically assigns a pending reaction to a remote worker.
// The reaction resumes the owning review's tool session, so it must run on the
// same host that produced that session (the review's worker).
func (s *Service) StartRemoteReaction(reac store.ReviewReaction, req store.ReviewRequest, workerID string) (workerproto.JobPayload, error) {
	if workerID == "" {
		return workerproto.JobPayload{}, errors.New("empty worker id")
	}
	contextID, err := s.ContextID(req.ID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	if err := s.requireContextWorker(contextID, workerID); err != nil {
		return workerproto.JobPayload{}, err
	}
	assigned, err := s.store.AssignReaction(reac.ID, workerID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	sessionID := assigned.SessionID
	if sessionID == "" {
		sessionID = req.SessionID
	}
	return workerproto.JobPayload{
		Kind:        workerproto.JobReaction,
		ReviewID:    req.ID,
		ReactionID:  assigned.ID,
		Repo:        req.Repo,
		PRNumber:    req.PRNumber,
		PRURL:       req.PRURL,
		SessionID:   sessionID,
		ContextID:   contextID,
		Instruction: assigned.Instruction,
		Timeout:     s.cfg.Timeout,
		Tool:        s.cfg.Tool,
	}, nil
}

// JobPayload builds the dispatch payload for a pending review. Explicit guides
// (SATURNUS_REVIEW_GUIDES) are resolved server-side; in-repo guides are left for
// the worker to discover in its own checkout. The command template, result API
// URL and prior findings come from the server's configuration.
func (s *Service) JobPayload(req store.ReviewRequest) (workerproto.JobPayload, error) {
	contextID, err := s.ContextID(req.ID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	runID := ""
	if run, err := s.store.CurrentRun(req.ID); err == nil {
		runID = run.ID
	}
	resultAPIURL := s.resultCallbackURL(req.ID, runID)
	guide := ""
	if path, ok := s.cfg.Guides[req.Repo]; ok && path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return workerproto.JobPayload{}, fmt.Errorf("read guide %s: %w", path, err)
		}
		guide = strings.TrimSpace(string(data))
	}
	tool := s.cfg.Tool
	if tool == "" {
		tool = "opencode"
	}
	return workerproto.JobPayload{
		Kind:            workerproto.JobReview,
		ReviewID:        req.ID,
		Repo:            req.Repo,
		PRNumber:        req.PRNumber,
		PRURL:           req.PRURL,
		Title:           req.Title,
		BaseBranch:      req.BaseBranch,
		HeadCommit:      req.HeadCommit,
		RetryReason:     req.RetryReason,
		SessionID:       req.SessionID,
		ContextID:       contextID,
		PriorFindings:   formatPriorFindings(s.store.ListFindings(req.ID)),
		CommandTemplate: s.cfg.CommandTemplate,
		Guide:           guide,
		ResultAPIURL:    resultAPIURL,
		ResultToken:     s.cfg.ResultToken,
		Tool:            tool,
		Timeout:         s.cfg.Timeout,
	}, nil
}

// CompleteRemote finishes a review whose run was executed on a remote worker:
// it persists the outcome (head commit, session, result), records the run's
// terminal state, publishes inline comments when configured, and notifies the
// reviewer and requester. A stale completion (the review is no longer owned by
// workerID) is rejected.
func (s *Service) CompleteRemote(reviewID, workerID string, ok bool, headCommit, sessionID, result, errText string) error {
	return s.CompleteRemoteRun(reviewID, "", workerID, ok, headCommit, sessionID, result, errText)
}

// CompleteRemoteRun validates the run fence under the same lock used for
// callbacks and requeues, so reassignment cannot race the completion check.
func (s *Service) CompleteRemoteRun(reviewID, runID, workerID string, ok bool, headCommit, sessionID, result, errText string) error {
	if !s.cfg.Enabled {
		return errors.New("PR review is disabled")
	}
	unlock, err := s.syncLocks.Lock(context.Background(), reviewID)
	if err != nil {
		return err
	}
	defer unlock()
	if runID != "" {
		run, err := s.store.CurrentRun(reviewID)
		if err != nil || run.ID != runID || run.WorkerID != workerID {
			return ErrStaleResult
		}
	}
	req, err := s.store.GetReview(reviewID)
	if err != nil {
		return err
	}
	status := "succeeded"
	if !ok {
		status = "failed"
	}
	status, errText = s.completionResult(&req, status, result, errText)
	if headCommit == "" {
		headCommit = req.HeadCommit
	}
	if sessionID == "" {
		sessionID = req.SessionID
	}
	req.Status = status
	req.ResultText = result
	req.Error = errText
	req.HeadCommit = headCommit
	req.PrHeadCommit = headCommit
	req.SessionID = sessionID
	req.RetryReason = ""
	req.CompletedAt = time.Now().UTC()
	runID = ""
	if run, err := s.store.CurrentRun(reviewID); err == nil {
		runID = run.ID
	}
	applied, err := s.store.CompleteReviewRun(req, runID)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("review %s is no longer owned by worker %s (stale completion dropped)", reviewID, workerID)
	}
	reviewlog.Event{Name: "completed", Component: "server", Kind: "review", ReviewID: reviewID, RunID: runID, WorkerID: workerID, Repo: req.Repo, PRNumber: req.PRNumber, Status: status, Error: errText}.Log()
	// Publish inline comments (plus the verdict review) when enabled and the
	// run produced a structured verdict.
	if status == "succeeded" && s.cfg.PostInlineComments && req.Verdict != "" && req.CommentsPostedAt.IsZero() {
		if posted, perr := s.PostPRInlineComments(req); perr != nil {
			logf("post inline comments for %s: %v", req.ID, perr)
		} else {
			if posted > 0 {
				logf("posted %d inline comment(s) for %s", posted, req.ID)
			}
			if err := s.store.SetCommentsPosted(req.ID, time.Now().UTC()); err != nil {
				logf("mark comments posted for %s: %v", req.ID, err)
			}
		}
	}
	s.notifyReviewer(req)
	return nil
}

// CompleteRemoteReaction finishes a reaction executed on a remote worker and
// delivers its outcome to the reviewer.
func (s *Service) CompleteRemoteReaction(reactionID, workerID string, ok bool, sessionID, result, errText string) error {
	if !s.cfg.Enabled {
		return errors.New("PR review is disabled")
	}
	reac, err := s.store.GetReaction(reactionID)
	if err != nil {
		return err
	}
	status := "succeeded"
	if !ok {
		status = "failed"
	}
	if sessionID == "" {
		sessionID = reac.SessionID
	}
	applied, err := s.store.UpdateReactionOutcomeByWorker(reactionID, workerID, status, sessionID, result, errText, time.Now().UTC())
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("reaction %s is no longer owned by worker %s (stale completion dropped)", reactionID, workerID)
	}
	reac.Status = status
	reac.ResultText = result
	reac.Error = errText
	reac.SessionID = sessionID
	reviewlog.Event{Name: "completed", Component: "server", Kind: "reaction", ReviewID: reac.ReviewID, ReactionID: reac.ID, WorkerID: workerID, Status: status, Error: errText}.Log()
	s.notifyReaction(reac)
	return nil
}

// UpdateRemoteProgress persists a chunk of streamed output from a remote
// worker's running job.
func (s *Service) UpdateRemoteProgress(reviewID, text string) error {
	return s.store.UpdateReviewProgress(reviewID, text)
}

// LocalAvailable reports whether the server itself can execute a review for a
// repo: the repo is in its own whitelist and the checkout is a git directory.
func (s *Service) LocalAvailable(repo string) bool {
	dir, ok := s.repoDirFor(repo)
	return !s.cfg.DisableLocalFallback && ok && IsGitDir(dir)
}

// ProcessLocal runs a review on the server itself (the existing in-process
// executor). It is used by the scheduler as the local fallback.
func (s *Service) ProcessLocal(reviewID string) {
	s.process(reviewID)
}

// ProcessReactionLocal runs a reviewer reaction on the server itself.
func (s *Service) ProcessReactionLocal(reactionID string) {
	s.processReaction(reactionID)
}

// RequeueForOrphan returns a reviewing review owned by workerID to pending so
// it can be redispatched, and marks its in-flight run as failed. It is the
// recovery path when a worker goes offline mid-review.
func (s *Service) RequeueForOrphan(reviewID, workerID, reason string) error {
	unlock, err := s.syncLocks.Lock(context.Background(), reviewID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.store.RequeueReview(reviewID, workerID); err != nil {
		return err
	}
	runID := ""
	if run, err := s.store.CurrentRun(reviewID); err == nil {
		runID = run.ID
		run.Status = "failed"
		run.Error = reason
		run.CompletedAt = time.Now().UTC()
		if err := s.store.UpdateReviewRun(run); err != nil {
			logf("mark orphan run failed %s: %v", run.ID, err)
		}
	}
	reviewlog.Event{Name: "requeued", Component: "server", Kind: "review", ReviewID: reviewID, RunID: runID, WorkerID: workerID, Status: "pending", Error: reason}.Log()
	s.NotifyJob(workerproto.JobReview, reviewID)
	return nil
}

// RequeueOrphanReaction returns a running reaction owned by workerID to pending.
func (s *Service) RequeueOrphanReaction(reactionID, workerID string) error {
	if err := s.store.RequeueReaction(reactionID, workerID); err != nil {
		return err
	}
	if reac, err := s.store.GetReaction(reactionID); err == nil {
		reviewlog.Event{Name: "requeued", Component: "server", Kind: "reaction", ReviewID: reac.ReviewID, ReactionID: reactionID, WorkerID: workerID, Status: "pending"}.Log()
	}
	s.NotifyJob(workerproto.JobReaction, reactionID)
	return nil
}

// NotifyJob routes a newly available job to the scheduler (when one is
// installed); without a scheduler the job is queued for the local worker pool.
func (s *Service) NotifyJob(kind, id string) {
	s.enqueue(kind, id)
}

// EnqueueLocal queues a job for the in-process local worker pool, bypassing the
// scheduler sink. The scheduler uses it when no remote worker can serve a job,
// so local execution keeps the worker pool's bounded concurrency.
func (s *Service) EnqueueLocal(kind, id string) {
	s.enqueueLocal(kind, id)
}
