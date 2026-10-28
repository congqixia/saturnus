package main

import (
	"context"
	"log"
	"sort"
	"time"

	"saturnus/internal/review"
	"saturnus/internal/reviewlog"
	"saturnus/internal/store"
	"saturnus/internal/worker"
	"saturnus/internal/workerproto"
)

// jobKindReconcile triggers a scheduler sweep through the control queue.
const jobKindReconcile = "reconcile"

// scheduler dispatches review and reaction jobs to remote workers, falling back
// to the server's own local executor, and recovers orphans when workers go
// offline. It is the single consumer of the review Service's job sink.
type scheduler struct {
	svc               *review.Service
	st                *store.Store
	hub               *worker.Hub
	onlineWindow      time.Duration
	reconcileEvery    time.Duration
	onWorkerHeartbeat func()

	jobs chan jobRef
}

type jobRef struct {
	kind string
	id   string
}

func newScheduler(svc *review.Service, st *store.Store) *scheduler {
	return &scheduler{
		svc:            svc,
		st:             st,
		onlineWindow:   90 * time.Second,
		reconcileEvery: 30 * time.Second,
		jobs:           make(chan jobRef, 256),
	}
}

// SetHub wires the control-plane hub (must be called before Run).
func (s *scheduler) SetHub(h *worker.Hub) {
	s.hub = h
}

// Notify is the review Service job sink.
func (s *scheduler) Notify(kind, id string) {
	select {
	case s.jobs <- jobRef{kind: kind, id: id}:
	default:
		// Queue overflowed: the reconcile sweep will pick the job back up.
	}
}

// Run processes jobs until ctx is cancelled. It also runs a periodic sweep that
// recovers orphaned reviews/reactions and redispenses pending ones.
func (s *scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.reconcileEvery)
	defer ticker.Stop()
	// Initial sweep: recover orphans and dispatch everything pending.
	s.reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.jobs:
			s.handle(j)
		case <-ticker.C:
			s.reconcile()
		}
	}
}

func (s *scheduler) handle(j jobRef) {
	switch j.kind {
	case jobKindReconcile:
		s.reconcile()
	case workerproto.JobReaction:
		s.dispatchReaction(j.id)
	default:
		s.dispatchReview(j.id)
	}
}

// --- worker.Handler implementation ---

func (s *scheduler) OnConnect(workerID string) {
	log.Printf("scheduler: worker %s connected", workerID)
	s.Notify(jobKindReconcile, "")
}

func (s *scheduler) OnDisconnect(workerID string) {
	log.Printf("scheduler: worker %s disconnected", workerID)
}

