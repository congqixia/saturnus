package review

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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
	assigned, err := s.store.AssignReview(req.ID, workerID)
	if err != nil {
		return workerproto.JobPayload{}, err
	}
	if _, err := s.store.CreateReviewRun(store.ReviewRun{
		ReviewID:    assigned.ID,
		Status:      "running",
		RetryReason: assigned.RetryReason,
		Tool:        s.cfg.Tool,
		WorkerID:    workerID,
	}); err != nil {
		logf("create remote review run %s: %v", assigned.ID, err)
	}
	return s.JobPayload(assigned)
}

// StartRemoteReaction atomically assigns a pending reaction to a remote worker.
// The reaction resumes the owning review's tool session, so it must run on the
// same host that produced that session (the review's worker).
func (s *Service) StartRemoteReaction(reac store.ReviewReaction, req store.ReviewRequest, workerID string) (workerproto.JobPayload, error) {
	if workerID == "" {
		return workerproto.JobPayload{}, errors.New("empty worker id")
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
	resultAPIURL := ""
	if s.cfg.ResultAPIURL != "" {
		resultAPIURL = strings.ReplaceAll(s.cfg.ResultAPIURL, "{review_id}", req.ID)
	}
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
	if !s.cfg.Enabled {
		return errors.New("PR review is disabled")
	}
	req, err := s.store.GetReview(reviewID)
	if err != nil {
		return err
	}
	status := "succeeded"
	if !ok {
		status = "failed"
	}
	if headCommit == "" {
		headCommit = req.HeadCommit
	}
	if sessionID == "" {
		sessionID = req.SessionID
	}
	applied, err := s.store.UpdateReviewOutcomeByWorker(reviewID, workerID, status, headCommit, sessionID, result, errText, time.Now().UTC())
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("review %s is no longer owned by worker %s (stale completion dropped)", reviewID, workerID)
	}
	req.Status = status
	req.ResultText = result
	req.Error = errText
	req.HeadCommit = headCommit
	req.PrHeadCommit = headCommit
	req.SessionID = sessionID
	// Merge any structured result the tool POSTed directly to the server while
	// this run was executing (the POST handler writes verdict/summary onto the
	// review row, so the run record below can preserve them alongside the text).
	if fresh, err := s.store.GetReview(reviewID); err == nil {
		req.Verdict = fresh.Verdict
		req.Summary = fresh.Summary
	}
	if run, err := s.store.CurrentRun(reviewID); err == nil {
		run.Status = status
		run.HeadCommit = headCommit
		run.SessionID = sessionID
		run.ResultText = result
		run.Error = errText
		run.Verdict = req.Verdict
		run.Summary = req.Summary
		run.CompletedAt = time.Now().UTC()
		if err := s.store.UpdateReviewRun(run); err != nil {
			logf("update remote review run %s: %v", run.ID, err)
		}
	}
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
	return ok && IsGitDir(dir)
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
	if err := s.store.RequeueReview(reviewID, workerID); err != nil {
		return err
	}
	if run, err := s.store.CurrentRun(reviewID); err == nil {
		run.Status = "failed"
		run.Error = reason
		run.CompletedAt = time.Now().UTC()
		if err := s.store.UpdateReviewRun(run); err != nil {
			logf("mark orphan run failed %s: %v", run.ID, err)
		}
	}
	s.NotifyJob(workerproto.JobReview, reviewID)
	return nil
}

// RequeueOrphanReaction returns a running reaction owned by workerID to pending.
func (s *Service) RequeueOrphanReaction(reactionID, workerID string) error {
	if err := s.store.RequeueReaction(reactionID, workerID); err != nil {
		return err
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