func (s *scheduler) OnEvent(ctx context.Context, workerID string, frame workerproto.Frame) error {
	switch frame.Type {
	case workerproto.EvHello:
		var p workerproto.HelloPayload
		if err := frame.Decode(&p); err == nil {
			s.hub.SetInFlight(workerID, p.InFlight)
			s.reconcileWorker(workerID, p.InFlight)
		}
	case workerproto.EvHeartbeat:
		// Liveness is tracked by the hub on every frame; keep the store's
		// last_seen fresh so ListOnlineWorkers stays accurate.
		if err := s.st.Heartbeat(workerID); err != nil {
			log.Printf("scheduler: worker %s heartbeat: %v", workerID, err)
		}
		var p workerproto.HeartbeatPayload
		if err := frame.Decode(&p); err == nil && p.GitHub != nil {
			if changed, err := s.st.UpdateWorkerGitHub(workerID, p.GitHub); err != nil {
				log.Printf("scheduler: worker %s GitHub metadata: %v", workerID, err)
			} else if changed && s.onWorkerHeartbeat != nil {
				s.onWorkerHeartbeat()
			}
		}
	case workerproto.EvProgress:
		var p workerproto.ProgressPayload
		if err := frame.Decode(&p); err == nil && frame.ReviewID != "" {
			if err := s.svc.UpdateRemoteProgress(frame.ReviewID, p.Text); err != nil {
				log.Printf("scheduler: progress for %s: %v", frame.ReviewID, err)
			}
		}
	case workerproto.EvStarted:
		var p workerproto.StartedPayload
		if err := frame.Decode(&p); err != nil {
			return err
		}
		return s.recordStarted(workerID, frame.ReviewID, p)
	case workerproto.EvComplete:
		var p workerproto.CompletePayload
		if err := frame.Decode(&p); err == nil {
			if p.RunID != "" && p.ReactionID == "" {
				run, err := s.st.CurrentRun(frame.ReviewID)
				if err != nil || run.ID != p.RunID || run.WorkerID != workerID {
					reviewlog.Event{Name: "completion_rejected", Component: "server", Kind: "review", ReviewID: frame.ReviewID, RunID: p.RunID, WorkerID: workerID, Error: "completion does not match the current review run"}.Log()
					return nil
				}
			}
			s.hub.ClearInFlight(workerID, frame.ReviewID)
			var cErr error
			if p.ReactionID != "" {
				cErr = s.svc.CompleteRemoteReaction(p.ReactionID, workerID, p.OK, p.SessionID, p.ResultText, p.Error)
			} else {
				cErr = s.svc.CompleteRemote(frame.ReviewID, workerID, p.OK, p.HeadCommit, p.SessionID, p.ResultText, p.Error)
			}
			if cErr != nil {
				event := reviewlog.Event{Name: "completion_rejected", Component: "server", Kind: "review", ReviewID: frame.ReviewID, RunID: p.RunID, ReactionID: p.ReactionID, WorkerID: workerID, Error: cErr.Error()}
				if p.ReactionID != "" {
					event.Kind = "reaction"
				}
				event.Log()
			}
		}
	case workerproto.EvCancelAck:
		s.hub.ClearInFlight(workerID, frame.ReviewID)
	case workerproto.EvError:
		var p workerproto.ErrorPayload
		_ = frame.Decode(&p)
		s.hub.ClearInFlight(workerID, frame.ReviewID)
		if p.Code == "busy" {
			if err := s.svc.RequeueForOrphan(frame.ReviewID, workerID, "worker at capacity"); err != nil {
				log.Printf("scheduler: requeue busy %s: %v", frame.ReviewID, err)
			}
		}
	}
	return nil
}

func (s *scheduler) jobEvent(name, status, workerID string, p workerproto.JobPayload) reviewlog.Event {
	return reviewlog.Event{Name: name, Component: "server", Kind: p.Kind, ReviewID: p.ReviewID, RunID: p.RunID, ReactionID: p.ReactionID, WorkerID: workerID, Repo: p.Repo, PRNumber: p.PRNumber, Status: status}
}

func (s *scheduler) recordStarted(workerID, reviewID string, p workerproto.StartedPayload) error {
	event := reviewlog.Event{Name: "started", Component: "server", Kind: "review", ReviewID: reviewID, RunID: p.RunID, ReactionID: p.ReactionID, WorkerID: workerID, Status: "running"}
	req, err := s.st.GetReview(reviewID)
	if err != nil {
		return err
	}
	event.Repo, event.PRNumber = req.Repo, req.PRNumber
	valid := req.Status == "reviewing" && req.WorkerID == workerID
	if p.ReactionID != "" {
		event.Kind = "reaction"
		reac, err := s.st.GetReaction(p.ReactionID)
		if err != nil {
			return err
		}
		valid = reac.ReviewID == reviewID && reac.Status == "running" && reac.WorkerID == workerID
	} else if run, err := s.st.CurrentRun(reviewID); err == nil {
		valid = valid && (p.RunID == "" || p.RunID == run.ID)
		event.RunID = run.ID
	} else if p.RunID != "" {
		valid = false
	}
	if !valid {
		event.Name, event.Status, event.Error = "start_rejected", "", "job is no longer running on this worker"
	}
	event.Log()
	return nil
}

// --- dispatch ---

// RepoAvailable reports whether a repo can currently be served: the server
// itself has a checkout, or an online worker with a live connection serves it.
// It backs the submission-time whitelist check in distributed mode.
func (s *scheduler) RepoAvailable(repo string) bool {
	if s.svc.LocalAvailable(repo) {
		return true
	}
	now := time.Now()
	for _, w := range s.st.ListOnlineWorkers(s.onlineWindow) {
		caps, err := w.CapabilitiesDecoded()
		if err != nil {
			continue
		}
		if _, ok := caps.Repos[repo]; ok && s.workerHealthy(w.ID, now) {
			return true
		}
	}
	return false
}

func (s *scheduler) dispatchReview(id string) {
	req, err := s.st.GetReview(id)
	if err != nil || req.Status != "pending" {
		return
	}
	workerID := s.pickReviewWorker(req)
	if workerID == "" {
		// No online worker serves this repo: fall back to the local executor
		// when the server has the checkout, otherwise leave pending for the
		// next sweep (a worker may register later).
		if s.svc.LocalAvailable(req.Repo) {
			s.jobEvent("dispatched", "pending", "local", workerproto.JobPayload{Kind: workerproto.JobReview, ReviewID: req.ID, Repo: req.Repo, PRNumber: req.PRNumber}).Log()
			s.svc.EnqueueLocal(workerproto.JobReview, req.ID)
		}
		return
	}
	payload, err := s.svc.StartRemote(req, workerID)
	if err != nil {
		event := s.jobEvent("dispatch_failed", "", workerID, workerproto.JobPayload{Kind: workerproto.JobReview, ReviewID: req.ID, Repo: req.Repo, PRNumber: req.PRNumber})
		event.Error = err.Error()
		event.Log()
		return
	}
	s.hub.AddInFlight(workerID, req.ID)
	s.jobEvent("dispatch", "reviewing", workerID, payload).Log()
	if err := s.hub.Send(workerID, workerproto.NewFrame(workerproto.CmdClaim, req.ID, payload)); err != nil {
		s.hub.ClearInFlight(workerID, req.ID)
		event := s.jobEvent("dispatch_failed", "reviewing", workerID, payload)
		event.Error = err.Error()
		event.Log()
		if err := s.svc.RequeueForOrphan(req.ID, workerID, "claim send failed"); err != nil {
			log.Printf("scheduler: requeue after failed claim %s: %v", req.ID, err)
		}
		return
	}
	s.jobEvent("dispatched", "reviewing", workerID, payload).Log()
}

func (s *scheduler) dispatchReaction(id string) {
	reac, err := s.st.GetReaction(id)
	if err != nil || reac.Status != "pending" {
		return
	}
	req, err := s.st.GetReview(reac.ReviewID)
	if err != nil {
		return
	}
	// A reaction resumes the review's tool session, so it must run on the host
	// that produced that session: the worker that ran the review.
	if req.WorkerID != "" {
		if !s.hub.Online(req.WorkerID) {
			return // wait; the sweep retries when the worker returns
		}
		payload, err := s.svc.StartRemoteReaction(reac, req, req.WorkerID)
		if err != nil {
			event := s.jobEvent("dispatch_failed", "", req.WorkerID, workerproto.JobPayload{Kind: workerproto.JobReaction, ReviewID: req.ID, ReactionID: reac.ID, Repo: req.Repo, PRNumber: req.PRNumber})
			event.Error = err.Error()
			event.Log()
			return
		}
		s.hub.AddInFlight(req.WorkerID, req.ID)
		s.jobEvent("dispatch", "running", req.WorkerID, payload).Log()
		if err := s.hub.Send(req.WorkerID, workerproto.NewFrame(workerproto.CmdClaim, req.ID, payload)); err != nil {
			s.hub.ClearInFlight(req.WorkerID, req.ID)
			event := s.jobEvent("dispatch_failed", "running", req.WorkerID, payload)
			event.Error = err.Error()
			event.Log()
			if err := s.svc.RequeueOrphanReaction(reac.ID, req.WorkerID); err != nil {
				log.Printf("scheduler: requeue after failed reaction claim %s: %v", reac.ID, err)
			}
			return
		}
		s.jobEvent("dispatched", "running", req.WorkerID, payload).Log()
		return
	}
	// Local review: run the reaction locally too.
	if s.svc.LocalAvailable(req.Repo) {
		s.jobEvent("dispatched", "pending", "local", workerproto.JobPayload{Kind: workerproto.JobReaction, ReviewID: req.ID, ReactionID: reac.ID, Repo: req.Repo, PRNumber: req.PRNumber}).Log()
		s.svc.EnqueueLocal(workerproto.JobReaction, reac.ID)
	}
}

// pickReviewWorker chooses an online worker that serves the review's repo and
// has capacity, preferring the worker that ran the previous run (session
// affinity for retries) and otherwise the least loaded.
func (s *scheduler) pickReviewWorker(req store.ReviewRequest) string {
	workers := s.st.ListOnlineWorkers(s.onlineWindow)
	type candidate struct {
		id   string
		load int
	}
	var cands []candidate
	for _, w := range workers {
		caps, err := w.CapabilitiesDecoded()
		if err != nil {
			continue
		}
		if _, ok := caps.Repos[req.Repo]; !ok {
			continue
		}
		max := caps.MaxConcurrent
		if max <= 0 {
			max = 1
		}
		if s.hub.InFlightCount(w.ID) >= max {
			continue
		}
		cands = append(cands, candidate{id: w.ID, load: s.hub.InFlightCount(w.ID)})
	}
	if len(cands) == 0 {
		return ""
	}
	// Session affinity: resume the previous run on the same host when possible.
	if affinity := s.st.LastRunWorkerID(req.ID); affinity != "" {
		for _, c := range cands {
			if c.id == affinity {
				return c.id
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].load != cands[j].load {
			return cands[i].load < cands[j].load
		}
		return cands[i].id < cands[j].id
	})
	return cands[0].id
}

// --- recovery ---

// reconcile recovers reviews/reactions whose workers went offline beyond the
// lease window and redispenses pending work.
func (s *scheduler) reconcile() {
	now := time.Now()
	for _, req := range s.st.ListReviewing() {
		if req.WorkerID == "" {
			continue
		}
		if s.workerHealthy(req.WorkerID, now) {
			continue
		}
		if err := s.svc.RequeueForOrphan(req.ID, req.WorkerID, "worker offline"); err != nil {
			// Already requeued or reassigned elsewhere; nothing to do.
		}
	}
	for _, reac := range s.st.ListRunningReactions() {
		if reac.WorkerID == "" {
			continue
		}
		if s.workerHealthy(reac.WorkerID, now) {
			continue
		}
		if err := s.svc.RequeueOrphanReaction(reac.ID, reac.WorkerID); err != nil {
			// Already requeued or reassigned elsewhere; nothing to do.
		}
	}
	for _, req := range s.st.ListReviews("pending", false) {
		s.dispatchReview(req.ID)
	}
	for _, reac := range s.st.ListPendingReactions() {
		s.dispatchReaction(reac.ID)
	}
}

// reconcileWorker cross-checks a (re)connecting worker's reported in-flight
// jobs against the reviews the server believes it owns. Reviews the worker no
// longer runs are requeued so they can be redispensed.
func (s *scheduler) reconcileWorker(workerID string, inFlight []string) {
	inflightSet := map[string]bool{}
	for _, id := range inFlight {
		inflightSet[id] = true
	}
	for _, req := range s.st.ListReviewing() {
		if req.WorkerID != workerID || inflightSet[req.ID] {
			continue
		}
		if err := s.svc.RequeueForOrphan(req.ID, workerID, "worker no longer running job after reconnect"); err != nil {
			// Already requeued or reassigned elsewhere; nothing to do.
		}
	}
	for _, reac := range s.st.ListRunningReactions() {
		if reac.WorkerID != workerID || inflightSet[reac.ReviewID] {
			continue
		}
		if err := s.svc.RequeueOrphanReaction(reac.ID, workerID); err != nil {
			// Already requeued or reassigned elsewhere; nothing to do.
		}
	}
}

// workerHealthy reports whether a worker has a live connection that has been
// heard from recently enough to still own in-flight jobs.
func (s *scheduler) workerHealthy(workerID string, now time.Time) bool {
	if !s.hub.Online(workerID) {
		return false
	}
	last := s.hub.LastSeen(workerID)
	return !last.IsZero() && now.Sub(last) < s.onlineWindow
}
